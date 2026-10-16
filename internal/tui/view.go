package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

const (
	percent = 100
	// progressCells is the width of the bring-up's progress bar.
	progressCells = 12
	// bandSpace is what the band puts around and between its parts.
	bandSpace = 6
	// helpTitleWidth is the width of a group's title in the help, and
	// helpGap the space between its keys.
	helpTitleWidth, helpGap = 9, 3
)

// View draws the TUI.
func (m *model) View() tea.View {
	content := ""
	switch {
	case m.ui.Width == 0 || m.ui.Height == 0:
	case m.ui.Width < component.MinWidth || m.ui.Height < component.MinHeight:
		content = lipgloss.Place(m.ui.Width, m.ui.Height, lipgloss.Center, lipgloss.Center,
			m.ui.Styles.Muted.Render(fmt.Sprintf(
				"The window is too small for %s.\nIt needs %d×%d, and it is %d×%d.",
				style.AppName, component.MinWidth, component.MinHeight, m.ui.Width, m.ui.Height,
			)))
	default:
		content = m.withOverlay(m.screenLayer().view())
	}
	view := tea.NewView(content)
	view.AltScreen = true
	if m.mouse {
		view.MouseMode = tea.MouseModeCellMotion
	}
	view.WindowTitle = "Keycard sandbox · " + ansi.Strip(m.statusBadge())
	view.ProgressBar = m.progress()
	return view
}

// dashboardView draws the tab that shows, under the band with the tabs and
// over the footer with the tab's keys.
func (m *model) dashboardView() string {
	var body string
	var hints []component.Hint
	switch m.tab {
	case tabAgent:
		body, hints = m.agents.View(), m.agents.Hints()
	case tabTraffic:
		body, hints = m.traffic.View(), m.traffic.Hints()
	case tabHome, tabCount:
		body, hints = m.home.View(), m.home.Hints()
	}
	if m.overlay.kind == overlayLog {
		hints = m.logs.Hints()
	}
	return m.ui.PageUnder(m.tabBand(), body, hints)
}

// tabBand draws the header band with the tabs, the one that shows marked,
// and how the sandbox is on the right.
func (m *model) tabBand() string {
	theme := m.ui.Styles
	tabs := component.Tabs(theme, []string{"Home", "Agents", "Traffic"}, int(m.tab))
	badge := m.statusBadge()
	if lipgloss.Width(theme.Wordmark())+lipgloss.Width(tabs)+lipgloss.Width(badge)+bandSpace > m.ui.Width {
		badge = m.shortBadge()
	}
	return component.Band(theme, m.ui.Width, tabs, badge)
}

// statusBadge says how the sandbox is, for the header band.
func (m *model) statusBadge() string {
	theme := m.ui.Styles
	switch phase, row := m.state.Phase(); phase {
	case state.PhaseBringingUp:
		done := m.state.Run.StepsDone()
		return theme.BandBusy.Render(component.Spinner(m.ui.Frame)+" bringing up ") +
			m.progressBar(done, len(m.state.Run.Steps)) + theme.BandLabel.Render(" "+strconv.Itoa(done)+"/"+
			strconv.Itoa(len(m.state.Run.Steps)))
	case state.PhaseChecking:
		return theme.BandBusy.Render(component.Spinner(m.ui.Frame) + " running the checks")
	case state.PhaseStopping:
		return theme.BandBusy.Render(component.Spinner(m.ui.Frame) + " stopping")
	case state.PhaseStopped:
		return theme.BandDown.Render("○ stopped at " + row.Step.Title)
	case state.PhaseFailed:
		return theme.BandFail.Render("✕ stopped at " + row.Step.Title)
	case state.PhaseReading:
		return theme.BandBusy.Render(component.Spinner(m.ui.Frame) + " reading")
	case state.PhaseUp:
		return theme.BandUp.Render("● up")
	case state.PhaseDown:
	}
	return theme.BandDown.Render("○ down")
}

// shortBadge is the status badge for a narrow band.
func (m *model) shortBadge() string {
	theme := m.ui.Styles
	switch phase, _ := m.state.Phase(); phase {
	case state.PhaseBringingUp:
		return theme.BandBusy.Render(component.Spinner(m.ui.Frame) + " " + strconv.Itoa(m.state.Run.StepsDone()) + "/" +
			strconv.Itoa(len(m.state.Run.Steps)))
	case state.PhaseChecking:
		return theme.BandBusy.Render(component.Spinner(m.ui.Frame) + " checks")
	case state.PhaseStopped:
		return theme.BandDown.Render("○ stopped")
	case state.PhaseFailed:
		return theme.BandFail.Render("✕ stopped")
	case state.PhaseReading, state.PhaseStopping, state.PhaseUp, state.PhaseDown:
	}
	return m.statusBadge()
}

// progressBar draws done of total as filled and empty cells.
func (m *model) progressBar(done, total int) string {
	theme := m.ui.Styles
	filled := 0
	if total > 0 {
		filled = done * progressCells / total
	}
	fill := lipgloss.NewStyle().Foreground(theme.Colors.Accent).Background(theme.Colors.Band)
	empty := lipgloss.NewStyle().Foreground(theme.Colors.Faint).Background(theme.Colors.Band)
	return fill.Render(strings.Repeat("━", filled)) + empty.Render(strings.Repeat("━", progressCells-filled))
}

