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
		content = m.withOverlay(m.dashboardView())
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "Keycard sandbox · " + ansi.Strip(m.statusBadge())
	view.ProgressBar = m.progress()
	return view
}

// dashboardView draws Home, under the header band and over the footer
// with Home's keys.
func (m *model) dashboardView() string {
	return m.shared.PageUnder(m.band(), m.home.View(), m.home.Hints())
}

// band draws the header band, with how the sandbox is on the right.
func (m *model) band() string {
	theme := m.shared.Styles
	label := theme.BandLabel.Render("home")
	badge := m.statusBadge()
	if lipgloss.Width(theme.Wordmark())+lipgloss.Width(label)+lipgloss.Width(badge)+bandSpace > m.shared.Width {
		badge = m.shortBadge()
	}
	return component.Band(theme, m.shared.Width, label, badge)
}

// statusBadge says how the sandbox is, for the header band: a progress bar
// while it comes up, else one word.
func (m *model) statusBadge() string {
	shared := m.shared
	theme := shared.Styles
	if shared.Job.Is(page.JobBringUp) {
		done := m.stepsDone()
		return theme.BandBusy.Render(component.Spinner(shared.Frame)+" bringing up ") +
			m.progressBar(done, len(shared.Steps)) + theme.BandLabel.Render(" "+strconv.Itoa(done)+"/"+
			strconv.Itoa(len(shared.Steps)))
	}
	if shared.Job.Is(page.JobStop) {
		return theme.BandBusy.Render(component.Spinner(shared.Frame) + " stopping")
	}
	if row := shared.FailedStep(); row != nil {
		if row.Stopped() {
			return theme.BandDown.Render("○ stopped at " + row.Step.Title)
		}
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
	if !shared.Job.Is(page.JobBringUp) {
		if row := shared.FailedStep(); !shared.Job.Is(page.JobStop) && row != nil {
			if row.Stopped() {
				return theme.BandDown.Render("○ stopped")
			}
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
		failed = failed || (row.State == page.StepFailed && row.Step.ID != sandbox.StepChecks && !row.Stopped())
	}
	switch {
	case failed:
		return tea.NewProgressBar(tea.ProgressBarError, done*percent/len(steps))
	case m.shared.Job.Is(page.JobBringUp):
		return tea.NewProgressBar(tea.ProgressBarDefault, done*percent/len(steps))
	}
	return nil
}

// withOverlay draws the open overlay, if any, in the middle of base.
func (m *model) withOverlay(base string) string {
	var modal string
	switch m.overlay.kind {
	case overlayHelp:
		modal = m.helpModal()
	case overlayConfirm:
		modal = m.confirmModal()
	case overlayQuit:
		modal = m.quitModal()
	case overlayNone:
		return base
	}
	return component.Overlay(base, modal, m.shared.Width, m.shared.Height)
}

func (m *model) helpModal() string {
	theme := m.shared.Styles
	m.help.ShowAll = true
	m.help.Styles.FullKey = theme.Accent
	m.help.Styles.FullDesc = theme.Muted
	m.help.Styles.FullSeparator = theme.Faint
	body := m.help.FullHelpView(m.shared.Keys.All())
	about := theme.Muted.Render("Quitting leaves the sandbox up: the tunnel, the mirror and the cluster keep running,\n" +
		"and they show here again next time. D stops everything.")
	return theme.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, theme.ModalTitle.Render("Keys"), "", body, "",
		about, "", theme.Dim.Render("Press any key to close.")))
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
		return theme.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, theme.ModalTitle.Render(m.overlay.title), "",
			component.Chips(theme, component.Hint{Key: component.KeyEnter, Desc: "quit"},
				component.Hint{Key: component.KeyEsc, Desc: "stay"})))
	}
	return theme.Modal.Width(component.ModalWidth(m.shared.Width)).Render(lipgloss.JoinVertical(lipgloss.Left,
		theme.ModalTitle.Render(m.overlay.title), "", theme.Muted.Render(m.overlay.body), "",
		component.Chips(theme, component.Hint{Key: component.KeyEnter, Desc: "quit, leave it running"},
			component.Hint{Key: "D", Desc: "stop everything and quit"}, component.Hint{Key: component.KeyEsc, Desc: "stay"})))
}
