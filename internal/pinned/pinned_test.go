package pinned_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/pinned"
)

// TestCommand runs k3d through scripts/pinned with a PATH that holds only
// what the script needs and a fake mise, so the host's mise and tools do not
// affect the result. Every row but the first is a refusal.
func TestCommand(t *testing.T) {
	t.Parallel()

	root, bin, miseDir, install := fakeMise(t)

	tests := []struct {
		name, lacks, source, installed, installPath string
		noMise                                      bool
		want                                        string
	}{
		{name: "pinned", want: "pinned k3d cluster list"},
		{name: "no mise", noMise: true, want: "pinned: mise is not installed. See README.md."},
		{
			name:  "mise lacks k3d",
			lacks: "k3d",
			// mise's own error comes first, then the script's.
			want: "mise ERROR k3d is not a mise bin\npinned: mise does not provide k3d. See mise's message above.",
		},
		{
			name:   "global k3d",
			source: "/home/user/.config/mise/config.toml",
			want:   "pinned: mise resolves k3d from /home/user/.config/mise/config.toml, not " + root + "/mise.toml",
		},
		{name: "k3d not installed", installed: "false", want: "pinned: k3d 1.0.0 is not installed. Run mise install."},
		{
			name:        "k3d outside its install path",
			installPath: install + "-other",
			want:        "pinned: mise resolves k3d to " + install + "/k3d, outside " + install + "-other",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := pinned.Command(t.Context(), "k3d", "cluster", "list")
			if err != nil {
				t.Fatal(err)
			}
			path := bin + ":" + miseDir
			if tc.noMise {
				path = bin
			}
			cmd.Env = []string{
				"PATH=" + path, "MISE_LACKS=" + tc.lacks, "MISE_SOURCE=" + tc.source,
				"MISE_INSTALLED=" + tc.installed, "MISE_INSTALL=" + tc.installPath,
			}
			out, err := pinned.Run(cmd, nil)
			if tc.name == "pinned" {
				if err != nil || strings.TrimSpace(string(out)) != tc.want {
					t.Fatalf("Run() = %q, %v, want %q", out, err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run() = %q, %v, want a refusal naming %q", out, err, tc.want)
			}
		})
	}
}

// fakeMise returns the repository root with symlinks resolved, a directory
// that holds only the tools scripts/pinned needs, a directory that holds the
// fake mise, and the install path the fake mise names for k3d.
//
// The fake mise answers only for the repository root, and resolves k3d to a
// fake that echoes its arguments. It refuses k3d when $MISE_LACKS is k3d.
// $MISE_SOURCE, $MISE_INSTALLED and $MISE_INSTALL override the source,
// installed flag and install path it reports for k3d.
func fakeMise(t *testing.T) (string, string, string, string) {
	t.Helper()
	root, err := pinned.Root()
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		t.Fatal(err)
	}
	bin, miseDir, install := t.TempDir(), t.TempDir(), t.TempDir()
	for _, tool := range []string{"basename", "bash", "dirname"} {
		if err = os.Symlink(lookTool(t, tool), filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	mise := `#!/usr/bin/env bash
tool=${!#}
if [[ "$1 $2" != "-C ` + root + `" || $tool != k3d || $tool == "$MISE_LACKS" ]]; then
	echo "mise ERROR $tool is not a mise bin" >&2
	exit 1
fi
source=${MISE_SOURCE:-` + root + `/mise.toml} installed=${MISE_INSTALLED:-true}
case "${*:3:$#-3}" in
which) echo "` + install + `/k3d" ;;
"ls --current --json")
	printf '[\n  {\n    "version": "1.0.0",\n    "requested_version": "1.0.0",\n'
	printf '    "install_path": "%s",\n' "${MISE_INSTALL:-` + install + `}"
	printf '    "source": {\n      "type": "mise.toml",\n      "path": "%s"\n    },\n' "$source"
	printf '    "installed": %s,\n    "active": true\n  }\n]\n' "$installed"
	;;
*) exit 1 ;;
esac
`
	for path, body := range map[string]string{
		filepath.Join(miseDir, "mise"): mise,
		filepath.Join(install, "k3d"):  "#!/usr/bin/env bash\necho \"pinned k3d $*\"\n",
	} {
		//nolint:gosec // G306: the fakes must be executable.
		if err = os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	return root, bin, miseDir, install
}

// lookTool finds tool on PATH but skips mise's shims, which need mise's own
// environment and fail under the stripped PATH the test runs with. It falls
// back to the version mise resolves for this directory.
func lookTool(t *testing.T, tool string) string {
	t.Helper()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		path := filepath.Join(dir, tool)
		info, err := os.Stat(path) //nolint:gosec // G703: the path is a PATH entry joined with a fixed tool name.
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
