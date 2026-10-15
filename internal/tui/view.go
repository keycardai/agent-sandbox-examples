package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

const (
	// percent turns a fraction into a progress value.
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
	shared := m.shared
	content := ""
	switch {
	case shared.Width == 0 || shared.Height == 0:
	case shared.Width < component.MinWidth || shared.Height < component.MinHeight:
		content = lipgloss.Place(shared.Width, shared.Height, lipgloss.Center, lipgloss.Center,
			shared.Styles.Muted.Render(fmt.Sprintf(
				"The window is too small for %s.\nIt needs %d×%d, and it is %d×%d.",
				style.AppName, component.MinWidth, component.MinHeight, shared.Width, shared.Height,
			)))
	default:
		content = m.withOverlay(m.screenView())
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

func (m *model) screenView() string {
	switch m.screen {
	case screenWelcome:
		return m.welcome.View()
	case screenWizard:
		return m.setup.WizardView()
	case screenReview:
		return m.setup.ReviewView()
	case screenSettings:
		return m.settings.View(m.settingsBackWord())
	case screenDashboard:
	}
	return m.dashboardView()
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
	case tabHealth:
		body, hints = m.health.View(), m.health.Hints()
	case tabHome, tabCount:
		body, hints = m.home.View(), m.home.Hints()
	}
	return m.shared.PageUnder(m.tabBand(), body, hints)
}

// tabBand draws the header band with the tabs, the one that shows marked,
// and how the sandbox is on the right.
func (m *model) tabBand() string {
	theme := m.shared.Styles
	tabs := component.Tabs(theme, []string{"Home", "Agents", "Traffic", "Health"}, int(m.tab))
	badge := m.statusBadge()
	if lipgloss.Width(theme.Wordmark())+lipgloss.Width(tabs)+lipgloss.Width(badge)+bandSpace > m.shared.Width {
		badge = m.shortBadge()
	}
	return component.Band(theme, m.shared.Width, tabs, badge)
}

// statusBadge says how the sandbox is, for the header band: a progress bar
// while it comes up, else one word.
func (m *model) statusBadge() string {
	shared := m.shared
	theme := shared.Styles
	if shared.Running {
		done := m.stepsDone()
		return theme.BandBusy.Render(component.Spinner(shared.Frame)+" bringing up ") +
			m.progressBar(done, len(shared.Steps)) + theme.BandLabel.Render(" "+strconv.Itoa(done)+"/"+
			strconv.Itoa(len(shared.Steps)))
	}
	if row := shared.FailedStep(); row != nil {
		return theme.BandFail.Render("✕ stopped at " + row.Step.Title)
	}
	if shared.SandboxUp() {
		return theme.BandUp.Render("● up")
	}
	return theme.BandDown.Render("○ down")
}

// shortBadge is the status badge for a narrow band: the steps done while it
// comes up, else one word.
func (m *model) shortBadge() string {
	shared := m.shared
	theme := shared.Styles
	if !shared.Running {
		if shared.FailedStep() != nil {
			return theme.BandFail.Render("✕ stopped")
		}
		return m.statusBadge()
	}
	return theme.BandBusy.Render(component.Spinner(shared.Frame) + " " + strconv.Itoa(m.stepsDone()) + "/" +
		strconv.Itoa(len(shared.Steps)))
}

// stepsDone counts the bring-up's steps that are done.
func (m *model) stepsDone() int {
	done := 0
	for _, row := range m.shared.Steps {
		if row.State == page.StepDone {
			done++
		}
	}
	return done
}

// progressBar draws done of total as filled and empty cells.
func (m *model) progressBar(done, total int) string {
	theme := m.shared.Styles
	filled := 0
	if total > 0 {
		filled = done * progressCells / total
	}
	fill := lipgloss.NewStyle().Foreground(theme.Colors.Accent).Background(theme.Colors.Band)
	empty := lipgloss.NewStyle().Foreground(theme.Colors.Faint).Background(theme.Colors.Band)
	return fill.Render(strings.Repeat("━", filled)) + empty.Render(strings.Repeat("━", progressCells-filled))
}

// progress is the bring-up's progress for terminals that show it, such as
// Ghostty, iTerm2 and Windows Terminal.
func (m *model) progress() *tea.ProgressBar {
	steps := m.shared.Steps
	if len(steps) == 0 {
		return nil
	}
	done, failed := 0, false
	for _, row := range steps {
		if row.State == page.StepDone {
			done++
		}
		failed = failed || (row.State == page.StepFailed && row.Step.ID != sandbox.StepChecks)
	}
	switch {
	case failed:
		return tea.NewProgressBar(tea.ProgressBarError, done*percent/len(steps))
	case m.shared.Running:
		return tea.NewProgressBar(tea.ProgressBarDefault, done*percent/len(steps))
	}
	return nil
}

// layout sizes the log pane for the window and the tab that shows.
func (m *model) layout() {
	switch m.tab {
	case tabAgent:
		m.agents.Layout()
	case tabTraffic:
		m.traffic.Layout()
	case tabHealth:
		m.health.Layout()
	case tabHome, tabCount:
		shared := m.shared
		width := shared.Width - component.PageIndent - component.BorderCols
		shared.Logs.SetSize(max(width, 1), max(shared.BodyRows()-component.BorderRows-1, 1))
	}
}

// withOverlay draws the open overlay, if any, in the middle of base. In
// the wizard, a question is part of the screen and not an overlay.
func (m *model) withOverlay(base string) string {
	var modal string
	switch m.overlay.kind {
	case overlayHelp:
		modal = m.helpModal()
	case overlayConfirm:
		modal = m.confirmModal()
	case overlayQuit:
		modal = m.quitModal()
	case overlayDemo:
		modal = m.demos.View()
	case overlayLogs:
		modal = m.health.LogsView()
	case overlayAgents:
		modal = m.agents.PickerView()
	case overlayQuestion:
		if m.screen == screenWizard {
			return base
		}
		modal = m.setup.QuestionModal(m.askTitle)
	case overlayNone:
		return base
	}
	return component.Overlay(base, modal, m.shared.Width, m.shared.Height)
}

// helpModal lists every key by group, each group on as many rows as the
// modal's width needs. A short window leaves out the note on quitting.
func (m *model) helpModal() string {
	shared := m.shared
	theme := shared.Styles
	width := component.ModalInnerWidth(shared.Width)
	rows := []string{theme.ModalTitle.Render("Keys"), ""}
	for _, group := range shared.Keys.Groups() {
		rows = append(rows, helpRows(theme, group, width)...)
	}
	about := theme.Muted.Width(width).Render("Quitting leaves the sandbox up, and it shows here again next time. " +
		"D stops everything.")
	footer := theme.Dim.Render("Press any key to close.")
	modal := theme.Modal.Width(component.ModalWidth(shared.Width))
	full := modal.Render(lipgloss.JoinVertical(lipgloss.Left, append(rows, "", about, "", footer)...))
	if lipgloss.Height(full) <= shared.Height {
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
	theme := m.shared.Styles
	choice := component.Chips(theme, component.Hint{Key: "y", Desc: "yes"}, component.Hint{Key: "n", Desc: "no"})
	return theme.Modal.Width(component.ModalWidth(m.shared.Width)).Render(lipgloss.JoinVertical(lipgloss.Left,
		theme.ModalTitle.Render(m.overlay.title), "", theme.Muted.Render(m.overlay.body), "", choice))
}

// quitModal asks how to leave. With nothing running, it only asks.
func (m *model) quitModal() string {
	theme := m.shared.Styles
	if !m.overlay.keepsRunning {
		parts := []string{theme.ModalTitle.Render(m.overlay.title), ""}
		if m.asking {
			parts = append(parts, theme.Muted.Render(setupStops), "")
		}
		return theme.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, append(parts,
			component.Chips(theme, component.Hint{Key: component.KeyEnter, Desc: "quit"},
				component.Hint{Key: component.KeyEsc, Desc: "stay"}))...))
	}
	return theme.Modal.Width(component.ModalWidth(m.shared.Width)).Render(lipgloss.JoinVertical(lipgloss.Left,
		theme.ModalTitle.Render(m.overlay.title), "", theme.Muted.Render(m.overlay.body), "",
		component.Chips(theme, component.Hint{Key: component.KeyEnter, Desc: "quit, leave it running"},
			component.Hint{Key: "D", Desc: "stop everything and quit"}, component.Hint{Key: component.KeyEsc, Desc: "stay"})))
}
