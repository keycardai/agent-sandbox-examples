package page

import (
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
)

// logPane draws the shown log under head, or waiting while it has no line.
func (u *UI) logPane(title string, head []string, waiting string, width, height int, focused bool) string {
	lines := []string{u.logFlags()}
	if u.Logs.Searching() {
		lines[0] = u.Logs.SearchView()
	}
	lines = append(head, lines...)
	if log := u.Logs.Current(); log == nil || log.Len() == 0 {
		lines = append(lines, u.Styles.Dim.Render(waiting))
	} else {
		lines = append(lines, strings.Split(u.Logs.View(), "\n")...)
	}
	return component.Box(u.Styles, title, lines, width, height, focused)
}

// logFlags says whether the log pane follows, wraps and filters its log.
func (u *UI) logFlags() string {
	theme := u.Styles
	flags := []string{}
	switch {
	case u.Logs.Source() == state.SourceResponse:
		flags = append(flags, theme.Dim.Render("Body"))
	case u.Logs.Following():
		flags = append(flags, theme.Running.Render("● following"))
	default:
		flags = append(flags, theme.Dim.Render("○ paused, G follows"))
	}
	if u.Logs.Wrapping() {
		flags = append(flags, theme.Dim.Render("wrap"))
	}
	if filter := u.Logs.Filter(); filter != "" {
		flags = append(flags, theme.Accent.Render("filter "+strconv.Quote(filter)))
	}
	return strings.Join(flags, theme.Faint.Render("  "))
}

// paint colors the lines of the log named source.
func (u *UI) paint(source string, lines []string) []string {
	switch {
	case source == state.SourceTraffic, source == state.SourceTrafficAll:
		return u.colorTraffic(lines)
	case source == state.SourceActivity:
		return u.colorActivity(lines)
	case strings.HasPrefix(source, "container "):
		return u.colorLog(lines)
	}
	return lines
}

// colorTraffic colors each traffic row by its verdict.
func (u *UI) colorTraffic(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i], _ = u.trafficLine(line)
	}
	return out
}

// trafficLine colors one traffic row by its verdict, and reports whether it
// was one.
func (u *UI) trafficLine(line string) (string, bool) {
	when, rest, _ := strings.Cut(line, "  ")
	switch {
	case strings.HasPrefix(rest, "✕ deny"):
		return u.Styles.Dim.Render(when) + "  " + u.Styles.Failed.Render(rest), true
	case strings.HasPrefix(rest, "✓ allow"):
		return u.Styles.Dim.Render(when) + "  " + u.Styles.Allow.Render("✓ allow") +
			u.Styles.Text.Render(strings.TrimPrefix(rest, "✓ allow")), true
	}
	return line, false
}

// colorLog colors a container's log as proxylog.ReadableLine wrote it.
func (u *UI) colorLog(lines []string) []string {
	theme := u.Styles
	levels := map[string]lipgloss.Style{
		"DEBUG": theme.Faint, "INFO": theme.Dim, "WARN": theme.Warn, "ERROR": theme.Failed,
	}
	width := proxylog.LevelWidth
	out := make([]string, len(lines))
	for i, line := range lines {
		if colored, ok := u.trafficLine(line); ok {
			out[i] = colored
			continue
		}
		when, rest, _ := strings.Cut(line, "  ")
		if len(rest) <= width || !isClock(when) {
			out[i] = line
			continue
		}
		level := strings.TrimSpace(rest[:width])
		levelStyle, known := levels[level]
		if !known {
			out[i] = line
			continue
		}
		message, fields, _ := strings.Cut(rest[width+1:], "  ")
		text := theme.Text
		if level == "WARN" || level == "ERROR" {
			text = levelStyle
		}
		out[i] = theme.Dim.Render(when) + "  " + levelStyle.Render(rest[:width]) + " " + text.Render(message) +
			"  " + theme.Dim.Render(fields)
	}
	return out
}

// isClock reports whether text is a clock time from proxylog.ReadableLine.
func isClock(text string) bool {
	return len(text) == len(time.TimeOnly) && text[2] == ':' && text[5] == ':'
}

// waitingText is what the log pane shows before a log has a line.
func waitingText(source string) string {
	switch kind, _, _ := strings.Cut(source, " "); kind {
	case "container":
		return "Waiting for the container's log…"
	case "service":
		return "The service has not written anything yet."
	case "step":
		return "This step has not run in this session. u brings the sandbox up, and runs it."
	case state.SourceTraffic:
		return "No requests through the proxy yet. Each one shows here as the agent makes it."
	case state.SourceActivity:
		return "Nothing has happened yet."
	case state.SourceAction:
		return "No action has run in this session."
	}
	return "Nothing to show yet."
}

// sourceTitle names a log for the log pane's title.
func sourceTitle(source string) string {
	kind, name, _ := strings.Cut(source, " ")
	switch kind {
	case "step":
		return "Step · " + name
	case "service":
		return "Service log · " + name
	case "container":
		return "Container log · " + name
	case state.SourceAction:
		return "Last action"
	case state.SourceTraffic:
		if name == "dns" {
			return "Every decision of the egress proxy, with DNS lookups"
		}
		return "Every request through the egress proxy"
	case state.SourceActivity:
		return "Activity · what happened, newest last"
	}
	return "Output"
}

// logHints lists the keys of a log pane with focus.
func logHints() []component.Hint {
	return []component.Hint{
		{Key: component.KeyEsc, Desc: "back"},
		{Key: "f", Desc: "follow"},
		{Key: "w", Desc: "wrap"},
		{Key: "/", Desc: "filter"},
		{Key: "G", Desc: "end"},
		{Key: "?", Desc: wordKeys},
	}
}
