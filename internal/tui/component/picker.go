package component

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

// Pick is what a key did to a Picker.
type Pick int

const (
	// PickNone moved the cursor, or did nothing.
	PickNone Pick = iota
	// PickChosen chose the item under the cursor.
	PickChosen
	// PickClosed closed the list without a choice.
	PickClosed
)

// Picker is a list in a modal to choose one item from: the arrows move the
// cursor, enter chooses, and esc or q closes the list.
type Picker struct {
	Cursor int
}

// Update moves the cursor through count items, and says whether msg chose
// the item under it or closed the list. The cursor stays an index of the
// items, and 0 when there are none. An empty list chooses nothing.
func (p *Picker) Update(msg tea.KeyPressMsg, keys KeyMap, count int) Pick {
	p.Cursor = max(min(p.Cursor, count-1), 0)
	switch {
	case key.Matches(msg, keys.Up):
		p.Cursor = max(p.Cursor-1, 0)
	case key.Matches(msg, keys.Down):
		p.Cursor = max(min(p.Cursor+1, count-1), 0)
	case msg.String() == KeyEnter && count > 0:
		return PickChosen
	case key.Matches(msg, keys.Back), msg.String() == "q":
		return PickClosed
	}
	return PickNone
}

// ListModal draws a list to choose from in a modal for a window width
// wide: its title, what it is for, as many of lines as rows holds around
// the cursor, and the keys.
func ListModal(theme style.Styles, width, rows int, title, about string, lines []string, cursor int,
	hints ...Hint,
) string {
	parts := make([]string, 0, len(lines)+listChrome)
	parts = append(parts, theme.ModalTitle.Render(title), theme.Muted.Render(about), "")
	parts = append(parts, Window(lines, cursor, rows)...)
	parts = append(parts, "", Chips(theme, hints...))
	return theme.Modal.Width(ModalWidth(width)).Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

// listChrome is the parts a list's modal has besides its lines.
const listChrome = 5
