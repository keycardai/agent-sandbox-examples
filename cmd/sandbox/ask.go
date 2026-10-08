package main

import (
	"errors"
	"fmt"

	"charm.land/huh/v2"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

var errAborted = errors.New("stopped before writing the settings files")

// terminalAsker asks the questions of `sandbox init` on the terminal.
type terminalAsker struct{}

func (terminalAsker) Input(question, value string, check func(string) error) (string, error) {
	err := huh.NewInput().Title(question).Value(&value).Validate(check).Run()
	return value, aborted(err)
}

func (terminalAsker) Secret(question string, check func(string) error) (string, error) {
	var value string
	err := huh.NewInput().Title(question).EchoMode(huh.EchoModePassword).Value(&value).Validate(check).Run()
	return value, aborted(err)
}

func (terminalAsker) Choose(question string, options []sandbox.Option, value string) (string, error) {
	choices := make([]huh.Option[string], 0, len(options))
	for _, option := range options {
		choices = append(choices, huh.NewOption(option.Label, option.Value))
	}
	err := huh.NewSelect[string]().Title(question).Options(choices...).Value(&value).Run()
	return value, aborted(err)
}

func (terminalAsker) Confirm(question string, value bool) (bool, error) {
	err := huh.NewConfirm().Title(question).Value(&value).Run()
	return value, aborted(err)
}

// aborted turns Ctrl-C on a question into errAborted.
func aborted(err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		return errAborted
	}
	if err != nil {
		return fmt.Errorf("asking: %w", err)
	}
	return nil
}
