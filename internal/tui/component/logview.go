package component

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// LogView keeps every log by name, and shows one of them in a viewport:
// from its end while it follows the log, wrapped or not, and filtered to
// the lines that hold a search. A log written whole, such as a response,
// shows from its first line instead.
type LogView struct {
	logs map[string]*Log
	// paint colors the lines of the log named source, and painted holds
	// each line of the log that shows as paint colored it.
	paint   func(source string, lines []string) []string
	painted map[string]string
	// fromTop names the logs that show from their first line.
	fromTop  map[string]bool
	viewport viewport.Model
	// source names the log that shows, and shown is its version that the
	// viewport holds.
	source    string
	shown     int
	follow    bool
	wrap      bool
	search    textinput.Model
	searching bool
	filter    string
}

// NewLogView returns a log view that colors each log's lines with paint.
// paint must color each line on its own, since the view colors a line once
// and keeps it. Its keys leave out the letters the TUI binds, such as u,
// which brings the sandbox up.
func NewLogView(paint func(source string, lines []string) []string) *LogView {
	search := textinput.New()
	search.Prompt = "/"
	search.Placeholder = "find in the log"
	output := viewport.New()
	bind := func(keys ...string) key.Binding { return key.NewBinding(key.WithKeys(keys...)) }
	output.KeyMap = viewport.KeyMap{
		Up:           bind("up", "k"),
		Down:         bind("down", "j"),
		PageUp:       bind("pgup", "b"),
		PageDown:     bind("pgdown", "space"),
		HalfPageUp:   bind("ctrl+u"),
		HalfPageDown: bind("ctrl+d"),
		Left:         bind("left"),
		Right:        bind("right"),
	}
	return &LogView{
		logs: map[string]*Log{}, paint: paint, fromTop: map[string]bool{}, viewport: output, follow: true,
		search: search,
	}
}

// FromTop has the log named name show from its first line, when it shows
// and each time it is written again, and not follow its end. It is for a
// log written whole, such as a response.
func (v *LogView) FromTop(name string) { v.fromTop[name] = true }

// Log returns the log named name, and makes it when it is new.
func (v *LogView) Log(name string) *Log {
	if log, ok := v.logs[name]; ok {
		return log
	}
	log := &Log{}
	v.logs[name] = log
	return log
}

// Current returns the log that shows, or nil when there is none by its
// name.
func (v *LogView) Current() *Log { return v.logs[v.source] }

// Source names the log that shows.
func (v *LogView) Source() string { return v.source }

// Show shows the log named name, from its end, or from its first line for
// a log that shows from the top.
func (v *LogView) Show(name string) {
	if v.source == name {
		return
	}
	v.source, v.follow, v.painted = name, !v.fromTop[name], nil
	v.Refresh(true)
	if !v.follow {
		v.viewport.GotoTop()
	}
}

// Refresh draws the log that shows again when it changed, or when force.
func (v *LogView) Refresh(force bool) {
	log := v.logs[v.source]
	if log == nil {
		v.viewport.SetContent("")
		return
	}
	if !force && log.Version() == v.shown {
		return
	}
	lines, version := log.Snapshot()
	written := version != v.shown
	v.shown = version
	if v.filter != "" {
		lines = filterLines(lines, v.filter)
	}
	v.viewport.SetContentLines(v.paintLines(lines))
	switch {
	case written && v.fromTop[v.source]:
		v.follow = false
		v.viewport.GotoTop()
	case v.follow:
		v.viewport.GotoBottom()
	}
}

// SetSize sizes the viewport, and draws the log again.
func (v *LogView) SetSize(width, height int) {
	v.viewport.SetWidth(width)
	v.viewport.SetHeight(height)
	v.Refresh(true)
}

// Keys scrolls the log, and turns follow, wrap and search.
func (v *LogView) Keys(msg tea.KeyPressMsg, keys KeyMap) tea.Cmd {
	switch {
	case key.Matches(msg, keys.Follow):
		v.follow = !v.follow
		if v.follow {
			v.viewport.GotoBottom()
		}
	case key.Matches(msg, keys.Wrap):
		v.wrap = !v.wrap
		v.viewport.SoftWrap = v.wrap
		v.Refresh(true)
	case key.Matches(msg, keys.Top):
		v.follow = false
		v.viewport.GotoTop()
	case key.Matches(msg, keys.Bottom):
		v.follow = true
		v.viewport.GotoBottom()
	case key.Matches(msg, keys.Search):
		v.searching = true
		v.search.SetValue(v.filter)
		return v.search.Focus()
	case key.Matches(msg, keys.Back):
		v.filter = ""
		v.Refresh(true)
	default:
		var cmd tea.Cmd
		v.viewport, cmd = v.viewport.Update(msg)
		v.follow = v.viewport.AtBottom()
		return cmd
	}
	return nil
}

// Searching reports whether the search takes the keys.
func (v *LogView) Searching() bool { return v.searching }

// SearchKeys edits the filter. Enter keeps it, and esc clears it.
func (v *LogView) SearchKeys(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case KeyEnter:
		v.searching, v.filter = false, v.search.Value()
		v.search.Blur()
		v.Refresh(true)
		return nil
	case KeyEsc:
		v.searching, v.filter = false, ""
		v.search.Blur()
		v.Refresh(true)
		return nil
	}
	var cmd tea.Cmd
	v.search, cmd = v.search.Update(msg)
	return cmd
}

// Wheel scrolls the log with the mouse wheel.
func (v *LogView) Wheel(msg tea.MouseWheelMsg) {
	v.viewport, _ = v.viewport.Update(msg)
	v.follow = v.viewport.AtBottom()
}

// Following reports whether the log shows from its end as it grows.
func (v *LogView) Following() bool { return v.follow }

// Wrapping reports whether long lines wrap.
func (v *LogView) Wrapping() bool { return v.wrap }

// Filter is the search the lines shown hold, or "".
func (v *LogView) Filter() string { return v.filter }

// ClearFilter drops the search. The lines it hid show again at the next
// Refresh(true).
func (v *LogView) ClearFilter() { v.filter = "" }

// SearchView draws the search as it is typed.
func (v *LogView) SearchView() string { return v.search.View() }

// View draws the viewport.
func (v *LogView) View() string { return v.viewport.View() }

// paintLines colors lines. It paints only the lines it has not painted
// for this log before, and keeps the colored lines for the next refresh.
func (v *LogView) paintLines(lines []string) []string {
	var fresh []string
	for _, line := range lines {
		if _, ok := v.painted[line]; !ok {
			fresh = append(fresh, line)
		}
	}
	painted := make(map[string]string, len(lines))
	for i, line := range v.paint(v.source, fresh) {
		painted[fresh[i]] = line
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		text, ok := painted[line]
		if !ok {
			text = v.painted[line]
			painted[line] = text
		}
		out[i] = text
	}
	v.painted = painted
	return out
}

// filterLines keeps the lines that hold query, in any case.
func filterLines(lines []string, query string) []string {
	query = strings.ToLower(query)
	var kept []string
	for _, line := range lines {
		if strings.Contains(strings.ToLower(ansi.Strip(line)), query) {
			kept = append(kept, line)
		}
	}
	return kept
}
