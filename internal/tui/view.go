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
	// percent turns a fraction into a progress value.
	percent = 100
	// progressCells is the width of the bring-up's progress bar.
	progressCells = 12
	// bandSpace is what the band puts around and between its parts.
	bandSpace = 6
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
	return m.ui.PageUnder(m.band(), m.home.View(), m.home.Hints())
}

// band draws the header band, with how the sandbox is on the right.
func (m *model) band() string {
	theme := m.ui.Styles
	label := theme.BandLabel.Render("home")
	badge := m.statusBadge()
	if lipgloss.Width(theme.Wordmark())+lipgloss.Width(label)+lipgloss.Width(badge)+bandSpace > m.ui.Width {
		badge = m.shortBadge()
	}
	return component.Band(theme, m.ui.Width, label, badge)
}

// statusBadge says how the sandbox is, for the header band: a progress bar
// while it comes up, else one word.
func (m *model) statusBadge() string {
	theme := m.ui.Styles
	switch phase, row := m.state.Phase(); phase {
	case state.PhaseBringingUp:
		done := m.stepsDone()
		return theme.BandBusy.Render(component.Spinner(m.ui.Frame)+" bringing up ") +
			m.progressBar(done, len(m.state.Steps)) + theme.BandLabel.Render(" "+strconv.Itoa(done)+"/"+
			strconv.Itoa(len(m.state.Steps)))
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

// shortBadge is the status badge for a narrow band: the steps done while it
// comes up, else one word.
func (m *model) shortBadge() string {
	theme := m.ui.Styles
	switch phase, _ := m.state.Phase(); phase {
	case state.PhaseBringingUp:
		return theme.BandBusy.Render(component.Spinner(m.ui.Frame) + " " + strconv.Itoa(m.stepsDone()) + "/" +
			strconv.Itoa(len(m.state.Steps)))
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

// stepsDone counts the bring-up's steps that are done.
func (m *model) stepsDone() int {
	done := 0
	for _, row := range m.state.Steps {
		if row.State == state.StepDone {
			done++
		}
	}
	return done
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

// progress is the bring-up's progress for terminals that show it, such as
// Ghostty, iTerm2 and Windows Terminal.
func (m *model) progress() *tea.ProgressBar {
	steps := m.state.Steps
	if len(steps) == 0 {
		return nil
	}
	done := m.stepsDone()
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
	return component.Overlay(base, modal, m.ui.Width, m.ui.Height)
}

func (m *model) helpModal() string {
	theme := m.ui.Styles
	m.help.ShowAll = true
	m.help.Styles.FullKey = theme.Accent
	m.help.Styles.FullDesc = theme.Muted
	m.help.Styles.FullSeparator = theme.Faint
	body := m.help.FullHelpView(m.ui.Keys.All())
	about := theme.Muted.Render("Quitting leaves the sandbox up: the tunnel, the mirror and the cluster keep running,\n" +
		"and they show here again next time. D stops everything.")
	return theme.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, theme.ModalTitle.Render("Keys"), "", body, "",
		about, "", theme.Dim.Render("Press any key to close.")))
}

func (m *model) confirmModal() string {
	theme := m.ui.Styles
	choice := component.Chips(theme, component.Hint{Key: "y", Desc: "yes"}, component.Hint{Key: "n", Desc: "no"})
	return theme.Modal.Width(component.ModalWidth(m.ui.Width)).Render(lipgloss.JoinVertical(lipgloss.Left,
		theme.ModalTitle.Render(m.overlay.title), "", theme.Muted.Render(m.overlay.body), "", choice))
}

// quitModal asks how to leave. With nothing running, it only asks. D is
// offered only where it stops something: not on an existing cluster.
func (m *model) quitModal() string {
	theme := m.ui.Styles
	if !m.anythingRunning() {
		return theme.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, theme.ModalTitle.Render(m.overlay.title), "",
			component.Chips(theme, component.Hint{Key: component.KeyEnter, Desc: "quit"},
				component.Hint{Key: component.KeyEsc, Desc: "stay"})))
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
