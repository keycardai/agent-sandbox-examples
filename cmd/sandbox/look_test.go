package main

import (
	"bytes"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

// TestLookPaint colors a progress line only where the terminal shows
// colors, and turns the styles' 24-bit colors into the 16 an ANSI terminal
// shows.
func TestLookPaint(t *testing.T) {
	t.Parallel()
	const line = "$ go run ./cmd/sandbox bootstrap"
	for _, tc := range []struct {
		name    string
		profile colorprofile.Profile
		// want is in the painted line, and absent is not.
		want, absent string
	}{
		{name: "not a terminal", profile: colorprofile.NoTTY, absent: "\x1b["},
		{name: "NO_COLOR", profile: colorprofile.ASCII, absent: "\x1b["},
		{name: "16 colors", profile: colorprofile.ANSI, want: "\x1b[", absent: "38;2;"},
		{name: "24-bit colors", profile: colorprofile.TrueColor, want: "38;2;"},
	} {
		lk := &look{styles: style.New(true), profile: tc.profile, width: maxWidth}
		got := lk.paint(sandbox.ToneCommand, line)
		if !strings.Contains(got, line) || !strings.Contains(got, tc.want) ||
			tc.absent != "" && strings.Contains(got, tc.absent) {
			t.Errorf("%s: painted %q, want %q in it and not %q", tc.name, got, tc.want, tc.absent)
		}
	}
}

// TestLookStep draws a step's title bold in the terminal's own color, so
// that it shows on a light background the look took for dark.
func TestLookStep(t *testing.T) {
	t.Parallel()
	lk := &look{styles: style.New(true), profile: colorprofile.TrueColor, width: maxWidth}
	if got := lk.paint(sandbox.ToneStep, "==> Set up the zone"); !strings.Contains(got, "\x1b[1m") ||
		strings.Contains(got, "38;2;") {
		t.Errorf("painted %q, want it bold, with no color of its own", got)
	}
}

// TestAskTheme draws a question in the TUI's form theme, in a rounded box.
// Yes or no is a choice of two, with the cursor on the answer it starts
// with.
func TestAskTheme(t *testing.T) {
	t.Parallel()
	themed := &look{styles: style.New(true), profile: colorprofile.TrueColor, width: maxWidth}
	asker := terminalAsker{look: func() *look { return themed }, out: &bytes.Buffer{}}
	text, answer := "", component.AnswerNo
	for _, tc := range []struct {
		name  string
		form  *huh.Form
		wants []string
	}{
		{"text", asker.form(huh.NewInput().Title("Zone URL").Value(&text)), []string{"╭", "Zone URL"}},
		{
			"yes or no", asker.form(component.YesNo(&answer).Title("Delete the cluster?")),
			[]string{"╭", "Delete the cluster?", "  Yes", "▸ No"},
		},
	} {
		// huh lays a field out once it knows the window, as it does when it
		// runs.
		_ = tc.form.Init()
		_, _ = tc.form.Update(tea.WindowSizeMsg{Width: maxWidth, Height: 24})
		view := ansi.Strip(tc.form.View())
		for _, want := range tc.wants {
			if !strings.Contains(view, want) {
				t.Errorf("%s: the form lacks %q:\n%s", tc.name, want, view)
			}
		}
	}
}

// TestYesNoKeys moves to the answer on y or n, so that n then enter says
// no to a question whose default is yes.
func TestYesNoKeys(t *testing.T) {
	t.Parallel()
	themed := &look{styles: style.New(true), profile: colorprofile.TrueColor, width: maxWidth}
	asker := terminalAsker{look: func() *look { return themed }, out: &bytes.Buffer{}}
	for _, tc := range []struct {
		keys        []string
		value, want string
	}{
		{[]string{"n"}, component.AnswerYes, component.AnswerNo},
		{[]string{"N"}, component.AnswerYes, component.AnswerNo},
		{[]string{"y"}, component.AnswerNo, component.AnswerYes},
		{nil, component.AnswerYes, component.AnswerYes},
	} {
		answer := tc.value
		form := asker.yesNoForm(&answer, "Save these settings?")
		_ = form.Init()
		_, _ = form.Update(tea.WindowSizeMsg{Width: maxWidth, Height: 24})
		for _, text := range tc.keys {
			_, _ = form.Update(tea.KeyPressMsg{Code: rune(text[0]), Text: text})
		}
		_, _ = form.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if answer != tc.want {
			t.Errorf("keys %q then enter from %s: answer %s, want %s", tc.keys, tc.value, answer, tc.want)
		}
	}
}

// TestSectionHeading writes the TUI's title of a part of setup and what it
// is for, wrapped to the look's width, and nothing for a part the TUI has
// no title for.
func TestSectionHeading(t *testing.T) {
	t.Parallel()
	const width = 40
	plain := &look{styles: style.New(true), profile: colorprofile.NoTTY, width: width}
	out := &bytes.Buffer{}
	asker := terminalAsker{look: func() *look { return plain }, out: out}
	asker.Section(sandbox.SectionZone)
	got := out.String()
	if !strings.HasPrefix(got, "\nZone and user\nA zone is a Keycard workspace") || !strings.HasSuffix(got, "\n\n") {
		t.Errorf("heading = %q, want the zone's title and what it is for", got)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(got), "\n") {
		if len(line) > width {
			t.Errorf("line %q is wider than %d", line, width)
		}
	}
	if got := plain.heading("no-such-part"); got != "" {
		t.Errorf("heading of an unknown part = %q, want none", got)
	}
}
