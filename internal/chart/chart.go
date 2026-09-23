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
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// ErrHelm is a `helm` invocation that failed.
var ErrHelm = errors.New("helm")

var errRepository = errors.New("repository")

// Render runs `helm template` on the chart at dir with the release namespace
// and values files given, and returns every non-empty YAML document it
// produced, in output order.
func Render(ctx context.Context, dir, release, namespace string, valuesFiles ...string) ([][]byte, error) {
	values := make([]string, 0, len(valuesFiles))
	for _, file := range valuesFiles {
		values = append(values, "--values="+file)
	}
	args := slices.Concat([]string{"template", "--namespace=" + namespace}, values, []string{"--", release, dir})
	out, err := Pinned(ctx, nil, "helm", args...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHelm, err)
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

// Pinned runs tool with args and stdin through scripts/pinned and returns its
// standard output. scripts/pinned refuses any version of tool other than the
// one mise.toml pins.
func Pinned(ctx context.Context, stdin []byte, tool string, args ...string) ([]byte, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	//nolint:gosec // G204: the script is this repository's and the caller chooses the tool and its arguments.
	cmd := exec.CommandContext(ctx, filepath.Join(root, "scripts", "pinned"), slices.Concat([]string{tool}, args)...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", tool, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// repoRoot returns the repository that holds this package, from the path the
// build records for this file. A chart elsewhere therefore cannot supply the
// launcher. The path holds only where the source tree is, as in the tests
// and under `go run`. A shipped binary would need another anchor.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return "", fmt.Errorf("%w: cannot locate the repository from %q", errRepository, file)
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file))), nil
}
