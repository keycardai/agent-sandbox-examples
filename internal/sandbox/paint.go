package sandbox

// Tone is what a progress line says, for a terminal to color.
type Tone int

// The tones of the progress lines.
const (
	// ToneCommand is a command the operator can run by hand.
	ToneCommand Tone = iota
	// ToneStep starts a part of a step.
	ToneStep
	// ToneGood says that the sandbox works.
	ToneGood
	// ToneWarn says that Up stopped short.
	ToneWarn
)

// paint returns text colored for tone by l.Paint, or text as it is when
// l.Paint is nil.
func (l *Local) paint(tone Tone, text string) string {
	if l.Paint == nil {
		return text
	}
	return l.Paint(tone, text)
}
