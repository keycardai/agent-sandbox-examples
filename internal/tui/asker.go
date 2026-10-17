package tui

import (
	"context"
	"fmt"
	"slices"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
)

// asker is the sandbox.Asker the setup runs with in the TUI. Each question
// goes to Update as an askMsg, and the wizard's goroutine waits for the
// reply. Once done closes, every question returns sandbox.ErrInterrupted,
// as the terminal's asker does on Ctrl-C. When ctx ends, it returns an
// error that wraps ctx.Err().
type asker struct {
	send func(msg any)
	done <-chan struct{}
	// preset answers a choice by its question, such as the demo the picker
	// chose, without showing it.
	preset map[string]string
	// log is the setup log the wizard writes its notes to.
	log *component.Log
}

// askMsg is a question for the TUI, and where its reply goes.
type askMsg struct {
	question *page.Question
	replies  chan<- reply
}

// reply is the operator's answer to a question, or the error that ended
// it.
type reply struct {
	text string
	yes  bool
	err  error
}

// Input asks for a line of text.
func (a asker) Input(ctx context.Context, title, value string, check func(string) error) (string, error) {
	got := a.ask(ctx, &page.Question{Kind: page.AskInput, Title: title, Value: value, Check: check})
	return got.text, got.err
}

// Secret asks for a secret, hidden as it is typed.
func (a asker) Secret(ctx context.Context, title string, check func(string) error) (string, error) {
	got := a.ask(ctx, &page.Question{Kind: page.AskSecret, Title: title, Check: check})
	return got.text, got.err
}

// Choose asks for one of the options, or takes the preset answer.
func (a asker) Choose(ctx context.Context, title string, options []sandbox.Option, value string) (string, error) {
	quest := &page.Question{Kind: page.AskChoose, Title: title, Value: value, Options: options}
	if preset, ok := a.preset[title]; ok && slices.ContainsFunc(options, func(o sandbox.Option) bool {
		return o.Value == preset
	}) {
		a.send(page.PresetMsg{Question: quest, Text: preset})
		return preset, nil
	}
	got := a.ask(ctx, quest)
	return got.text, got.err
}

// Confirm asks yes or no.
func (a asker) Confirm(ctx context.Context, title string, value bool) (bool, error) {
	got := a.ask(ctx, &page.Question{Kind: page.AskConfirm, Title: title, Yes: value})
	return got.yes, got.err
}

// Section tells the TUI which part of the settings the wizard asks about
// next, for the step rail. It implements sandbox.SectionAsker. The wizard
// writes its notes from this goroutine, so the log's length here is where
// the part's notes start.
func (a asker) Section(id string) {
	from := 0
	if a.log != nil {
		from = a.log.Len()
	}
	a.send(page.SectionMsg{ID: id, NotesFrom: from})
}

func (a asker) ask(ctx context.Context, quest *page.Question) reply {
	replies := make(chan reply, 1)
	a.send(askMsg{question: quest, replies: replies})
	select {
	case got := <-replies:
		return got
	case <-a.done:
		return reply{err: sandbox.ErrInterrupted}
	case <-ctx.Done():
		return reply{err: fmt.Errorf("waiting for an answer: %w", ctx.Err())}
	}
}

// startQuestions begins a run of the setup's questions, and returns the
// asker that sends them to Update, writes notes to log, and answers
// preset's choices.
func (m *model) startQuestions(preset map[string]string, log *component.Log) asker {
	m.questions = make(chan struct{})
	return asker{send: m.bus.Send, done: m.questions, preset: preset, log: log}
}

// asking reports whether a run of questions is open: it started, and was
// neither stopped nor ended.
func (m *model) asking() bool { return m.questions != nil }

// stopQuestions ends the run's questions, which then return
// sandbox.ErrInterrupted, and drops the question that shows.
func (m *model) stopQuestions() {
	if m.questions != nil {
		close(m.questions)
	}
	m.endQuestions()
}

// endQuestions forgets the run, once its work is done.
func (m *model) endQuestions() {
	m.questions, m.replies = nil, nil
	m.setup.Drop()
}
