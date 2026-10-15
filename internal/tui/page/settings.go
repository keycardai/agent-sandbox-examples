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
)

// labelWidth is the width of a setting's label.
const labelWidth = 18

// section is one part of the settings the settings screen lists.
type section struct {
	id, title, about string
}

// Settings lists the settings to change one, with the problems they have.
type Settings struct {
	shared *Shared
	cursor int
}

// NewSettings returns the settings screen.
func NewSettings(shared *Shared) *Settings { return &Settings{shared: shared} }

// Move moves the cursor on up or down, and reports whether msg was one.
func (s *Settings) Move(msg tea.KeyPressMsg) bool {
	switch {
	case key.Matches(msg, s.shared.Keys.Up):
		s.cursor = max(s.cursor-1, 0)
	case key.Matches(msg, s.shared.Keys.Down):
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
	shared := s.shared
	theme := shared.Styles
	width := min(shared.Width-component.BorderCols-component.PageIndent, readWidth+checkNameWidth)
	rows := make([]string, 0, len(sections())*half)
	for i, sec := range sections() {
		value := ansi.Truncate(cmp.Or(shared.Describe[sec.id], "not set"), max(width-labelWidth-half, 1), "…")
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
	if failed := shared.setupFailure(width); failed != "" {
		parts = append(parts, failed, "")
	}
	if len(shared.Problems) > 0 {
		parts = append(parts, shared.problemList(width))
	} else {
		parts = append(parts, theme.OK.Render("✓ ")+theme.Text.Render("Ready to launch."))
	}
	hints := []component.Hint{
		{Key: component.KeyEnter, Desc: "change"},
		{Key: "a", Desc: "change everything"},
		{Key: "u", Desc: "launch"},
	}
	if shared.Session.Local().Store.Saved() {
		hints = append(hints, component.Hint{Key: "F", Desc: "forget saved"})
	}
	return shared.Page("settings", lipgloss.JoinVertical(lipgloss.Left, parts...),
		append(hints, component.Hint{Key: component.KeyEsc, Desc: back}, component.Hint{Key: "q", Desc: wordQuit}))
}

// savedWhere says where the settings are saved, if they are.
func (s *Settings) savedWhere() string {
	store := s.shared.Session.Local().Store
	if store.Saved() {
		return "They are saved in " + sandbox.HomeRelative(store.Path()) + ", with the secrets in " +
			store.KeychainWhere() + "."
	}
	if !store.CanSave() {
		return "They are not saved, and last until you quit."
	}
	return "They are not saved, and last until you quit. Changing one asks whether to save them."
}

func sections() []section {
	return []section{
		{sandbox.SectionAccount, "Keycard account", "the service account that sets up the zone"},
		{sandbox.SectionZone, "Zone and user", "the zone, and who the agent acts as"},
		{sandbox.SectionScenario, "Demo", "what the agent does, and its credentials"},
		{sandbox.SectionCluster, wordCluster, "where the sandbox runs, and how Keycard reaches it"},
	}
}
