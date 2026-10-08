package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/issuer"
)

// A token larger than the cap is refused, from stdin or from a file, so
// nothing past the cap is silently dropped.
func TestOversizedToken(t *testing.T) {
	t.Parallel()
	oversized := bytes.Repeat([]byte("a"), issuer.MaxBody+1)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"-", tokenFile} {
		cmd := newCommand()
		cmd.Reader = bytes.NewReader(oversized)
		err := cmd.Run(t.Context(), []string{
			"oidc-check", "--issuer", "https://issuer.example", "--token-file", path,
			"--audience", "https://zone.example", "--subject", "system:serviceaccount:agents:agent",
		})
		if !errors.Is(err, errToken) || !strings.Contains(err.Error(), "holds more than") {
			t.Fatalf("--token-file %s: err = %v, want errToken naming the size cap", path, err)
		}
	}
}
