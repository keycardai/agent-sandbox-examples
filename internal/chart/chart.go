// Package chart renders a Helm chart with `helm template`, so a test can pin
// what the chart says about a cluster without a cluster.
package chart

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// ErrHelm is a `helm` invocation that failed.
var ErrHelm = errors.New("helm")

// Render runs `helm template` on the chart at dir with the values files and
// returns every non-empty YAML document it produced, in output order. It runs
// the helm that the mise.toml governing dir pins, and refuses any other.
func Render(ctx context.Context, dir, release string, valuesFiles ...string) ([][]byte, error) {
	helm, err := pinnedHelm(ctx, dir)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(valuesFiles))
	for _, file := range valuesFiles {
		values = append(values, "--values="+file)
	}
	args := slices.Concat([]string{"template"}, values, []string{"--", release, dir})
	//nolint:gosec // G204: mise chooses helm and the caller chooses the chart and values paths.
	cmd := exec.CommandContext(ctx, helm, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w: %s", ErrHelm, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	reader := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(out)))
	var docs [][]byte
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("splitting helm output: %w", err)
		}
		if len(bytes.TrimSpace(doc)) > 0 {
			docs = append(docs, doc)
		}
	}
}

// pinnedHelm returns the path of the helm that the mise.toml nearest dir pins.
func pinnedHelm(ctx context.Context, dir string) (string, error) {
	mise, err := exec.LookPath("mise")
	if err != nil {
		return "", fmt.Errorf("%w: mise is not installed: %w", ErrHelm, err)
	}
	config, err := governingConfig(dir)
	if err != nil {
		return "", err
	}
	which, err := runMise(ctx, mise, dir, "which", "helm")
	if err != nil {
		return "", err
	}
	ls, err := runMise(ctx, mise, dir, "ls", "--current", "--json", "helm")
	if err != nil {
		return "", err
	}
	return checkPinned(config, string(which), ls)
}

// governingConfig returns the nearest mise.toml at or above dir, with symlinks
// resolved.
func governingConfig(dir string) (string, error) {
	resolved, err := filepath.Abs(dir)
	if err == nil {
		resolved, err = filepath.EvalSymlinks(resolved)
	}
	if err != nil {
		return "", fmt.Errorf("%w: resolving chart directory %s: %w", ErrHelm, dir, err)
	}
	for d := resolved; ; d = filepath.Dir(d) {
		config := filepath.Join(d, "mise.toml")
		if _, err := os.Stat(config); err == nil {
			return config, nil
		}
		if d == filepath.Dir(d) {
			return "", fmt.Errorf("%w: no mise.toml at or above %s", ErrHelm, resolved)
		}
	}
}

// runMise runs mise in dir and returns its standard output. A failure carries
// mise's standard error.
func runMise(ctx context.Context, mise, dir string, args ...string) ([]byte, error) {
	args = append([]string{"-C", dir}, args...)
	//nolint:gosec // G204: mise comes from exec.LookPath and the arguments are fixed.
	cmd := exec.CommandContext(ctx, mise, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf(
			"%w: mise %s: %w: %s", ErrHelm, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()),
		)
	}
	return out, nil
}

// checkPinned returns which, the output of `mise which helm`, when list, the
// output of `mise ls --current --json helm`, is one entry whose source is
// config, whose version is installed, and whose install path holds which.
// mise reports the source with symlinks resolved.
func checkPinned(config, which string, list []byte) (string, error) {
	var entries []struct {
		Version     string `json:"version"`
		InstallPath string `json:"install_path"`
		Source      struct {
			Path string `json:"path"`
		} `json:"source"`
		Installed bool `json:"installed"`
	}
	if err := json.Unmarshal(list, &entries); err != nil {
		return "", fmt.Errorf("%w: mise ls --current --json helm printed something other than one entry: %w", ErrHelm, err)
	}
	if len(entries) != 1 {
		return "", fmt.Errorf("%w: mise ls --current --json helm printed %d entries, not one", ErrHelm, len(entries))
	}
	entry := entries[0]
	if filepath.Clean(entry.Source.Path) != config {
		return "", fmt.Errorf("%w: mise resolves helm from %q, not %s", ErrHelm, entry.Source.Path, config)
	}
	if !entry.Installed {
		return "", fmt.Errorf("%w: helm %s is not installed, run mise install", ErrHelm, entry.Version)
	}
	which = filepath.Clean(strings.TrimSpace(which))
	prefix := filepath.Clean(entry.InstallPath) + string(filepath.Separator)
	if entry.InstallPath == "" || !strings.HasPrefix(which, prefix) {
		return "", fmt.Errorf("%w: mise resolves helm to %q, outside %q", ErrHelm, which, entry.InstallPath)
	}
	return which, nil
}
