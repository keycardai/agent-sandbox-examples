package tui

import (
	"errors"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
)

// TestAsker sends a question to the TUI and its answer back, answers a
// preset choice without asking, asks a preset the options lack, and
// returns sandbox.ErrInterrupted once the setup stops.
func TestAsker(t *testing.T) {
	t.Parallel()
	queue := make(chan any, 1)
	done := make(chan struct{})
	options := []sandbox.Option{
		{Label: "pi coding agent (pi)", Value: "pi"},
		{Label: "Smoke (smoke)", Value: sandbox.DefaultScenario},
	}
	ask := asker{
		send:   func(msg any) { queue <- msg },
		done:   done,
		preset: map[string]string{sandbox.QuestionDemo: "pi"},
	}
	if got, err := ask.Choose(
		t.Context(),
		sandbox.QuestionDemo,
		options,
		sandbox.DefaultScenario,
	); err != nil ||
		got != "pi" {
		t.Errorf("Choose = %q, %v; want the preset pi", got, err)
	}
	if msg, ok := (<-queue).(page.PresetMsg); !ok || msg.Text != "pi" {
		t.Errorf("message = %+v, want the preset for the rail", msg)
	}
	go func() {
		if msg, ok := (<-queue).(askMsg); ok {
			msg.replies <- reply{text: sandbox.DefaultScenario}
		}
	}()
	ask.preset = map[string]string{sandbox.QuestionDemo: "gone"}
	if got, err := ask.Choose(t.Context(), sandbox.QuestionDemo, options, sandbox.DefaultScenario); err != nil ||
		got != sandbox.DefaultScenario {
		t.Errorf("Choose with a missing preset = %q, %v; want the question asked", got, err)
	}
	close(done)
	if _, err := ask.Input(t.Context(), "Name", "", nil); !errors.Is(err, sandbox.ErrInterrupted) {
		t.Errorf("Input after stop = %v, want sandbox.ErrInterrupted", err)
	}
}
