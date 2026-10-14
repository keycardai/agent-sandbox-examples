package main

import (
	"context"
	"errors"
	"fmt"

	"charm.land/huh/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// terminalAsker asks questions on the terminal with huh.
type terminalAsker struct{}

func (terminalAsker) Confirm(ctx context.Context, question string, value bool) (bool, error) {
	err := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title(question).Value(&value))).
		WithShowHelp(false).
		RunWithContext(ctx)
	return value, asked(ctx, err)
}

// asked turns huh's errors into the errors sandbox.Asker documents. huh
// reports an ended ctx as huh.ErrTimeout, so asked checks ctx first.
func asked(ctx context.Context, err error) error {
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("waiting for an answer: %w", ctx.Err())
	case errors.Is(err, huh.ErrUserAborted):
		return sandbox.ErrInterrupted
	}
	return fmt.Errorf("asking: %w", err)
}
