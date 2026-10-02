package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

var errBoom = errors.New("boom")

// TestPlainUIReportsEachStep checks the output scripts and logs read: one line
// per finished step, the error under a failed one, and an emitter that sees
// the step start and end.
func TestPlainUIReportsEachStep(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	ui := &sandbox.UI{Out: &out}
	ctx := context.Background()
	_ = ui.Step(ctx, "Zone", func(context.Context) (string, error) { return testZone, nil })
	err := ui.Step(ctx, "Cluster", func(context.Context) (string, error) { return "", errBoom })
	if !errors.Is(err, errBoom) {
		t.Fatalf("Step returned %v, want the step's error", err)
	}
	want := "  ✓ Zone · zone_1\n  ✗ Cluster\n    boom\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}

	var kinds []sandbox.EventKind
	emitting := &sandbox.UI{Emit: func(e sandbox.Event) { kinds = append(kinds, e.Kind) }}
	_ = emitting.Step(ctx, "Zone", func(context.Context) (string, error) { return "", nil })
	if len(kinds) != 2 || kinds[0] != sandbox.EventStepStart || kinds[1] != sandbox.EventStepEnd {
		t.Errorf("emitted %v, want a start then an end", kinds)
	}
}

func TestElapsedHidesQuickSteps(t *testing.T) {
	t.Parallel()

	if got := sandbox.Elapsed(300 * time.Millisecond); got != "" {
		t.Errorf("a quick step shows %q", got)
	}
	if got := sandbox.Elapsed(1234 * time.Millisecond); got != "1.2s" {
		t.Errorf("Elapsed = %q, want 1.2s", got)
	}
}
