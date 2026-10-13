package pinned_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
)

// TestCommand puts a fake mise alone on PATH, so the host's mise and tools do
// not affect the result. The fake answers only for the repository root, with
// the row's `which` and `ls` output. Every row but "pinned" expects an error.
// The test sets PATH, so it cannot run in parallel.
func TestCommand(t *testing.T) {
	root, err := pinned.Root()
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		t.Fatal(err)
	}
	install := t.TempDir()
	writeExecutable(t, filepath.Join(install, "k3d"), "#!/bin/sh\necho \"pinned k3d $*\"\n")

	entry := func(source string, installed bool, installPath string) string {
		return fmt.Sprintf(`{"version": "1.0.0", "install_path": %q, `+
			`"source": {"type": "mise.toml", "path": %q}, "installed": %t, "active": true}`,
			installPath, source, installed)
	}
	const global = "/home/user/.config/mise/config.toml"
	resolves := "echo " + install + "/k3d"
	pinnedEntry := entry(root+"/mise.toml", true, install)

	tests := []struct {
		name   string
		noMise bool
		which  string // the fake's shell command for `mise which k3d`
		ls     string // the fake's output for `mise ls --current --json k3d`
		want   string // k3d's output, or the error
	}{
		{name: "pinned", which: resolves, ls: "[" + pinnedEntry + "]", want: "pinned k3d cluster list\n"},
		{name: "no mise", noMise: true, want: "pinned: mise is not installed. See README.md"},
		{
			name:  "mise lacks k3d",
			which: "echo 'mise ERROR k3d is not a mise bin' >&2; exit 1",
			want: "pinned: mise does not provide k3d: mise -C " + root +
				" which k3d: exit status 1: mise ERROR k3d is not a mise bin",
		},
		{
			name:  "two entries",
			which: resolves,
			ls:    "[" + pinnedEntry + ", " + pinnedEntry + "]",
			want:  "pinned: mise ls --current --json k3d printed something other than one entry",
		},
		{
			name:  "global k3d",
			which: resolves,
			ls:    "[" + entry(global, true, install) + "]",
			want:  "pinned: mise resolves k3d from " + global + ", not " + root + "/mise.toml",
		},
		{
			name:  "k3d not installed",
			which: resolves,
			ls:    "[" + entry(root+"/mise.toml", false, install) + "]",
			want:  "pinned: k3d 1.0.0 is not installed. Run mise install",
		},
		{
			name:  "k3d outside its install path",
			which: resolves,
			ls:    "[" + entry(root+"/mise.toml", true, install+"-other") + "]",
			want:  "pinned: mise resolves k3d to " + install + "/k3d, outside " + install + "-other",
		},
		{
			name:  "no install path",
			which: resolves,
			ls:    "[" + entry(root+"/mise.toml", true, "") + "]",
			want:  "pinned: mise resolves k3d to " + install + "/k3d, outside ",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if !tc.noMise {
				writeExecutable(t, filepath.Join(bin, "mise"), `#!/bin/sh
if [ "$*" = "-C `+root+` which k3d" ]; then
	`+tc.which+`
	exit
fi
if [ "$*" = "-C `+root+` ls --current --json k3d" ]; then
	echo '`+tc.ls+`'
	exit
fi
echo "mise ERROR unexpected arguments $*" >&2
exit 1
`)
			}
			t.Setenv("PATH", bin)

			out, err := pinned.Output(t.Context(), nil, "k3d", "cluster", "list")
			if tc.name == "pinned" {
				if err != nil || string(out) != tc.want {
					t.Fatalf("Output() = %q, %v, want %q", out, err, tc.want)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("Output() = %q, %v, want the refusal %q", out, err, tc.want)
			}
		})
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	//nolint:gosec // G306: the fakes must be executable.
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}
