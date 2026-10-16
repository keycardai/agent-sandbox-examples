package main

import (
	"os"
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

// maxWidth is the widest a question or a heading gets, the width the TUI
// reads at.
const maxWidth = 76

// look is how a subcommand draws on the terminal: with the TUI's styles,
// fitted to the colors the terminal shows.
type look struct {
	styles  style.Styles
	profile colorprofile.Profile
	width   int
}

// terminalLook returns a function that finds the look of the terminal out
// writes to on its first call. Asking the terminal for its background takes
// a moment, and a subcommand that asks nothing and prints no progress does
// not need it.
func terminalLook(out *os.File) func() *look {
	return sync.OnceValue(func() *look {
		found := &look{profile: colorprofile.Detect(out, os.Environ()), width: maxWidth}
		dark := true
		if found.colored() {
			dark = lipgloss.HasDarkBackground(os.Stdin, out)
		}
		found.styles = style.New(dark)
		if width, _, err := term.GetSize(out.Fd()); err == nil && width > 0 {
			found.width = min(width, maxWidth)
		}
		return found
	})
}

// colored reports whether the terminal shows colors. NO_COLOR, a dumb
// terminal and output that is not a terminal show none.
func (lk *look) colored() bool {
	return lk.profile >= colorprofile.ANSI
}

// paint colors a progress line as the TUI colors text that means the same:
// iris for a command, bold for a step, green for a sandbox that works and
// amber for an Up that stopped. A step keeps the terminal's own color,
// which shows on any background.
func (lk *look) paint(tone sandbox.Tone, text string) string {
	if !lk.colored() {
		return text
	}
	var styled lipgloss.Style
	switch tone {
	case sandbox.ToneCommand:
		styled = lk.styles.Accent
	case sandbox.ToneStep:
		styled = lipgloss.NewStyle().Bold(true)
	case sandbox.ToneGood:
		styled = lk.styles.OK
	case sandbox.ToneWarn:
		styled = lk.styles.Warn
	}
	return lk.fit(styled.Render(text))
}

// heading is the title of a part of setup and what it is for, as the TUI's
// setup shows them. It is "" for a part setup has no section for.
func (lk *look) heading(id string) string {
	section := sandbox.SectionOf(id)
	if section.Title == "" {
		return ""
	}
	about := lipgloss.Wrap(section.About, lk.width, " ")
	if !lk.colored() {
		return "\n" + section.Title + "\n" + about + "\n\n"
	}
	return lk.fit("\n" + lk.styles.SectionTitle.Render(section.Title) + "\n" + lk.styles.Muted.Render(about) + "\n\n")
}

// fit turns the colors in text into ones the terminal shows. The styles
// write 24-bit colors, which some terminals do not show.
func (lk *look) fit(text string) string {
	var out strings.Builder
	_, _ = (&colorprofile.Writer{Forward: &out, Profile: lk.profile}).WriteString(text)
	return out.String()
}
