package page

import (
	"cmp"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
)

// labelWidth is the width of a setting's label.
const labelWidth = 18

// section is one part of the settings the settings screen lists.
type section struct {
	id, title, about string
}

// Settings lists the settings to change one, with the problems they have.
type Settings struct {
	view

	cursor int
}

// NewSettings returns the settings screen.
func NewSettings(ui *UI, sandbox *state.Sandbox) *Settings {
	return &Settings{ui: ui, state: sandbox}
}

// Move moves the cursor on up or down, and reports whether msg was one.
func (s *Settings) Move(msg tea.KeyPressMsg) bool {
	switch {
	case key.Matches(msg, s.ui.Keys.Up):
		s.cursor = max(s.cursor-1, 0)
	case key.Matches(msg, s.ui.Keys.Down):
		s.cursor = min(s.cursor+1, len(sections())-1)
	default:
		return false
	}
	return true
}

// Selected names the section under the cursor.
func (s *Settings) Selected() string { return sections()[s.cursor].id }

// SelectDemo puts the cursor on the demo, the setting changed most.
func (s *Settings) SelectDemo() {
	s.cursor = slices.IndexFunc(sections(), func(sec section) bool { return sec.id == sandbox.SectionScenario })
}

// View lists the settings, with the one under the cursor ready to change,
// and the problems the settings have. back names where esc goes.
func (s *Settings) View(back string) string {
	theme := s.ui.Styles
	width := min(s.ui.Width-component.BorderCols-component.PageIndent, readWidth+checkNameWidth)
	rows := make([]string, 0, len(sections())*half)
	for i, sec := range sections() {
		value := ansi.Truncate(cmp.Or(s.state.Describe[sec.id], "not set"), max(width-labelWidth-half, 1), "…")
		mark, label := "  ", lipgloss.NewStyle().Width(labelWidth).Render(theme.Text.Render(sec.title))
		line := mark + label + theme.Muted.Render(value)
		if i == s.cursor {
			line = component.Selected(theme, theme.Accent.Render("▸ ")+label+value, true, true, width)
		}
		rows = append(rows, line, "  "+lipgloss.NewStyle().PaddingLeft(labelWidth).Render(theme.Dim.Render(sec.about)))
	}
	parts := []string{
		theme.Title.Render("Settings"), theme.Muted.Width(width).Render("Pick a setting to change it. " + s.savedWhere()),
		theme.Muted.Render("After a change, u brings the sandbox up again with it."), "", strings.Join(rows, "\n"), "",
	}
	if failed := s.setupFailure(width); failed != "" {
		parts = append(parts, failed, "")
	}
	if len(s.state.Problems) > 0 {
		parts = append(parts, s.problemList(width))
	} else {
		parts = append(parts, theme.OK.Render("✓ ")+theme.Text.Render("Ready to launch."))
	}
	hints := []component.Hint{
		{Key: component.KeyEnter, Desc: "change"},
		{Key: "a", Desc: "change everything"},
		{Key: "u", Desc: "launch"},
	}
	if s.state.Saving().Saved {
		hints = append(hints, component.Hint{Key: "F", Desc: "forget saved"})
	}
	return s.ui.Page("settings", lipgloss.JoinVertical(lipgloss.Left, parts...),
		append(hints, component.Hint{Key: component.KeyEsc, Desc: back}, component.Hint{Key: "q", Desc: wordQuit}))
}

// savedWhere says where the settings are saved, if they are.
func (s *Settings) savedWhere() string {
	saving := s.state.Saving()
	keychain := ""
	if err := s.state.KeychainErr; err != nil {
		keychain = " The keychain cannot keep the secrets: " + component.FirstLine(err.Error()) + "."
	}
	switch {
	case saving.Saved && saving.CanSave:
		return "They are saved in " + sandbox.HomeRelative(saving.File) + ", with the secrets in " + saving.Keychain + "."
	case saving.Saved:
		return "They are saved in " + sandbox.HomeRelative(saving.File) + ", and changes last until you quit." + keychain
	case !saving.CanSave:
		return "They are not saved, and last until you quit." + keychain
	}
	return "They are not saved, and last until you quit. Changing one asks whether to save them."
}

// sections are the sections the settings list: those setup can ask on
// their own.
func sections() []section {
	var list []section
	for _, info := range sandbox.Sections() {
		if info.Change != "" {
			list = append(list, section{info.ID, info.Title, info.Short})
		}
	}
	return list
}
