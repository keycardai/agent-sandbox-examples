package main

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/install"
)

// TestCommand runs command lines that each fail before any tool runs, with
// or without the check under test. A broken check then fails the test and
// does not touch a cluster.
func TestCommand(t *testing.T) {
	t.Parallel()
	// values does not exist, so install refuses it before it runs a tool.
	values := filepath.Join(t.TempDir(), "a,b.yaml")
	tests := []struct {
		name     string
		args     []string
		want     error
		wantText string
	}{
		{
			name:     "values path with a comma is one path",
			args:     []string{"install", "--context", "x", "--values", values},
			want:     install.ErrInstall,
			wantText: values + " is not a readable file",
		},
		{
			name:     "argument after a step's flags",
			args:     []string{"bootstrap", "--cluster", "asp", "--issuer", "https://asp.example.com", "--dry-run", "extra"},
			want:     errUnexpectedArgument,
			wantText: `unexpected argument "extra". bootstrap takes only flags`,
		},
		{
			name:     "mistyped subcommand",
			args:     []string{"chek"},
			want:     errUnexpectedArgument,
			wantText: `unexpected argument "chek". Run go run ./cmd/sandbox --help`,
		},
		{
			name:     "argument to install",
			args:     []string{"install", "--context", "x", "--values", values, "extra"},
			want:     errUnexpectedArgument,
			wantText: `unexpected argument "extra". install takes only flags`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := newCommand()
			cmd.Writer = io.Discard
			err := cmd.Run(t.Context(), append([]string{"sandbox"}, tc.args...))
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err = %v, want %v containing %q", err, tc.want, tc.wantText)
			}
		})
	}
}
