package page

import "github.com/keycardai/agent-sandbox-examples/internal/sandbox"

// QuestionKind is a kind of question the setup wizard asks.
type QuestionKind int

// The kinds of question.
const (
	AskInput QuestionKind = iota
	AskSecret
	AskChoose
	AskConfirm
)

// Question is one question from the setup wizard. Check, when set, says
// why a typed answer will not do.
type Question struct {
	Kind    QuestionKind
	Title   string
	Value   string
	Yes     bool
	Options []sandbox.Option
	Check   func(string) error
	// rows is how many rows a choice's options take, wrapped.
	rows int
}

type answer struct {
	text string
	yes  bool
}

// SectionMsg says which part of the settings the next questions are about,
// and where that part's notes start in the setup log.
type SectionMsg struct {
	ID        string
	NotesFrom int
}

// PresetMsg says the wizard took a preset answer, for the step rail.
type PresetMsg struct {
	Question *Question
	Text     string
}
