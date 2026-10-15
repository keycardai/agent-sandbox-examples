package page

import (
	"context"
	"fmt"
	"slices"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
)

// QuestionKind is a kind of question the setup wizard asks.
type QuestionKind int

// The kinds of question.
const (
	AskInput QuestionKind = iota
	AskSecret
	AskChoose
	AskConfirm
)

// Question is one question from the setup wizard. The wizard waits on its
// reply until the TUI answers.
type Question struct {
	Kind    QuestionKind
	Title   string
	Value   string
	Yes     bool
	Options []sandbox.Option
	// rows is how many rows a choice's options take, wrapped.
	rows  int
	check func(string) error
	reply chan answer
}

// answer is the operator's answer to a question, or the error that ended
// it.
type answer struct {
	text string
	yes  bool
	err  error
}

// AskMsg is a question for the TUI.
type AskMsg struct{ Question *Question }

// SectionMsg says which part of the settings the next questions are about,
// and where that part's notes start in the setup log.
type SectionMsg struct {
	ID        string
	NotesFrom int
}

// PresetMsg says the wizard took a preset answer, for the step rail.
type PresetMsg struct {
	question *Question
	text     string
}

// Asker is the sandbox.Asker the setup wizard runs with in the TUI. Each
// question goes to the TUI as an AskMsg, and the wizard's goroutine waits
// for the answer. When Done closes, every question returns
// sandbox.ErrInterrupted, as the terminal's asker does on Ctrl-C. When ctx
// ends, it returns an error that wraps ctx.Err().
type Asker struct {
	Send func(msg any)
	Done <-chan struct{}
	// Preset answers a choice by its question, such as the demo the picker
	// chose, without showing it.
	Preset map[string]string
	// Log is the setup log the wizard writes its notes to.
	Log *component.Log
}

// Input asks for a line of text.
func (a Asker) Input(ctx context.Context, title, value string, check func(string) error) (string, error) {
	got := a.ask(ctx, &Question{Kind: AskInput, Title: title, Value: value, check: check})
	return got.text, got.err
}

// Secret asks for a secret, hidden as it is typed.
func (a Asker) Secret(ctx context.Context, title string, check func(string) error) (string, error) {
	got := a.ask(ctx, &Question{Kind: AskSecret, Title: title, check: check})
	return got.text, got.err
}

// Choose asks for one of the options, or takes the preset answer.
func (a Asker) Choose(ctx context.Context, title string, options []sandbox.Option, value string) (string, error) {
	quest := &Question{Kind: AskChoose, Title: title, Value: value, Options: options}
	if preset, ok := a.Preset[title]; ok && slices.ContainsFunc(options, func(o sandbox.Option) bool {
		return o.Value == preset
	}) {
		a.Send(PresetMsg{question: quest, text: preset})
		return preset, nil
	}
	got := a.ask(ctx, quest)
	return got.text, got.err
}

// Confirm asks yes or no.
func (a Asker) Confirm(ctx context.Context, title string, value bool) (bool, error) {
	got := a.ask(ctx, &Question{Kind: AskConfirm, Title: title, Yes: value})
	return got.yes, got.err
}

// Section tells the TUI which part of the settings the wizard asks about
// next, for the step rail. It implements sandbox.SectionAsker. The wizard
// writes its notes from this goroutine, so the log's length here is where
// the part's notes start.
func (a Asker) Section(id string) {
	from := 0
	if a.Log != nil {
		from = a.Log.Len()
	}
	a.Send(SectionMsg{ID: id, NotesFrom: from})
}

func (a Asker) ask(ctx context.Context, q *Question) answer {
	q.reply = make(chan answer, 1)
	a.Send(AskMsg{Question: q})
	select {
	case got := <-q.reply:
		return got
	case <-a.Done:
		return answer{err: sandbox.ErrInterrupted}
	case <-ctx.Done():
		return answer{err: fmt.Errorf("waiting for an answer: %w", ctx.Err())}
	}
}
