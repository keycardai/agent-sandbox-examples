package page

import (
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
)

const (
	// maxCaptionRows bounds the words over a log.
	maxCaptionRows = 3
	// logsChrome is the rows the list of logs takes besides its logs.
	logsChrome = 10
	// bringUpPrefix names a bring-up step's log in the list.
	bringUpPrefix = "Bring-up: "
)

// LogEntry is one log the list offers: its name, the log, the pod's
// container to follow for it, and what it shows.
type LogEntry struct {
	Name, Source, Container, Caption string
}

// Logs lists every log the sandbox has, for looking into a problem, and
// shows the one chosen over the screen.
type Logs struct {
	view

	picker component.Picker
	// Shown is the log chosen from the list.
	Shown LogEntry
}

// NewLogs returns the list of logs.
func NewLogs(ui *UI, sandbox *state.Sandbox) *Logs { return &Logs{ui: ui, state: sandbox} }

// Entries are every log the sandbox has: the pod's containers, the
// programs on this machine, each step of the bring-up, and what happened in
// this session, last.
func (l *Logs) Entries() []LogEntry {
	entries := []LogEntry{
		{"Agent", "", state.AgentContainer, l.agentCaption()},
		{"Egress proxy", "", state.ProxyContainer, "The egress proxy's own log: every decision, with every DNS lookup."},
		{"Firewall", "", state.NetsetupContainer, "The rules the firewall set when the pod started. They send each " +
			"DNS lookup and web request from the agent to the proxy, and drop everything else."},
	}
	if l.state.Run.StepOf(sandbox.StepMirror) != nil {
		entries = append(entries, LogEntry{
			"Mirror", state.ServiceSource(sandbox.ServiceMirror), "",
			"The mirror on this machine serves the cluster's public keys.",
		})
	}
	if l.state.Run.StepOf(sandbox.StepTunnel) != nil {
		entries = append(entries, LogEntry{
			"Tunnel", state.ServiceSource(sandbox.ServiceTunnel), "",
			"The Cloudflare tunnel puts the mirror on the internet, for Keycard.",
		})
	}
	for _, row := range l.state.Run.Steps {
		entries = append(entries, LogEntry{
			bringUpPrefix + row.Step.Title, state.StepSource(row.Step.ID), "", "What this step of the bring-up printed.",
		})
	}
	return append(entries,
		LogEntry{"Last action", state.SourceAction, "", "What the last action printed, such as a restart of the pod."},
		LogEntry{"Activity", state.SourceActivity, "", "What happened in this session, newest last."})
}

// Open puts the list's cursor on the step that runs, in a bring-up or the
// checks, or on the one a bring-up stopped at, or else keeps it on a log,
// since the list can be shorter than when it last showed.
func (l *Logs) Open() {
	entries := l.Entries()
	step := l.state.Run.FailedStep()
	if l.state.Run.Job.RunsSteps() && l.state.Run.Cursor < len(l.state.Run.Steps) {
		step = l.state.Run.Steps[l.state.Run.Cursor]
	}
	if step != nil {
		if i := slices.IndexFunc(entries, func(entry LogEntry) bool {
			return entry.Name == bringUpPrefix+step.Step.Title
		}); i >= 0 {
			l.picker.Cursor = i
		}
	}
	l.picker.Cursor = max(min(l.picker.Cursor, len(entries)-1), 0)
}

// Keys moves through the list, and returns the log chosen. l closes it.
func (l *Logs) Keys(msg tea.KeyPressMsg) (LogEntry, component.Pick) {
	entries := l.Entries()
	if key.Matches(msg, l.ui.Keys.Logs) {
		return LogEntry{}, component.PickClosed
	}
	pick := l.picker.Update(msg, l.ui.Keys, len(entries))
	if pick != component.PickChosen {
		return LogEntry{}, pick
	}
	return entries[l.picker.Cursor], pick
}

// ListView draws the list of every log.
func (l *Logs) ListView() string {
	theme := l.ui.Styles
	width := component.ModalInnerWidth(l.ui.Width)
	entries := l.Entries()
	lines := make([]string, 0, len(entries))
	for i, entry := range entries {
		mark := "  "
		if i == l.picker.Cursor {
			mark = theme.Accent.Render("▸ ")
		}
		lines = append(lines, component.Selected(theme, mark+theme.Text.Render(entry.Name), i == l.picker.Cursor, true,
			width))
	}
	return component.ListModal(theme, l.ui.Width, max(l.ui.Height-component.ChromeRows-logsChrome, 1),
		"Every log", "For looking into a problem.", lines, l.picker.Cursor,
		component.Hint{Key: "↑↓", Desc: wordChoose}, component.Hint{Key: component.KeyEnter, Desc: "show"},
		component.Hint{Key: component.KeyEsc, Desc: "close"})
}

// View draws the log chosen over the screen, under the band: what it
// shows, and the log.
func (l *Logs) View() string {
	width, height := l.size()
	return l.ui.logPane(l.Shown.Name+" · log", l.head(width), waitingText(l.ui.Logs.Source()), width, height, true)
}

// Hints lists the keys that move through the log.
func (l *Logs) Hints() []component.Hint { return logHints() }

// Layout sizes the log under what it shows.
func (l *Logs) Layout() {
	width, height := l.size()
	l.ui.Logs.SetSize(max(width-component.BorderCols, 1),
		max(height-component.BorderRows-len(l.head(width))-1, 1))
}

// size is the log's box: the window between the band and the footer.
func (l *Logs) size() (int, int) { return max(l.ui.Width, 1), max(l.ui.Height-half, 1) }

// head says what the log shows, wrapped to width.
func (l *Logs) head(width int) []string {
	theme := l.ui.Styles
	lines := strings.Split(lipgloss.NewStyle().Width(max(width-component.BorderCols-1, 1)).
		Render(theme.Muted.Render(l.Shown.Caption)), "\n")
	if len(lines) > maxCaptionRows {
		lines = lines[:maxCaptionRows]
		lines[maxCaptionRows-1] += theme.Muted.Render("…")
	}
	return lines
}

// agentCaption says what the agent's log shows, by how the demo runs.
func (l *Logs) agentCaption() string {
	scenario := l.state.LoadedDemo()
	if scenario == nil {
		return "What the agent printed."
	}
	switch scenario.Run.Mode {
	case sandbox.ModeShell:
		return "The agent's own output. It waits until you press a to open the demo. What the demo does " +
			"then shows on the Traffic tab."
	case sandbox.ModeBackground:
		return "The agent's own output. The demo runs on its own, and writes what it does here."
	}
	return "The agent's own output. It stays idle until you send it a request."
}
