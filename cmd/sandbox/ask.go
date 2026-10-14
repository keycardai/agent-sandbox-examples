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

func (terminalAsker) Input(ctx context.Context, question, value string, check func(string) error) (string, error) {
	err := ask(ctx, huh.NewInput().Title(question).Value(&value).Validate(check))
	return value, err
}

func (terminalAsker) Secret(ctx context.Context, question string, check func(string) error) (string, error) {
	var value string
	err := ask(ctx, huh.NewInput().Title(question).EchoMode(huh.EchoModePassword).Value(&value).Validate(check))
	return value, err
}

func (terminalAsker) Choose(
	ctx context.Context,
	question string,
	options []sandbox.Option,
	value string,
) (string, error) {
	choices := make([]huh.Option[string], 0, len(options))
	for _, option := range options {
		choices = append(choices, huh.NewOption(option.Label, option.Value))
	}
	err := ask(ctx, huh.NewSelect[string]().Title(question).Options(choices...).Value(&value))
	return value, err
}

func (terminalAsker) Confirm(ctx context.Context, question string, value bool) (bool, error) {
	err := ask(ctx, huh.NewConfirm().Title(question).Value(&value))
	return value, err
}

// ask runs a form with one field until the operator answers or ctx ends.
// When ctx has ended, it returns ctx's error, because huh reports that case
// as huh.ErrTimeout. On Ctrl-C it returns sandbox.ErrInterrupted.
func ask(ctx context.Context, field huh.Field) error {
	err := huh.NewForm(huh.NewGroup(field)).WithShowHelp(false).RunWithContext(ctx)
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
