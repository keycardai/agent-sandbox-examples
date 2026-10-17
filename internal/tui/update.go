package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

// Init shows Home. When the bring-up starts with the tunnel, Init brings
// the sandbox up, unless the tunnel already runs: then the sandbox may be
// up from before, and the TUI only shows what runs. Without a tunnel step,
// as on an existing cluster, the bring-up waits for u. Home says what to
// do once the first read comes back, since the sandbox may be up.
func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{frameTick(), m.poll(), m.bus.next(), tea.RequestWindowSize, tea.RequestBackgroundColor}
	switch {
	case len(m.state.Run.Steps) == 0, m.state.Run.Steps[0].Step.ID != sandbox.StepTunnel:
	case m.session.Local().ServiceStatus(sandbox.ServiceTunnel).Running:
		m.state.Record("•", "Found the tunnel running from before")
		m.ui.Notify("Found the tunnel from before.", false)
	default:
		m.startBringUp(0, false)
	}
	return tea.Batch(cmds...)
}

// Update handles one message.
//
//nolint:ireturn // Bubble Tea's Model interface returns the model as tea.Model.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if queued, ok := msg.(busMsg); ok {
		_, cmd := m.Update(queued.msg)
		return m, tea.Batch(cmd, m.bus.next())
	}
	if cmd, handled := m.updateBackground(msg); handled {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.ui.Width, m.ui.Height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		m.ui.Styles = style.New(msg.IsDark())
	case tea.KeyPressMsg:
		return m, m.keyPress(msg)
	}
	return m, nil
}

// updateBackground handles the messages of background work.
func (m *model) updateBackground(msg tea.Msg) (tea.Cmd, bool) {
	if cmd, handled := m.updateJobs(msg); handled {
		return cmd, true
	}
	if m.updateAnswers(msg) {
		return nil, true
	}
	switch msg := msg.(type) {
	case frameMsg:
		m.ui.Frame++
		return frameTick(), true
	case pollMsg:
		m.tickPlanned = false
		return m.poll(), true
	case statusMsg:
		if m.state.Reads.Current(msg.mark) {
			m.state.Reads.Set(msg.status)
		} else {
			m.stale = true
		}
		return m.polled(), true
	case podsMsg:
		m.onPods(msg)
		return m.polled(), true
	}
	return nil, false
}

// updateJobs handles what the bring-up, an action and the agent report.
func (m *model) updateJobs(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case stepStartMsg:
		m.state.StepStarted(msg.index)
	case stepDoneMsg:
		m.onStepDone(msg)
	case bringUpDoneMsg:
		return m.onBringUpDone(msg), true
	case actionDoneMsg:
		return m.onActionDone(msg), true
	case execDoneMsg:
		return m.onExecDone(msg), true
	case eventsMsg:
		m.onEvents(msg)
	default:
		return nil, false
	}
	return nil, true
}

// keyPress routes a key: to the overlay when one is open, else to Home. A
// question that waited opens once the overlay closed, or once the answer
// it sent is handled.
func (m *model) keyPress(msg tea.KeyPressMsg) tea.Cmd {
	if cmd, handled := m.overlayKeys(msg); handled {
		if cmd == nil {
			m.openWaiting()
		}
		return cmd
	}
	switch {
	case msg.String() == component.KeyCtrlC, msg.String() == "q":
		return m.quit()
	case key.Matches(msg, m.ui.Keys.Help):
		m.overlay = overlay{kind: overlayHelp}
		return nil
	}
	return m.dashboardKeys(msg)
}

// overlayKeys gives a key to the open overlay, if any.
func (m *model) overlayKeys(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch m.overlay.kind {
	case overlayConfirm:
		return m.confirmKeys(msg), true
	case overlayQuit:
		return m.quitKeys(msg), true
	case overlayHelp:
		m.overlay = overlay{}
		return nil, true
	case overlayNone:
	}
	return nil, false
}

// quit asks how to leave. By default the sandbox keeps running: the
// tunnel, the mirror and the cluster stay up, and they show again next
// time. D asks, then stops everything first. With nothing running, it
// only asks. The dialog says what runs when it is drawn, so a job that
// ends while it is open drops out of it.
func (m *model) quit() tea.Cmd {
	m.overlay = overlay{kind: overlayQuit, title: "Quit " + style.AppName + "?"}
	return nil
}

