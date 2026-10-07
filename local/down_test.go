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

// TestDown runs down.sh from outside the repository. The PATH holds a decoy
// k3d that records every call, and ends with a fake mise that answers only
// against the repository root. The mise resolves k3d to fakeK3d and jq to the
// real jq, except the tool named in $MISE_LACKS. Its entry for k3d takes its
// source, installed flag and install path from $MISE_SOURCE, $MISE_INSTALLED
// and $MISE_INSTALL when they are set. The empty list is the "[]" k3d v5.9.0
// prints when it lists no cluster.
func TestDown(t *testing.T) {
	t.Parallel()

	// present lists the cluster the run names, so a run that passes the tool
	// checks deletes it.
	const present = `[{"name":"asp"}]`

	root := repoRoot(t)
	bin := linkTools(t, "basename", "bash", "dirname")
	pinned, miseDir := t.TempDir(), t.TempDir()
	realJq := lookTool(t, "jq")
	mise := `#!/usr/bin/env bash
tool=${!#}
if [[ "$1 $2" != "-C ` + root + `" || $tool == "$MISE_LACKS" ]]; then
	echo "mise ERROR $tool is not a mise bin" >&2
	exit 1
fi
source=` + root + `/mise.toml installed=true
case $tool in
k3d)
	path=` + pinned + `/k3d install=${MISE_INSTALL:-` + pinned + `}
	source=${MISE_SOURCE:-$source} installed=${MISE_INSTALLED:-true}
	;;
jq) path=` + realJq + ` install=` + filepath.Dir(realJq) + ` ;;
*) exit 1 ;;
esac
case "${*:3:$#-3}" in
which) echo "$path" ;;
"ls --current --json")
	printf '[\n  {\n    "version": "1.0.0",\n    "requested_version": "1.0.0",\n    "install_path": "%s",\n' "$install"
	printf '    "source": {\n      "type": "mise.toml",\n      "path": "%s"\n    },\n' "$source"
	printf '    "installed": %s,\n    "active": true\n  }\n]\n' "$installed"
	;;
*) exit 1 ;;
esac
`
	for path, body := range map[string]string{
		filepath.Join(bin, "docker"):   "#!/usr/bin/env bash\n",
		filepath.Join(bin, "k3d"):      "#!/usr/bin/env bash\nprintf 'decoy %s\\n' \"$*\" >>\"$K3D_CALLS\"\n",
		filepath.Join(miseDir, "mise"): mise,
		filepath.Join(pinned, "k3d"):   fakeK3d,
	} {
		//nolint:gosec // The fakes must be executable.
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name, list, lacks          string
		source, installed, install string
		noMise, wantErr            bool
		wantOut, wantCalls         string
	}{
		{
			name:    "cannot list",
			list:    "FAIL",
			wantErr: true,
			wantOut: "down.sh: cannot list k3d clusters. Is Docker running?",
		},
		{
			name:    "absent",
			list:    `[{"name":"asp-other"},{"name":"as"}]`,
			wantOut: "down.sh: there is no k3d cluster named asp",
		},
		{name: "empty list", list: "[]", wantOut: "down.sh: there is no k3d cluster named asp"},
		{
			name:    "entry without a name",
			list:    `[{}]`,
			wantErr: true,
			wantOut: "down.sh: k3d cluster list printed something other than a cluster list",
		},
		{
			name:      "present",
			list:      `[{"name":"other"},{"name":"asp"}]`,
			wantOut:   "down.sh: deleted asp",
			wantCalls: "cluster delete asp\n",
		},
		{
			name:    "no mise",
			list:    "[]",
			noMise:  true,
			wantErr: true,
			wantOut: "down.sh: mise is not installed. See README.md.",
		},
		{
			name:    "mise lacks k3d",
			list:    "[]",
			lacks:   "k3d",
			wantErr: true,
			// mise's own error comes first, then the script's.
			wantOut: "mise ERROR k3d is not a mise bin\ndown.sh: mise does not provide k3d. See mise's message above.",
		},
		{
			name:    "global k3d",
			list:    present,
			source:  "/home/user/.config/mise/config.toml",
			wantErr: true,
			wantOut: "down.sh: mise resolves k3d from /home/user/.config/mise/config.toml, not " + root + "/mise.toml",
		},
		{
			name:      "k3d not installed",
			list:      present,
			installed: "false",
			wantErr:   true,
			wantOut:   "down.sh: k3d 1.0.0 is not installed. Run mise install.",
		},
		{
			name:    "k3d outside its install path",
			list:    present,
			install: pinned + "-other",
			wantErr: true,
			wantOut: "down.sh: mise resolves k3d to " + pinned + "/k3d, outside " + pinned + "-other",
		},
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
			path := bin + ":" + miseDir
			if tc.noMise {
				path = bin
			}
			//nolint:gosec // The arguments are fixed inputs to a script in this repository.
			cmd := exec.CommandContext(t.Context(), filepath.Join(bin, "bash"), filepath.Join(root, "local", "down.sh"),
				"--env", env, "--cluster", "asp", "--yes")
			cmd.Dir = dir
			cmd.Env = []string{
				"PATH=" + path, "K3D_LIST=" + tc.list, "K3D_CALLS=" + calls, "MISE_LACKS=" + tc.lacks,
				"MISE_SOURCE=" + tc.source, "MISE_INSTALLED=" + tc.installed, "MISE_INSTALL=" + tc.install,
			}

			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %t\n%s", err, tc.wantErr, out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("output lacks %q\n%s", tc.wantOut, out)
			}
			// A missing file means no k3d had a call other than the listing.
			got, _ := os.ReadFile(calls) //nolint:gosec // The path is the test's temp file.
			if string(got) != tc.wantCalls {
				t.Errorf("k3d calls = %q, want %q", got, tc.wantCalls)
			}
		})
	}
}

// repoRoot returns the repository root with symlinks resolved, as pwd -P
// prints it.
func repoRoot(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs("..")
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		t.Fatal(err)
	}

	return root
}
