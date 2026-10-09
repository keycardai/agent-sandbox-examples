package chart_test

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/chart"
	"github.com/keycardai/agent-sandbox-examples/internal/preflight"
)

// TestPreflight runs preflight.Check over each chart's render, with the
// values the other chart tests use. Check judges the install chart's
// SandboxTemplate and the sandbox chart's Sandbox. It refuses a render in
// which it judges nothing.
func TestPreflight(t *testing.T) {
	t.Parallel()
	installDir, err := filepath.Abs("../../install")
	if err != nil {
		t.Fatal(err)
	}
	renders := []struct {
		name, dir, release string
		values             []string
	}{
		{"install", installDir, release, []string{filepath.Join(installDir, "values.example.yaml")}},
		{"sandbox", sandboxDir(t), sandboxUserID, sandboxValues(t)},
	}
	for _, r := range renders {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			docs, err := chart.Render(t.Context(), r.dir, r.release, "agents", r.values...)
			if err != nil {
				t.Fatal(err)
			}
			report := preflight.Check(bytes.Join(docs, []byte("\n---\n")))
			for _, refusal := range report.Refusals {
				t.Errorf("%s: %s: %s", refusal.Rule, refusal.Where, refusal.Detail)
			}
		})
	}
}
