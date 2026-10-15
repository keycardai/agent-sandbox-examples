package page

import (
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
)

// logPane draws the log that shows, under head, with its follow, wrap and
// filter state on the line before it, or waiting while it has no line. A
// pane with focus gets the iris border.
func (s *Shared) logPane(title string, head []string, waiting string, width, height int, focused bool) string {
	lines := []string{s.logFlags()}
	if s.Logs.Searching() {
		lines[0] = s.Logs.SearchView()
	}
	lines = append(head, lines...)
	if log := s.Logs.Current(); log == nil || log.Len() == 0 {
		lines = append(lines, s.Styles.Dim.Render(waiting))
	} else {
		lines = append(lines, strings.Split(s.Logs.View(), "\n")...)
	}
	return component.Box(s.Styles, title, lines, width, height, focused)
}

// logFlags says whether the log pane follows its log, wraps it and filters
// it. A response is not a log to follow, so it is a body.
func (s *Shared) logFlags() string {
	theme := s.Styles
	flags := []string{}
	switch {
	case s.Logs.Source() == SourceResponse:
		flags = append(flags, theme.Dim.Render("Body"))
	case s.Logs.Following():
		flags = append(flags, theme.Running.Render("● following"))
	default:
		flags = append(flags, theme.Dim.Render("○ paused, G follows"))
	}
	if s.Logs.Wrapping() {
		flags = append(flags, theme.Dim.Render("wrap"))
	}
	if filter := s.Logs.Filter(); filter != "" {
		flags = append(flags, theme.Accent.Render("filter "+strconv.Quote(filter)))
	}
	return strings.Join(flags, theme.Faint.Render("  "))
}

// paint colors the lines of the log named source: the proxy's decisions by
// verdict, the activity by mark, and a container's log by level.
func (s *Shared) paint(source string, lines []string) []string {
	switch {
	case source == SourceTraffic, source == SourceTrafficAll:
		return s.colorTraffic(lines)
	case source == SourceActivity:
		return s.colorActivity(lines)
	case strings.HasPrefix(source, "container "):
		return s.colorLog(lines)
	}
	return lines
}

// colorTraffic colors each traffic row by its verdict.
func (s *Shared) colorTraffic(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i], _ = s.trafficLine(line)
	}
	return out
}

// trafficLine colors one traffic row by its verdict, and reports whether it
// was one.
func (s *Shared) trafficLine(line string) (string, bool) {
	when, rest, _ := strings.Cut(line, "  ")
	switch {
	case strings.HasPrefix(rest, "✕ deny"):
		return s.Styles.Dim.Render(when) + "  " + s.Styles.Failed.Render(rest), true
	case strings.HasPrefix(rest, "✓ allow"):
		return s.Styles.Dim.Render(when) + "  " + s.Styles.Allow.Render("✓ allow") +
			s.Styles.Text.Render(strings.TrimPrefix(rest, "✓ allow")), true
	}
	return line, false
}

// colorLog colors a container's log as proxylog.ReadableLine wrote it: a
// proxy decision by its verdict, and a structured line by its level, with
// its fields dimmed.
func (s *Shared) colorLog(lines []string) []string {
	theme := s.Styles
	levels := map[string]lipgloss.Style{
		"DEBUG": theme.Faint, "INFO": theme.Dim, "WARN": theme.Warn, "ERROR": theme.Failed,
	}
	width := proxylog.LevelWidth
	out := make([]string, len(lines))
	for i, line := range lines {
		if colored, ok := s.trafficLine(line); ok {
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

// isClock reports whether text is a time as proxylog.ReadableLine prints
// it.
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
	case SourceTraffic:
		return "No requests through the proxy yet. Each one shows here as the agent makes it."
	case SourceActivity:
		return "Nothing has happened yet."
	case SourceAction:
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
	case SourceAction:
		return "Last action"
	case SourceTraffic:
		if name == "dns" {
			return "Every decision of the egress proxy, with DNS lookups"
		}
		return "Every request through the egress proxy"
	case SourceActivity:
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
