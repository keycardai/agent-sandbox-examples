package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"charm.land/huh/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
)

// terminalAsker asks questions on the terminal with huh, in the TUI's form
// theme, and writes a heading to out before each part of setup.
type terminalAsker struct {
	look func() *look
	out  io.Writer
}

func (a terminalAsker) Input(ctx context.Context, question, value string, check func(string) error) (string, error) {
	err := a.ask(ctx, huh.NewInput().Title(question).Value(&value).Validate(check))
	return value, err
}

func (a terminalAsker) Secret(ctx context.Context, question string, check func(string) error) (string, error) {
	var value string
	err := a.ask(ctx, huh.NewInput().Title(question).EchoMode(huh.EchoModePassword).Value(&value).Validate(check))
	return value, err
}

func (a terminalAsker) Choose(
	ctx context.Context,
	question string,
	options []sandbox.Option,
	value string,
) (string, error) {
	choices := make([]huh.Option[string], 0, len(options))
	for _, option := range options {
		choices = append(choices, huh.NewOption(option.Label, option.Value))
	}
	err := a.ask(ctx, huh.NewSelect[string]().Title(question).Options(choices...).Value(&value))
	return value, err
}

// Confirm asks yes or no with the TUI's yes-or-no field. y and n move to
// the answer, and enter gives it. The TUI answers on y or n at once.
func (a terminalAsker) Confirm(ctx context.Context, question string, value bool) (bool, error) {
	answer := component.YesNoValue(value)
	err := a.run(ctx, a.yesNoForm(&answer, question))
	return answer == component.AnswerYes, err
}

// Section writes the heading of a part of setup, before its first question.
func (a terminalAsker) Section(name string) {
	_, _ = fmt.Fprint(a.out, a.look().heading(name))
}

// ask runs a form with one field until the operator answers or ctx ends.
func (a terminalAsker) ask(ctx context.Context, field huh.Field) error {
	return a.run(ctx, a.form(field))
}

// run runs form until the operator answers or ctx ends. When ctx has
// ended, it returns ctx's error, because huh reports that case as
// huh.ErrTimeout. On Ctrl-C it returns sandbox.ErrInterrupted.
func (a terminalAsker) run(ctx context.Context, form *huh.Form) error {
	err := form.RunWithContext(ctx)
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

// yesNoForm asks question with component.YesNo and its keys, keeping the
// answer in value.
func (a terminalAsker) yesNoForm(value *string, question string) *huh.Form {
	return a.form(component.YesNo(value).Title(question)).WithKeyMap(component.YesNoKeys())
}

// form is a form of one field in the TUI's form theme, as wide as the look.
func (a terminalAsker) form(field huh.Field) *huh.Form {
	look := a.look()
	return huh.NewForm(huh.NewGroup(field)).WithShowHelp(false).
		WithTheme(look.styles.FormTheme(look.width)).WithWidth(look.width)
}
