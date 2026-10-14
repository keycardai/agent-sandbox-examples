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

// asked returns the error for a huh form that returned err. When ctx has
// ended, it returns ctx's error, because huh reports that case as
// huh.ErrTimeout. On Ctrl-C it returns sandbox.ErrInterrupted.
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
