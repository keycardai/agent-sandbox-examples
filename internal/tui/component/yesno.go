package component

import (
	"charm.land/bubbles/v2/key"
	"charm.land/huh/v2"
)

// AnswerYes and AnswerNo are the values of YesNo's two options.
const (
	AnswerYes = "yes"
	AnswerNo  = "no"
)

// YesNo is a yes-or-no question's field, in the TUI's setup and in the
// subcommands: a choice of two, which the same keys answer as any other
// choice. It keeps AnswerYes or AnswerNo in value.
func YesNo(value *string) *huh.Select[string] {
	return huh.NewSelect[string]().
		Options(huh.NewOption("Yes", AnswerYes), huh.NewOption("No", AnswerNo)).
		Value(value)
}

// YesNoValue is the value of YesNo's option for yes, or for no.
func YesNoValue(yes bool) string {
	if yes {
		return AnswerYes
	}
	return AnswerNo
}

// YesNoKeys are the keys of a form that asks only YesNo: y moves to Yes and
// n to No, its first and last options, and enter gives the answer.
func YesNoKeys() *huh.KeyMap {
	keys := huh.NewDefaultKeyMap()
	keys.Select.GotoTop = key.NewBinding(key.WithKeys("y", "Y", "home", "g"))
	keys.Select.GotoBottom = key.NewBinding(key.WithKeys("n", "N", "end", "G"))
	return keys
}
