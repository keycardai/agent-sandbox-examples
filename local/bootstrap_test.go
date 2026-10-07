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

// localdev is the Keycard stack's cluster, which the lane never touches.
const localdev = "localdev"

// TestBootstrapDryRun runs bootstrap.sh --dry-run, which reads its settings
// file as data and validates its settings before it looks for Docker or any
// other tool.
func TestBootstrapDryRun(t *testing.T) {
	t.Parallel()

	const (
		host    = "https://asp.localdev.keycard.sh"
		valid   = host + ":8443/oidc"
		noHost  = "the issuer must name a host"
		badName = "the cluster name must be lowercase letters, digits and dashes (got "
		refused = "line 1 is not KEY=value for a key local.env.example lists"
		// noFlag as a row's cluster passes no --cluster flag. An empty cluster
		// passes --cluster asp.
		noFlag = "-"
	)
	// The PATH holds only what the script runs before its tool check, plus
	// touch, so a settings file that runs "touch MARKER" leaves a marker.
	bin := linkTools(t, "basename", "bash", "dirname", "touch")
	example, err := os.ReadFile("local.env.example")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, cluster, issuer, file string
		wantErr                     bool
		want                        []string
		env                         string
	}{
		{"http", "", "http://asp.localdev.keycard.sh", "", true, []string{"the issuer must be https"}, ""},
		{"no host", "", "https:///path", "", true, []string{noHost}, ""},
		{"space in host", "", "https://asp .localdev.keycard.sh", "", true, []string{noHost}, ""},
		{"userinfo", "", "https://user@asp.localdev.keycard.sh", "", true, []string{noHost}, ""},
		{"unicode host", "", "https://asp.localdév.keycard.sh", "", true, []string{noHost}, ""},
		{"port out of range", "", host + ":65536", "", true, []string{"the issuer port must be 1-65535"}, ""},
		{"space in path", "", host + "/a b", "", true, []string{"the issuer must have an optional port and a path"}, ""},
		{"query", "", valid + "?a=b", "", true, []string{"the issuer must have no query or fragment"}, ""},
		{"fragment", "", valid + "#a", "", true, []string{"the issuer must have no query or fragment"}, ""},
		{"trailing slash", "", valid + "/", "", true, []string{"the issuer must have no trailing slash"}, ""},
		{"localdev cluster", localdev, valid, "", true, []string{"'localdev' is the Keycard localdev stack"}, ""},
		{"option as cluster", "--verbose", valid, "", true, []string{badName + "'--verbose')"}, ""},
		{
			"quoted in file",
			noFlag,
			valid,
			"KEYCARD_LOCAL_CLUSTER=\"quoted\"\n",
			true,
			[]string{badName + `'"quoted"')`},
			"",
		},
		{"cluster over 32 characters", strings.Repeat("a", 33), valid, "", true, []string{badName}, ""},
		{"file assigns a script variable", "", valid, "cluster=other\n", true, []string{refused}, ""},
		{"file runs a command", "", valid, "touch MARKER\n", true, []string{refused}, ""},
		{"example file", noFlag, valid, string(example), false, []string{
			"k3d\ncluster\ncreate\nkeycard-sandbox-local\n",
		}, ""},
		{"valid", "", valid, "# comment\n\nKEYCARD_LOCAL_CLUSTER=other\n", false, []string{
			"k3d\ncluster\ncreate\nasp\n--image\nrancher/k3s:v1.36.4-k3s1\n",
			"--k3s-arg\n--kube-apiserver-arg=service-account-issuer=" + valid + "@server:*\n",
			"--k3s-arg\n--kube-apiserver-arg=service-account-jwks-uri=" + valid + "/openid/v1/jwks@server:*\n",
			"--k3s-arg\n--disable=traefik@server:*\n--k3s-arg\n--disable=servicelb@server:*\n",
		}, ""},
		{"environment beats file", noFlag, valid, "KEYCARD_LOCAL_CLUSTER=from-file\n", false, []string{
			"k3d\ncluster\ncreate\nfrom-env\n",
		}, "KEYCARD_LOCAL_CLUSTER=from-env"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cluster := cmp.Or(tc.cluster, "asp")
			if cluster == noFlag {
				cluster = ""
			}
			out, failed := dryRun(t, bin, cluster, tc.issuer, tc.file, tc.env)
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

// TestBootstrapNeedsMise runs bootstrap.sh past validation with no mise on
// PATH, so a bootstrap.sh that took its tools from PATH instead of mise fails.
func TestBootstrapNeedsMise(t *testing.T) {
	t.Parallel()

	bin := linkTools(t, "basename", "bash", "dirname")
	//nolint:gosec // The fake docker must be executable.
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/usr/bin/env bash\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(t.TempDir(), "local.env")
	if err := os.WriteFile(env, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // The arguments are fixed inputs to a script in this repository.
	cmd := exec.CommandContext(t.Context(), filepath.Join(bin, "bash"), "bootstrap.sh",
		"--env", env, "--cluster", "asp", "--issuer", "https://asp.localdev.keycard.sh")
	cmd.Env = []string{"PATH=" + bin}

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("bootstrap.sh succeeded with no mise on PATH\n%s", out)
	}
	if want := "bootstrap.sh: mise is not installed. See README.md."; !strings.Contains(string(out), want) {
		t.Errorf("output lacks %q\n%s", want, out)
	}
}

// dryRun runs bootstrap.sh --dry-run with only bin on its PATH and a settings
// file holding file. An empty cluster passes no --cluster flag, and env is a
// KEY=value added to the run's environment when it is not empty. MARKER in
// file stands for a path the run must never create. A settings file per run
// keeps the host's local.env out of it. It reports whether the run failed and
// logs why.
func dryRun(t *testing.T, bin, cluster, issuer, file, env string) ([]byte, bool) {
	t.Helper()

	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	settings := filepath.Join(dir, "local.env")
	if err := os.WriteFile(settings, []byte(strings.ReplaceAll(file, "MARKER", marker)), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"bootstrap.sh", "--dry-run", "--env", settings, "--issuer", issuer}
	if cluster != "" {
		args = append(args, "--cluster", cluster)
	}
	//nolint:gosec // The arguments are a test table's inputs to a script in this repository.
	cmd := exec.CommandContext(t.Context(), filepath.Join(bin, "bash"), args...)
	cmd.Env = []string{"PATH=" + bin}
	if env != "" {
		cmd.Env = append(cmd.Env, env)
	}

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
		path := filepath.Join(dir, tool)
		//nolint:gosec // G703: the path is a PATH entry joined with a fixed tool name.
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			continue
		}
		// A mise shim is a link to the mise binary itself.
		if target, err := filepath.EvalSymlinks(path); err == nil && filepath.Base(target) == "mise" {
			continue
		}
		return path
	}
	//nolint:gosec // G204: mise is fixed and the tool name comes from this test.
	out, err := exec.CommandContext(t.Context(), "mise", "which", tool).Output()
	if err != nil {
		t.Fatalf("%s is not on PATH outside mise's shims, and mise does not resolve it: %v", tool, err)
	}

	return strings.TrimSpace(string(out))
}
