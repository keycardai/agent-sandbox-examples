package local_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeK3d prints $K3D_LIST for "cluster list -o json", or fails when it is
// FAIL, and appends every other call to $K3D_CALLS.
const fakeK3d = `#!/usr/bin/env bash
if [[ $* == "cluster list -o json" ]]; then
	[[ $K3D_LIST == FAIL ]] && exit 1
	printf '%s\n' "$K3D_LIST"
	exit 0
fi
printf '%s\n' "$*" >>"$K3D_CALLS"
`

// TestDown runs down.sh against a fake k3d. The empty list is the "[]" k3d
// v5.9.0 prints when it lists no cluster.
func TestDown(t *testing.T) {
	t.Parallel()

	bin := linkTools(t, "basename", "bash", "dirname", "jq")
	for name, body := range map[string]string{"docker": "#!/usr/bin/env bash\n", "k3d": fakeK3d} {
		//nolint:gosec // The fakes must be executable.
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name, list  string
		wantErr     bool
		wantOut     string
		wantDeleted bool
	}{
		{"cannot list", "FAIL", true, "down.sh: cannot list k3d clusters. Is Docker running?", false},
		{"absent", `[{"name":"asp-other"},{"name":"as"}]`, false, "down.sh: there is no k3d cluster named asp", false},
		{"empty list", "[]", false, "down.sh: there is no k3d cluster named asp", false},
		{"present", `[{"name":"other"},{"name":"asp"}]`, false, "down.sh: deleted asp", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			env := filepath.Join(dir, "local.env")
			if err := os.WriteFile(env, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			//nolint:gosec // The arguments are fixed inputs to a script in this repository.
			cmd := exec.CommandContext(t.Context(), filepath.Join(bin, "bash"), "down.sh",
				"--env", env, "--cluster", "asp", "--yes")
			cmd.Env = []string{"PATH=" + bin, "K3D_LIST=" + tc.list, "K3D_CALLS=" + calls}

			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %t\n%s", err, tc.wantErr, out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("output lacks %q\n%s", tc.wantOut, out)
			}
			// A missing file means k3d had no other call.
			got, _ := os.ReadFile(calls) //nolint:gosec // The path is the test's temp file.
			want := ""
			if tc.wantDeleted {
				want = "cluster delete asp\n"
			}
			if string(got) != want {
				t.Errorf("k3d calls = %q, want %q", got, want)
			}
		})
	}
}
