// Package pinned runs the external tools that the repository's mise.toml
// pins. It refuses any other version of a tool.
package pinned

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

var (
	errRepository = errors.New("repository")
	errNotPinned  = errors.New("pinned")
)

// Command returns a command that runs tool with args. mise resolves tool
// against the repository's mise.toml, whatever the working directory.
// Command fails unless mise reports one entry for tool whose source is that
// mise.toml, whose version is installed, and whose install path contains the
// resolved path.
func Command(ctx context.Context, tool string, args ...string) (*exec.Cmd, error) {
	path, err := resolve(ctx, tool)
	if err != nil {
		return nil, err
	}
	//nolint:gosec // G204: mise resolves the tool and the caller chooses its arguments.
	return exec.CommandContext(ctx, path, args...), nil
}

// Output runs tool with args and stdin and returns its standard output.
func Output(ctx context.Context, stdin []byte, tool string, args ...string) ([]byte, error) {
	cmd, err := Command(ctx, tool, args...)
	if err != nil {
		return nil, err
	}
	return Run(cmd, stdin)
}

// Run runs cmd with stdin and returns its standard output. On failure, the
// error has the program's base name, its arguments, and its standard error.
func Run(cmd *exec.Cmd, stdin []byte) ([]byte, error) {
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		name := slices.Concat([]string{filepath.Base(cmd.Path)}, cmd.Args[1:])
		return nil, fmt.Errorf("%s: %w: %s", strings.Join(name, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Root returns the root of the repository that holds this package, from the
// source path the build records for this file. The path exists only where
// the source tree is, as in the tests and under `go run`.
func Root() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return "", fmt.Errorf("%w: cannot locate the repository from %q", errRepository, file)
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file))), nil
}

// miseEntry holds the fields of one `mise ls --json` entry that check reads.
// The fields follow JSONToolVersion in mise's src/cli/ls.rs.
type miseEntry struct {
	Version     string `json:"version"`
	InstallPath string `json:"install_path"`
	Source      struct {
		Path string `json:"path"`
	} `json:"source"`
	Installed bool `json:"installed"`
}

// resolve returns the path to tool if mise reports one entry for it and the
// entry passes check.
func resolve(ctx context.Context, tool string) (string, error) {
	root, err := Root()
	if err != nil {
		return "", err
	}
	// mise reports the source path with symlinks resolved.
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return "", fmt.Errorf("%w: %w", errRepository, err)
	}
	mise, err := exec.LookPath("mise")
	if err != nil {
		return "", fmt.Errorf("%w: mise is not installed. See README.md", errNotPinned)
	}

	//nolint:gosec // G204: mise comes from PATH and the caller chooses the tool.
	out, err := Run(exec.CommandContext(ctx, mise, "-C", root, "which", tool), nil)
	if err != nil {
		return "", fmt.Errorf("%w: mise does not provide %s: %w", errNotPinned, tool, err)
	}
	path := strings.TrimSuffix(string(out), "\n")

	//nolint:gosec // G204: mise comes from PATH and the caller chooses the tool.
	out, err = Run(exec.CommandContext(ctx, mise, "-C", root, "ls", "--current", "--json", tool), nil)
	var entries []miseEntry
	if err == nil {
		err = json.Unmarshal(out, &entries)
	}
	if err != nil || len(entries) != 1 {
		return "", fmt.Errorf("%w: mise ls --current --json %s printed something other than one entry", errNotPinned, tool)
	}
	if err = entries[0].check(root, tool, path); err != nil {
		return "", err
	}
	return path, nil
}

func (e miseEntry) check(root, tool, path string) error {
	switch {
	case e.Source.Path != filepath.Join(root, "mise.toml"):
		return fmt.Errorf("%w: mise resolves %s from %s, not %s/mise.toml", errNotPinned, tool, e.Source.Path, root)
	case !e.Installed:
		return fmt.Errorf("%w: %s %s is not installed. Run mise install", errNotPinned, tool, e.Version)
	case !filepath.IsAbs(e.InstallPath) || !strings.HasPrefix(path, e.InstallPath+string(filepath.Separator)):
		return fmt.Errorf("%w: mise resolves %s to %s, outside %s", errNotPinned, tool, path, e.InstallPath)
	}
	return nil
}
