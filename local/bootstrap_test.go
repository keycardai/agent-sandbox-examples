package local_test

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBootstrapDryRun runs bootstrap.sh --dry-run, which reads its settings
// file as data and validates its settings before it looks for Docker or any
// other tool.
func TestBootstrapDryRun(t *testing.T) {
	t.Parallel()

	const (
		host    = "https://asp.localdev.keycard.sh"
		valid   = host + ":8443/oidc"
		noHost  = "the issuer must name a host"
		refused = "line 1 is not KEYCARD_LOCAL_CLUSTER=value or KEYCARD_LOCAL_ISSUER=value"
	)

	// The PATH holds only what the script runs before its tool check, plus
	// touch, so a settings file that runs "touch MARKER" leaves a marker.
	bin := linkTools(t, "basename", "bash", "dirname", "touch")

	tests := []struct {
		name, cluster, issuer, file string
		wantErr                     bool
		want                        []string
	}{
		{"http", "", "http://asp.localdev.keycard.sh", "", true, []string{"the issuer must be https"}},
		{"no host", "", "https:///path", "", true, []string{noHost}},
		{"space in host", "", "https://asp .localdev.keycard.sh", "", true, []string{noHost}},
		{"userinfo", "", "https://user@asp.localdev.keycard.sh", "", true, []string{noHost}},
		{"unicode host", "", "https://asp.localdév.keycard.sh", "", true, []string{noHost}},
		{"port out of range", "", host + ":65536", "", true, []string{"the issuer port must be 1-65535"}},
		{"space in path", "", host + "/a b", "", true, []string{"the issuer must have an optional port and a path"}},
		{"query", "", valid + "?a=b", "", true, []string{"the issuer must have no query or fragment"}},
		{"fragment", "", valid + "#a", "", true, []string{"the issuer must have no query or fragment"}},
		{"trailing slash", "", valid + "/", "", true, []string{"the issuer must have no trailing slash"}},
		{"localdev cluster", "localdev", valid, "", true, []string{"'localdev' is the Keycard localdev stack"}},
		{"file assigns a script variable", "", valid, "cluster=other\n", true, []string{refused}},
		{"file runs a command", "", valid, "touch MARKER\n", true, []string{refused}},
		{"valid", "", valid, "# comment\n\nKEYCARD_LOCAL_CLUSTER=other\n", false, []string{
			"k3d\ncluster\ncreate\nasp\n--image\nrancher/k3s:v1.36.4-k3s1\n",
			"--k3s-arg\n--kube-apiserver-arg=service-account-issuer=" + valid + "@server:*\n",
			"--k3s-arg\n--kube-apiserver-arg=service-account-jwks-uri=" + valid + "/openid/v1/jwks@server:*\n",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out, failed := dryRun(t, bin, cmp.Or(tc.cluster, "asp"), tc.issuer, tc.file)
			if failed != tc.wantErr {
				t.Fatalf("failed = %t, want %t\n%s", failed, tc.wantErr, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(out), want) {
					t.Errorf("output lacks %q\n%s", want, out)
				}
			}
		})
	}
}

// dryRun runs bootstrap.sh --dry-run with only bin on its PATH and a settings
// file holding file. MARKER in file stands for a path the run must never
// create. A settings file per run keeps the host's local.env out of it.
// It reports whether the run failed and logs why.
func dryRun(t *testing.T, bin, cluster, issuer, file string) ([]byte, bool) {
	t.Helper()

	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	env := filepath.Join(dir, "local.env")
	if err := os.WriteFile(env, []byte(strings.ReplaceAll(file, "MARKER", marker)), 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // The arguments are a test table's inputs to a script in this repository.
	cmd := exec.CommandContext(t.Context(), filepath.Join(bin, "bash"), "bootstrap.sh", "--dry-run",
		"--env", env, "--cluster", cluster, "--issuer", issuer)
	cmd.Env = []string{"PATH=" + bin}

	out, err := cmd.CombinedOutput()
	if _, statErr := os.Stat(marker); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the settings file ran a command: stat %s = %v", marker, statErr)
	}

	if err != nil {
		t.Logf("bootstrap.sh: %v", err)
	}

	return out, err != nil
}

// linkTools returns a directory that holds a symlink to each named tool and
// nothing else.
func linkTools(t *testing.T, tools ...string) string {
	t.Helper()

	dir := t.TempDir()
	for _, tool := range tools {
		if err := os.Symlink(lookTool(t, tool), filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// lookTool finds tool on PATH but skips mise's shims, which need mise's own
// environment and fail under the stripped PATH these tests run with. It
// falls back to the version mise resolves for this directory.
func lookTool(t *testing.T, tool string) string {
	t.Helper()

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Base(dir) == "shims" && filepath.Base(filepath.Dir(dir)) == "mise" {
			continue
		}
		path := filepath.Join(dir, tool)
		//nolint:gosec // G703: the path is a PATH entry joined with a fixed tool name.
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return path
		}
	}
	//nolint:gosec // G204: mise is fixed and the tool name comes from this test.
	out, err := exec.CommandContext(t.Context(), "mise", "which", tool).Output()
	if err != nil {
		t.Fatalf("%s is not on PATH outside mise's shims, and mise does not resolve it: %v", tool, err)
	}

	return strings.TrimSpace(string(out))
}
