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

// Asker asks the operator questions on the terminal. Its methods return an
// error that wraps ctx.Err() when ctx ends, and ErrInterrupted on Ctrl-C.
type Asker interface {
	// Confirm asks a yes or no question. value is the default answer.
	Confirm(ctx context.Context, question string, value bool) (bool, error)
}

// confirm asks question with l.Ask. It returns ErrNoTerminal when l.Ask is
// nil.
func (l *Local) confirm(ctx context.Context, question string, value bool) (bool, error) {
	if l.Ask == nil {
		return false, ErrNoTerminal
	}
	return l.Ask.Confirm(ctx, question, value) //nolint:wrapcheck // Asker documents its errors. Callers add context.
}
