// Package page holds the TUI's screens. A page keeps only how it shows the
// state, such as a cursor. The root model changes the state.
package page

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

const (
	// toastFor is how long a notice stays in the footer.
	toastFor = 6 * time.Second
	// readWidth bounds paragraphs, so they stay readable on a wide window.
	readWidth = 76
	// shortID is how much of a zone's ID the header shows.
	shortID = 8
	// half halves a size, to center or split it.
	half = 2
	// Words several pages show.
	wordQuit   = "quit"
	wordKeys   = "keys"
	wordChoose = "choose"
	wordAgent  = "agent"
)

// UI is what the pages draw with. The root model sets it.
type UI struct {
	Styles style.Styles
	Keys   component.KeyMap
	// The window's size, and the terminal's background once it reports it.
	Width, Height int
	Background    color.Color
	// Frame counts the redraws, and turns the spinners.
	Frame int
	// Logs holds every log, and shows one in the log pane.
	Logs  *component.LogView
	Toast component.Toast
	// ShowDNS adds the DNS lookups to the traffic feed.
	ShowDNS bool
}

// NewUI returns the UI before the window and the colors are known.
func NewUI() *UI {
	ui := &UI{Styles: style.New(true), Keys: component.NewKeyMap()}
	ui.Logs = component.NewLogView(ui.paint)
	ui.Logs.FromTop(state.SourceResponse)
	return ui
}

// TrafficSource names the feed of the proxy's decisions the TUI shows.
func (u *UI) TrafficSource() string {
	if u.ShowDNS {
		return state.SourceTrafficAll
	}
	return state.SourceTraffic
}

// Notify shows text in the footer for a while.
func (u *UI) Notify(text string, isErr bool) {
	u.Toast = component.Toast{Text: text, Err: isErr, Until: time.Now().Add(toastFor)}
}

// Page draws a full screen with title in the header band.
func (u *UI) Page(title, body string, hints []component.Hint) string {
	return u.PageUnder(component.Band(u.Styles, u.Width, u.Styles.BandLabel.Render(title), ""), body, hints)
}

// PageUnder draws body between header and the footer, cut to the window.
func (u *UI) PageUnder(header, body string, hints []component.Hint) string {
	return component.Screen(u.Width, u.Height, header, body, component.Footer(u.Styles, u.Width, hints, u.Toast))
}

// BodyRows is the rows a page's body has under its top padding.
func (u *UI) BodyRows() int { return max(u.Height-component.ChromeRows-1, 1) }

// view is what a page draws from: the UI, and the sandbox's state.
type view struct {
	ui    *UI
	state *state.Sandbox
}

// AgentName names the selected agent in a sentence, by its person.
func AgentName(sandbox *state.Sandbox) string {
	if agent, _ := sandbox.Selected(); agent.Email != "" {
		return agent.Email + "'s agent"
	}
	return "the agent"
}

// AgentActionName says what a does, for the footer.
func (v view) AgentActionName() string {
	scenario := v.state.LoadedDemo()
	switch {
	case scenario == nil:
		return wordAgent
	case scenario.Run.Mode == sandbox.ModeShell && len(scenario.Run.Command) > 0:
		return "open " + scenario.Run.Command[0]
	case scenario.Run.Mode == sandbox.ModeBackground:
		return "agent log"
	}
	return "open agent"
}

func (v view) agentName() string { return AgentName(v.state) }

func (v view) endpoints() []sandbox.ScenarioEndpoint {
	if scenario := v.state.LoadedDemo(); scenario != nil {
		return scenario.EndpointList()
	}
	return nil
}

func (u *UI) spinner() string { return component.Spinner(u.Frame) }

// podMark is a pod state's mark, and the style of the mark and its text.
func (u *UI) podMark(pod state.PodState) (string, lipgloss.Style) {
	switch pod {
	case state.PodRunning:
		return "●", u.Styles.OK
	case state.PodFirewallFailed, state.PodAgentFailed, state.PodUnread:
		return "✕", u.Styles.Failed
	case state.PodNone, state.PodMissing:
		return "○", u.Styles.Dim
	case state.PodComingUp, state.PodStarting:
	}
	return u.spinner(), u.Styles.Running
}

