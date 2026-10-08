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
	"slices"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/keycardai/agent-sandbox-poc/internal/pinned"
)

// ErrHelm is a `helm` invocation that failed.
var ErrHelm = errors.New("helm")

// Render runs `helm template` on the chart at dir with the release namespace
// and values files given, and returns every non-empty YAML document it
// produced, in output order.
func Render(ctx context.Context, dir, release, namespace string, valuesFiles ...string) ([][]byte, error) {
	values := make([]string, 0, len(valuesFiles))
	for _, file := range valuesFiles {
		values = append(values, "--values="+file)
	}
	args := slices.Concat([]string{"template", "--namespace=" + namespace}, values, []string{"--", release, dir})
	out, err := pinned.Output(ctx, nil, "helm", args...)
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
