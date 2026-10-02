package sandbox

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// EventKind says what an Event reports.
type EventKind int

// The events the CLI reports.
const (
	// EventHeader opens a run. Its Text is the management API URL.
	EventHeader EventKind = iota
	// EventSection starts a group of steps.
	EventSection
	EventOK
	EventWarn
	EventInfo
	// EventQuote is text from elsewhere, such as a file the agent read.
	EventQuote
	EventStepStart
	EventStepEnd
)

// Event is one thing the CLI reports.
type Event struct {
	Kind EventKind
	// ID pairs a step's start and end.
	ID      int
	Text    string
	Detail  string
	Err     error
	Elapsed time.Duration
}

// UI reports what the CLI is doing. With Emit set, a front end such as the
// terminal UI receives each event. Otherwise UI prints plain lines to Out,
// which is what scripts and logs read.
type UI struct {
	Out io.Writer
	// Interactive says the CLI may prompt.
	Interactive bool
	Emit        func(Event)
	steps       int
}

// Header opens the run.
func (u *UI) Header(apiURL string) { u.emit(Event{Kind: EventHeader, Text: apiURL}) }

// Section starts a group of steps.
func (u *UI) Section(title string) { u.emit(Event{Kind: EventSection, Text: title}) }

// OK reports something done.
func (u *UI) OK(text string) { u.emit(Event{Kind: EventOK, Text: text}) }

// Warn reports something to look out for.
func (u *UI) Warn(text string) { u.emit(Event{Kind: EventWarn, Text: text}) }

// Info reports a note.
func (u *UI) Info(text string) { u.emit(Event{Kind: EventInfo, Text: text}) }

// Quote reports text from elsewhere, such as a file the agent read.
func (u *UI) Quote(text string) { u.emit(Event{Kind: EventQuote, Text: text}) }

// Step runs fn and reports its outcome with the detail fn returns and how
// long it took. A failure reports the error and returns it.
func (u *UI) Step(ctx context.Context, title string, fn func(context.Context) (string, error)) error {
	u.steps++
	id := u.steps
	u.emit(Event{Kind: EventStepStart, ID: id, Text: title})
	start := time.Now()
	detail, err := fn(ctx)
	u.emit(Event{Kind: EventStepEnd, ID: id, Text: title, Detail: detail, Err: err, Elapsed: time.Since(start)})
	return err
}

func (u *UI) emit(e Event) {
	if u.Emit != nil {
		u.Emit(e)
		return
	}
	if line, ok := PlainLine(e); ok {
		_, _ = fmt.Fprintln(u.Out, line)
	}
}

// PlainLine renders an event as text without styles. A step's start has no
// line, because its end reports it.
func PlainLine(e Event) (string, bool) {
	switch e.Kind {
	case EventHeader:
		return "Keycard sandbox · " + e.Text, true
	case EventSection:
		return "\n" + e.Text, true
	case EventOK:
		return "  ✓ " + e.Text, true
	case EventWarn:
		return "  ! " + e.Text, true
	case EventInfo:
		return "    " + e.Text, true
	case EventQuote:
		var b strings.Builder
		for _, line := range strings.Split(strings.TrimRight(e.Text, "\n"), "\n") {
			b.WriteString("      | " + line + "\n")
		}
		return strings.TrimRight(b.String(), "\n"), true
	case EventStepEnd:
		line := e.Text
		if e.Detail != "" {
			line += " · " + e.Detail
		}
		if e.Elapsed > 500*time.Millisecond {
			line += "  " + e.Elapsed.Round(100*time.Millisecond).String()
		}
		if e.Err != nil {
			return "  ✗ " + line + "\n    " + e.Err.Error(), true
		}
		return "  ✓ " + line, true
	default:
		return "", false
	}
}
