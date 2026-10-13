package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/install"
)

// TestInstallValuesComma checks that install keeps a values path with a comma
// as one path. The file does not exist, so install refuses it before it runs
// a tool, and the refusal names the whole path.
func TestInstallValuesComma(t *testing.T) {
	t.Parallel()
	values := filepath.Join(t.TempDir(), "a,b.yaml")
	err := newCommand().Run(t.Context(), []string{"sandbox", "install", "--context", "x", "--values", values})
	if !errors.Is(err, install.ErrInstall) || !strings.Contains(err.Error(), values+" is not a readable file") {
		t.Fatalf("err = %v, want %s refused as one path", err, values)
	}
}
