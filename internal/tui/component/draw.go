// Package component draws the parts the TUI's screens share, and keeps the
// state of the ones that have any: a pane with a border, the log pane with
// every log, a list to choose from, the key hints, the header band and the
// footer.
package component

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

const (
	// BorderRows is what a pane's border takes down.
	BorderRows = 2
	// BorderCols is what a pane's border and its padding take across.
	BorderCols = 4
	// CornerCols is what a border's two corners take.
	CornerCols = 2
	// colorShift turns a 16-bit color channel into 8 bits.
	colorShift = 8
	// half halves a size, to center or split it.
	half = 2
)

// spinnerFrames turn while something runs.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"} //nolint:gochecknoglobals // Read only.

// Spinner is the spinner's frame for the frame count.
func Spinner(frame int) string { return spinnerFrames[frame%len(spinnerFrames)] }

// Box draws a pane with its title in the top border, cut to fit. A pane
// with focus gets a heavy iris border. width and height include the border. The panes
// have no numbers: the number keys switch tabs.
func Box(theme style.Styles, title string, lines []string, width, height int, focused bool) string {
	border, borderStyle, titleStyle := lipgloss.NormalBorder(), lipgloss.NewStyle().Foreground(theme.Colors.Line),
		theme.PaneTitle
	if focused {
		border, borderStyle, titleStyle = lipgloss.ThickBorder(),
			lipgloss.NewStyle().Foreground(theme.Colors.AccentFill), theme.PaneTitleFocused
	}
	inner := width - BorderCols
	head := ansi.Truncate(" "+title+" ", max(width-CornerCols-1, 0), "…")
	fill := max(width-CornerCols-1-lipgloss.Width(head), 0)
	out := make([]string, 0, height)
	out = append(out, borderStyle.Render(border.TopLeft+border.Top)+titleStyle.Render(head)+
		borderStyle.Render(strings.Repeat(border.Top, fill)+border.TopRight))
	for i := range max(height-BorderRows, 0) {
		line := ""
		if i < len(lines) {
			line = ansi.Truncate(lines[i], inner, "…")
		}
		pad := strings.Repeat(" ", max(inner-lipgloss.Width(line), 0))
		out = append(out, borderStyle.Render(border.Left)+" "+line+pad+" "+borderStyle.Render(border.Right))
	}
	out = append(out, borderStyle.Render(border.BottomLeft+strings.Repeat(border.Bottom, max(width-CornerCols, 0))+
		border.BottomRight))
	return strings.Join(out, "\n")
}

// Selected paints the row under the cursor, keeping its colors: the
// selection color in the pane with focus, and the band's in the others.
func Selected(theme style.Styles, line string, isCursor, paneFocused bool, width int) string {
	if !isCursor {
		return line
	}
	if paneFocused {
		return Highlight(line, theme.Colors.Selection, width)
	}
	return Highlight(line, theme.Colors.Band, width)
}

// Highlight puts bg behind a styled line across width. The background goes
// on at the start and again after each reset inside the line, so the
// line's own colors stay.
func Highlight(line string, bg color.Color, width int) string {
	red, green, blue, _ := bg.RGBA()
	on := fmt.Sprintf("\x1b[48;2;%d;%d;%dm", red>>colorShift, green>>colorShift, blue>>colorShift)
	line = strings.ReplaceAll(line, "\x1b[m", "\x1b[m"+on)
	line = strings.ReplaceAll(line, "\x1b[0m", "\x1b[0m"+on)
	return on + line + strings.Repeat(" ", max(width-lipgloss.Width(line), 0)) + "\x1b[m"
}

// Window returns the rows of lines that keep cursor in view.
func Window(lines []string, cursor, rows int) []string {
	if rows <= 0 || len(lines) <= rows {
		return lines
	}
	start := min(max(cursor-rows/half, 0), len(lines)-rows)
	return lines[start : start+rows]
}

// Fit pads or cuts lines to exactly rows lines.
func Fit(lines []string, rows int) []string {
	if len(lines) > rows {
		return lines[:rows]
	}
	for len(lines) < rows {
		lines = append(lines, "")
	}
	return lines
}

// Pad cuts or pads text to width cells.
func Pad(text string, width int) string {
	text = ansi.Truncate(text, width, "…")
	return text + strings.Repeat(" ", max(width-lipgloss.Width(text), 0))
}

// Duration prints d as 9s or 1m04s.
func Duration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%int(time.Minute.Seconds()))
}

// FirstLine returns the first line of text.
func FirstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// Plural counts n of word: 1 request, 2 requests.
func Plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