// quitBody says what quitting leaves running, and what it stops.
func (m *model) quitBody() string {
	body := "The sandbox keeps running. The tunnel, the mirror and the cluster stay up,\n" +
		"and they show here again next time."
	if m.session.Local().Existing() {
		body = "The sandbox keeps running in the cluster, and shows here again next time."
	}
	switch job := m.state.Run.Job; job.Kind {
	case state.JobBringUp:
		body = "A bring-up is running, and quitting stops it where it is.\n" + body
	case state.JobChecks:
		body = "The checks are running, and quitting stops them where they are.\n" + body
	case state.JobAction, state.JobStop:
		body = job.Name + " is running, and quitting stops it where it is.\n" + body
	case state.JobNone:
	}
	return body
}

// anythingRunning reports whether a part of the sandbox runs that D
// stops: a bring-up, an action, the tunnel, the mirror or the local
// cluster.
func (m *model) anythingRunning() bool {
	status := m.state.Reads.Status
	local := status.ClusterExists && !m.session.Local().Existing()
	return m.state.Run.Job.Busy() || status.Tunnel.Running || status.Mirror.Running || local
}

func (m *model) quitKeys(msg tea.KeyPressMsg) tea.Cmd {
	keepsRunning := m.anythingRunning()
	m.overlay = overlay{}
	switch msg.String() {
	case component.KeyEnter, "q", "y", component.KeyCtrlC:
		m.state.Run.CancelJob()
		return tea.Quit
	case "D":
		if !keepsRunning || !m.state.Run.Job.RunsSteps() && m.busy() {
			return nil
		}
		m.askStopAll(true)
	}
	return nil
}

// confirm asks a yes or no question. y sends yes to Update, which acts on
// it then. Any other key, enter too, says no.
func (m *model) confirm(title, body string, yes tea.Msg) {
	m.overlay = overlay{kind: overlayConfirm, title: title, body: body, yes: yes}
}

// confirmLater is confirm for a question from background work. While
// another overlay is open, the question waits for it to close, so that a
// key meant for that overlay does not answer it.
func (m *model) confirmLater(title, body string, yes tea.Msg) {
	if m.overlay.kind == overlayNone {
		m.confirm(title, body, yes)
		return
	}
	m.waiting = &overlay{kind: overlayConfirm, title: title, body: body, yes: yes}
}

// openWaiting opens the question that waited, once no overlay is open.
func (m *model) openWaiting() {
	if m.overlay.kind == overlayNone && m.waiting != nil {
		m.overlay, m.waiting = *m.waiting, nil
	}
}

func (m *model) confirmKeys(msg tea.KeyPressMsg) tea.Cmd {
	yes := m.overlay.yes
	m.overlay = overlay{}
	if msg.String() != "y" && msg.String() != "Y" {
		return nil
	}
	return func() tea.Msg { return yes }
}

// dashboardKeys handles a key on Home: the actions on the sandbox, then
// those on the agent.
func (m *model) dashboardKeys(msg tea.KeyPressMsg) tea.Cmd {
	if m.actionKeys(msg) {
		return nil
	}
	return m.agentActionKeys(msg)
}

// actionKeys handles the keys that act on the sandbox, and reports whether
// msg was one.
func (m *model) actionKeys(msg tea.KeyPressMsg) bool {
	keys := m.ui.Keys
	switch {
	case key.Matches(msg, keys.Start):
		m.toggleBringUp()
	case key.Matches(msg, keys.Checks):
		m.runChecks()
	case key.Matches(msg, keys.Restart):
		m.restartAgent()
	case key.Matches(msg, keys.StopAll):
		m.stopAll()
	default:
		return false
	}
	return true
}

// agentActionKeys handles the keys that work with the agent.
func (m *model) agentActionKeys(msg tea.KeyPressMsg) tea.Cmd {
	keys := m.ui.Keys
	switch {
	case key.Matches(msg, keys.Agent):
		return m.agent()
	case key.Matches(msg, keys.Shell):
		return m.openAgent(true)
	case key.Matches(msg, keys.Events):
		return m.openEvents()
	}
	return nil
}

// busy reports whether a job runs, and then says which in the footer.
// Work that changes the sandbox waits for it.
func (m *model) busy() bool {
	text := m.state.Run.Job.WaitText()
	if text != "" {
		m.ui.Notify(text, true)
	}
	return text != ""
}

// isDown reports whether the last read found the sandbox down, and then
// says so in the footer. Work on the agent needs the sandbox up.
func (m *model) isDown() bool {
	if !m.state.Reads.Done || m.state.Up() {
		return false
	}
	m.ui.Notify("The sandbox is down. Press u to bring it up.", true)
	return true
}