// bodyWidth is the width of a page's body inside the indent on both sides.
func (u *UI) bodyWidth() int { return max(u.Width-component.PageIndent*half, 1) }

// para wraps text to the body's width, or a readable width, in st.
func (u *UI) para(st lipgloss.Style, text string) string {
	return st.Width(min(u.bodyWidth(), readWidth+readWidth/half)).Render(text)
}

// actionLine is a key and what it does.
func (u *UI) actionLine(key, text string) string {
	return u.Styles.KeyChip.Render(key) + " " + u.Styles.Text.Render(text)
}

// agentBar says which agent the tab shows, when there is more than one.
func (v view) agentBar() []string {
	if len(v.state.Agents.List) <= 1 {
		return nil
	}
	theme := v.ui.Styles
	agent, index := v.state.Selected()
	return []string{theme.Dim.Render("Showing ") + theme.Accent.Render(agent.Email+"'s agent") +
		theme.Dim.Render(fmt.Sprintf(", %d of %d. Pick another on the Agents tab (2).", index+1, len(v.state.Agents.List)))}
}

// stepLine draws one step: its icon, its title, and how long it took.
func (v view) stepLine(row *state.StepRow, width int) string {
	theme := v.ui.Styles
	icon, elapsed := v.stepIcon(row), v.stepElapsed(row)
	space := max(width-lipgloss.Width(icon)-1-lipgloss.Width(elapsed)-1, 1)
	title := ansi.Truncate(row.Step.Title, space, "…")
	lineStyle := theme.Muted
	switch row.State {
	case state.StepRunning:
		lineStyle = theme.Text.Bold(true)
	case state.StepFailed:
		lineStyle = theme.Failed
		if row.Stopped() {
			lineStyle = theme.Text
		}
	case state.StepDone:
		lineStyle = theme.Text
	case state.StepPending:
	}
	return icon + " " + lineStyle.Render(title) + strings.Repeat(" ", max(space-lipgloss.Width(title), 0)) + " " +
		elapsed
}

func (v view) stepIcon(row *state.StepRow) string {
	theme := v.ui.Styles
	switch row.State {
	case state.StepRunning:
		return theme.Running.Render(v.ui.spinner())
	case state.StepDone:
		return theme.OK.Render("✓")
	case state.StepFailed:
		if row.Stopped() {
			return theme.Text.Render("○")
		}
		return theme.Failed.Render("✕")
	case state.StepPending:
	}
	return theme.Faint.Render("·")
}

// stepElapsed says how long a step took, or the checks' score.
func (v view) stepElapsed(row *state.StepRow) string {
	theme := v.ui.Styles
	if row.Total > 0 && row.State != state.StepRunning {
		scoreStyle := theme.OK
		if row.Passed < row.Total {
			scoreStyle = theme.Warn
		}
		return scoreStyle.Render(strconv.Itoa(row.Passed) + "/" + strconv.Itoa(row.Total) + " passed")
	}
	switch row.State {
	case state.StepRunning:
		return theme.Running.Render(component.Duration(v.state.Now().Sub(row.Started)))
	case state.StepDone, state.StepFailed:
		return theme.Dim.Render(component.Duration(row.Ended.Sub(row.Started)))
	case state.StepPending:
	}
	return ""
}

// containerDetail says what a container does, and how often it restarted.
func containerDetail(ctr sandbox.ContainerStatus) string {
	detail := ctr.State
	switch {
	case ctr.Init && ctr.State == state.StateTerminated && ctr.ExitCode == 0:
		detail = "done"
	case ctr.Reason != "":
		detail = ctr.Reason
	}
	if ctr.State == state.StateTerminated && ctr.ExitCode != 0 {
		detail += " (exit " + strconv.Itoa(ctr.ExitCode) + ")"
	}
	if ctr.Restarts > 0 {
		detail += " ↻" + strconv.Itoa(ctr.Restarts)
	}
	return detail
}

// shortHost shortens a zone URL's long ID, and keeps the domain.
func shortHost(url string) string {
	host := strings.TrimPrefix(url, "https://")
	id, domain, ok := strings.Cut(host, ".")
	if !ok || len(id) <= shortID {
		return host
	}
	return id[:shortID] + "…" + domain
}