// progress is the bring-up's progress for terminals that show it.
func (m *model) progress() *tea.ProgressBar {
	steps := m.state.Run.Steps
	if len(steps) == 0 {
		return nil
	}
	done := m.state.Run.StepsDone()
	switch phase, _ := m.state.Phase(); phase {
	case state.PhaseFailed:
		return tea.NewProgressBar(tea.ProgressBarError, done*percent/len(steps))
	case state.PhaseBringingUp:
		return tea.NewProgressBar(tea.ProgressBarDefault, done*percent/len(steps))
	case state.PhaseReading, state.PhaseChecking, state.PhaseStopping, state.PhaseStopped, state.PhaseUp,
		state.PhaseDown:
	}
	return nil
}

// layout sizes the log pane for the window, and the log chosen from the
// list or the tab that shows.
func (m *model) layout() {
	if m.overlay.kind == overlayLog {
		m.logs.Layout()
		return
	}
	switch m.tab {
	case tabAgent:
		m.agents.Layout()
	case tabTraffic:
		m.traffic.Layout()
	case tabHome, tabCount:
		width := m.ui.Width - component.PageIndent - component.BorderCols
		m.ui.Logs.SetSize(max(width, 1), max(m.ui.BodyRows()-component.BorderRows-1, 1))
	}
}

// withOverlay draws the open overlay, if any, in the middle of base.
func (m *model) withOverlay(base string) string {
	open, ok := m.overlayLayer()
	if !ok {
		return base
	}
	modal := open.view()
	if modal == "" {
		return base
	}
	return component.Overlay(base, modal, m.ui.Width, m.ui.Height)
}

// questionModal draws the setup's question outside the wizard. In the
// wizard, the question is part of the screen, so it draws nothing over it.
func (m *model) questionModal() string {
	if m.screen == screenWizard {
		return ""
	}
	return m.setup.QuestionModal(m.askTitle)
}

// helpModal lists every key by group, each group on as many rows as the
// modal's width needs. A short window leaves out the note on quitting.
func (m *model) helpModal() string {
	theme := m.ui.Styles
	width := component.ModalInnerWidth(m.ui.Width)
	rows := []string{theme.ModalTitle.Render("Keys"), ""}
	for _, group := range m.ui.Keys.Groups() {
		rows = append(rows, helpRows(theme, group, width)...)
	}
	about := theme.Muted.Width(width).Render("Quitting leaves the sandbox up, and it shows here again next time. " +
		"D stops everything.")
	footer := theme.Dim.Render("Press any key to close.")
	modal := theme.Modal.Width(component.ModalWidth(m.ui.Width))
	full := modal.Render(lipgloss.JoinVertical(lipgloss.Left, append(rows, "", about, "", footer)...))
	if lipgloss.Height(full) <= m.ui.Height {
		return full
	}
	return modal.Render(lipgloss.JoinVertical(lipgloss.Left, append(rows, "", footer)...))
}

// helpRows lays a group's keys out after its title, as many on a row as
// width takes.
func helpRows(theme style.Styles, group component.HelpGroup, width int) []string {
	var rows []string
	row := ""
	for _, binding := range group.Bindings {
		help := binding.Help()
		pair := theme.Accent.Render(help.Key) + " " + theme.Muted.Render(help.Desc)
		if row != "" && lipgloss.Width(row)+helpGap+lipgloss.Width(pair) > width-helpTitleWidth {
			rows, row = append(rows, row), ""
		}
		if row != "" {
			row += strings.Repeat(" ", helpGap)
		}
		row += pair
	}
	rows = append(rows, row)
	for i, line := range rows {
		title := ""
		if i == 0 {
			title = group.Title
		}
		rows[i] = theme.Text.Render(component.Pad(title, helpTitleWidth)) + line
	}
	return rows
}

func (m *model) confirmModal() string {
	theme := m.ui.Styles
	choice := component.Chips(theme, component.Hint{Key: "y", Desc: "yes"}, component.Hint{Key: "n", Desc: "no"})
	return theme.Modal.Width(component.ModalWidth(m.ui.Width)).Render(lipgloss.JoinVertical(lipgloss.Left,
		theme.ModalTitle.Render(m.overlay.title), "", theme.Muted.Render(m.overlay.body), "", choice))
}

// quitModal asks how to leave, and offers D only where it stops something.
func (m *model) quitModal() string {
	theme := m.ui.Styles
	if !m.anythingRunning() {
		parts := []string{theme.ModalTitle.Render(m.overlay.title), ""}
		if m.state.Run.Job.Is(state.JobSetup) {
			parts = append(parts, theme.Muted.Render(setupStops), "")
		}
		return theme.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, append(parts,
			component.Chips(theme, component.Hint{Key: component.KeyEnter, Desc: "quit"},
				component.Hint{Key: component.KeyEsc, Desc: "stay"}))...))
	}
	chips := []component.Hint{{Key: component.KeyEnter, Desc: "quit, leave it running"}}
	if !m.session.Local().Existing() {
		chips = append(chips, component.Hint{Key: "D", Desc: "stop everything and quit"})
	}
	chips = append(chips, component.Hint{Key: component.KeyEsc, Desc: "stay"})
	return theme.Modal.Width(component.ModalWidth(m.ui.Width)).Render(lipgloss.JoinVertical(lipgloss.Left,
		theme.ModalTitle.Render(m.overlay.title), "", theme.Muted.Render(m.quitBody()), "",
		component.Chips(theme, chips...)))
}
