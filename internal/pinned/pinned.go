// Package pinned runs the external tools that the repository's mise.toml
// pins, through scripts/pinned. scripts/pinned refuses any other version of
// a tool.
package pinned

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

var errRepository = errors.New("repository")

// Command returns a command that runs tool with args through scripts/pinned.
// scripts/pinned execs the tool, so the command's process is the tool's.
func Command(ctx context.Context, tool string, args ...string) (*exec.Cmd, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	//nolint:gosec // G204: the script is this repository's and the caller chooses the tool and its arguments.
	return exec.CommandContext(ctx, filepath.Join(root, "scripts", "pinned"), slices.Concat([]string{tool}, args)...), nil
}

// Output runs tool with args and stdin through scripts/pinned and returns its
// standard output.
func Output(ctx context.Context, stdin []byte, tool string, args ...string) ([]byte, error) {
	cmd, err := Command(ctx, tool, args...)
	if err != nil {
		return nil, err
	}
	return Run(cmd, stdin)
}

// Run runs cmd with stdin and returns its standard output. A failure names
// the command by its program's base name and arguments, and includes its
// standard error.
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

// Root returns the repository that holds this package, from the path the
// build records for this file. Charts and scripts therefore always come from
// this checkout. The path is valid only where the source tree is, as in the
// tests and under `go run`. A binary copied to another machine gets a path
// that does not exist there.
func Root() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return "", fmt.Errorf("%w: cannot locate the repository from %q", errRepository, file)
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file))), nil
}
