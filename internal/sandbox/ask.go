package sandbox

import (
	"context"
	"errors"
)

var (
	// ErrNoTerminal means stdin is not a terminal, so Local cannot ask.
	ErrNoTerminal = errors.New("no terminal to ask")
	// ErrInterrupted means the operator pressed Ctrl-C at a question.
	ErrInterrupted = errors.New("interrupted")
)

// Asker asks the operator one question at a time on the terminal. value is
// the default answer. When check returns an error, the Asker asks again. Its
// methods return an error that wraps ctx.Err() when ctx ends, and
// ErrInterrupted on Ctrl-C.
type Asker interface {
	Input(ctx context.Context, question, value string, check func(string) error) (string, error)
	Secret(ctx context.Context, question string, check func(string) error) (string, error)
	Choose(ctx context.Context, question string, options []Option, value string) (string, error)
	Confirm(ctx context.Context, question string, value bool) (bool, error)
}

// Option is one choice. Choose shows Label and returns Value.
type Option struct {
	Label, Value string
}

// confirm asks question with l.Ask. It returns ErrNoTerminal when l.Ask is
// nil.
func (l *Local) confirm(ctx context.Context, question string, value bool) (bool, error) {
	if l.Ask == nil {
		return false, ErrNoTerminal
	}
	return l.Ask.Confirm(ctx, question, value) //nolint:wrapcheck // Asker documents its errors. Callers add context.
}

// choose asks question with l.Ask. It returns ErrNoTerminal when l.Ask is
// nil.
func (l *Local) choose(ctx context.Context, question string, options []Option, value string) (string, error) {
	if l.Ask == nil {
		return "", ErrNoTerminal
	}
	//nolint:wrapcheck // Asker documents its errors. Callers add context.
	return l.Ask.Choose(ctx, question, options, value)
}
