package component

import (
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

const (
	// PageIndent is the space left of a page's body.
	PageIndent = 2
	// ChromeRows is what the header and the footer take.
	ChromeRows = 2
	// MinWidth is the narrowest window the TUI draws in.
	MinWidth = 80
	// MinHeight is the shortest window the TUI draws in.
	MinHeight = 22
	// modalMargin and modalMaxWidth bound a modal's width.
	modalMargin, modalMaxWidth = 8, 92
)

// Toast is a notice in the footer, which goes away after a while.
type Toast struct {
	Text  string
	Err   bool
	Until time.Time
}

// Screen draws a full screen: header over the body over the footer. The
// body is indented, and cut to the window.
func Screen(width, height int, header, body, footer string) string {
	// Each line is indented on its own, rather than padded to the widest
	// line, so that only a line that is too long is cut.
	indent := strings.Repeat(" ", PageIndent)
	lines := Fit(append([]string{""}, strings.Split(body, "\n")...), max(height-ChromeRows, 1))
	for i, line := range lines {
		if line != "" {
			lines[i] = ansi.Truncate(indent+line, width, "…")
		}
	}
	return strings.Join(append(append([]string{header}, lines...), footer), "\n")
}

// Band draws the header band: the wordmark, then middle, and right at the
// right edge, on the band's color across the window.
func Band(theme style.Styles, width int, middle, right string) string {
	gap := theme.Band.Render("   ")
	left := theme.Band.Render(" ") + theme.Wordmark() + gap + middle
	space := max(width-lipgloss.Width(left)-lipgloss.Width(right)-1, 1)
	line := left + theme.Band.Render(strings.Repeat(" ", space)) + right + theme.Band.Render(" ")
	return ansi.Truncate(line, width, "…")
}

// Tabs draws the tabs for the header band, with the one at active marked.
// Each tab's key is its number.
func Tabs(theme style.Styles, names []string, active int) string {
	parts := make([]string, 0, len(names))
	for i, name := range names {
		key := strconv.Itoa(i + 1)
		if i == active {
			parts = append(parts, theme.BandTabOn.Render(" "+key+" "+name+" "))
			continue
		}
		parts = append(parts, theme.Band.Render(" ")+theme.BandKey.Render(key)+theme.BandValue.Render(" "+name+" "))
	}
	return strings.Join(parts, theme.Band.Render(" "))
}

// Footer draws the keys as chips, and the toast on the right while it
// lasts. A toast that does not fit takes the keys' place.
func Footer(theme style.Styles, width int, hints []Hint, toast Toast) string {
	parts := make([]string, 0, len(hints))
	for _, hint := range hints {
		parts = append(parts, theme.KeyChip.Render(hint.Key)+" "+theme.KeyDesc.Render(hint.Desc))
	}
	keys := " " + strings.Join(parts, "  ")
	note := ""
	if time.Now().Before(toast.Until) {
		noteStyle := theme.Toast
		if toast.Err {
			noteStyle = theme.ToastError
		}
		note = noteStyle.Render(toast.Text) + " "
	}
	if lipgloss.Width(keys)+lipgloss.Width(note) > width {
		if lipgloss.Width(note) > width/half {
			keys = ""
		} else {
			keys = ansi.Truncate(keys, width-lipgloss.Width(note)-1, "…")
		}
	}
	gap := strings.Repeat(" ", max(width-lipgloss.Width(keys)-lipgloss.Width(note), 0))
	return ansi.Truncate(keys+gap+note, width, "…")
}

// Chips draws a modal's keys as chips.
func Chips(theme style.Styles, hints ...Hint) string {
	parts := make([]string, 0, len(hints))
	for _, hint := range hints {
		parts = append(parts, theme.KeyChip.Render(hint.Key)+" "+theme.KeyDesc.Render(hint.Desc))
	}
	return strings.Join(parts, "   ")
}

// ModalWidth is a modal's width in a window width wide.
func ModalWidth(width int) int {
	return max(min(width-modalMargin, modalMaxWidth), 1)
}

// ModalInnerWidth is what a modal has for its content, inside its border
// and padding.
func ModalInnerWidth(width int) int {
	return max(ModalWidth(width)-style.ModalPadX*half-CornerCols, 1)
}

// Overlay draws modal in the middle of base, a screen width by height.
func Overlay(base, modal string, width, height int) string {
	x := max((width-lipgloss.Width(modal))/half, 0)
	y := max((height-lipgloss.Height(modal))/half, 0)
	return lipgloss.NewCompositor(
		lipgloss.NewLayer(base),
		lipgloss.NewLayer(modal).X(x).Y(y).Z(1),
	).Render()
}
