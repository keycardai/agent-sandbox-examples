// Package chart renders a Helm chart with `helm template`, so a test can pin
// what the chart says about a cluster without a cluster.
package chart

import (
	"bufio"
	"bytes"
	"context"
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
// helm through scripts/pinned in the repository that holds dir, which refuses
// any helm but the one that repository's mise.toml pins.
func Render(ctx context.Context, dir, release string, valuesFiles ...string) ([][]byte, error) {
	root, err := repoRoot(dir)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(valuesFiles))
	for _, file := range valuesFiles {
		values = append(values, "--values="+file)
	}
	args := slices.Concat([]string{"helm", "template"}, values, []string{"--", release, dir})
	//nolint:gosec // G204: the script is this repository's and the caller chooses the chart and values paths.
	cmd := exec.CommandContext(ctx, filepath.Join(root, "scripts", "pinned"), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf(
			"%w %s: %w: %s",
			ErrHelm,
			strings.Join(args[1:], " "),
			err,
			strings.TrimSpace(stderr.String()),
		)
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

// repoRoot returns the nearest directory at or above dir that holds a
// mise.toml, with symlinks resolved.
func repoRoot(dir string) (string, error) {
	resolved, err := filepath.Abs(dir)
	if err == nil {
		resolved, err = filepath.EvalSymlinks(resolved)
	}
	if err != nil {
		return "", fmt.Errorf("%w: resolving chart directory %s: %w", ErrHelm, dir, err)
	}
	for d := resolved; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "mise.toml")); err == nil {
			return d, nil
		}
		if d == filepath.Dir(d) {
			return "", fmt.Errorf("%w: no mise.toml at or above %s", ErrHelm, resolved)
		}
	}
}
