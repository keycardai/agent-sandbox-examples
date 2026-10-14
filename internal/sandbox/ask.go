package sandbox

import (
	"context"
	"errors"
)

var (
	// ErrNoTerminal is a question asked when stdin is not a terminal.
	ErrNoTerminal = errors.New("no terminal to ask")
	// ErrInterrupted is a question the operator stopped with Ctrl-C.
	ErrInterrupted = errors.New("interrupted")
)

// Asker asks the operator questions on the terminal. When ctx ends, a
// question returns an error that wraps ctx.Err(). Ctrl-C on a question
// returns ErrInterrupted.
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
