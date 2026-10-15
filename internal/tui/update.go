package tui

import (
	"context"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

// Init picks the first screen. With settings that have a problem, such as
// no settings at all, it opens the welcome screen. Otherwise it shows the
// dashboard. When the bring-up starts with the tunnel, Init brings the
// sandbox up, unless the tunnel already runs: then the sandbox is up from
// before, and the TUI only shows it. Without a tunnel step, as on an
// existing cluster, the bring-up waits for u.
func (m *model) Init() tea.Cmd {
	shared := m.shared
	for _, name := range []string{sandbox.ServiceTunnel, sandbox.ServiceMirror} {
		path := shared.Session.Local().ServiceStatus(m.ctx, name).LogPath
		log := shared.Logs.Log(page.ServiceSource(name))
		m.jobs.Go(func() { component.TailFile(m.ctx, path, log) })
	}
	cmds := []tea.Cmd{frameTick(), m.poll(), m.bus.next(), tea.RequestWindowSize, tea.RequestBackgroundColor}
	switch {
	case len(shared.Problems) > 0:
		return tea.Batch(append(cmds, m.openWelcome())...)
	case len(shared.Steps) == 0:
		return tea.Batch(cmds...)
	}
	shared.Logs.Show(page.StepSource(shared.Steps[0].Step.ID))
	switch {
	case shared.Steps[0].Step.ID != sandbox.StepTunnel:
		shared.Notify("Press u to bring the sandbox up.", false)
	case shared.Session.Local().ServiceStatus(m.ctx, sandbox.ServiceTunnel).Running:
		shared.Record("•", "Found the sandbox running from before")
		shared.Notify("Showing the sandbox from before. Press u to bring it up again.", false)
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
		m.shared.Width, m.shared.Height = msg.Width, msg.Height
		m.layout()
		return m, m.setup.Resize()
	case tea.BackgroundColorMsg:
		m.shared.Styles, m.shared.Background = style.New(msg.IsDark()), msg.Color
		m.shared.Logs.Refresh(true)
	case page.AskMsg:
		return m, m.ask(msg.Question)
	case tea.KeyPressMsg:
		return m, m.keyPress(msg)
	case tea.MouseWheelMsg:
		if m.overlay.kind == overlayNone {
			m.shared.Logs.Wheel(msg)
		}
	}
	if m.overlay.kind == overlayQuestion {
		return m, m.answer(msg)
	}
	return m, nil
}

// Follow follows the log of a pod's container in the log pane, and stops
// following the one before.
func (m *model) Follow(row page.ContainerRow) {
	name := page.ContainerSource(row.Pod, row.Container.Name)
	if m.stream != name {
		m.stopStream()
		m.stream = name
		ctx, cancel := context.WithCancel(m.ctx)
		m.streamStop = cancel
		finished := row.Container.State == page.StateTerminated
		session, log := m.shared.Session, m.shared.Logs.Log(name)
		m.jobs.Go(func() { followContainer(ctx, session, row.Pod, row.Container.Name, finished, log) })
	}
	m.shared.Logs.Show(name)
}

// ShowAgentList opens the list of agents, which the Agents tab draws.
func (m *model) ShowAgentList() {
	m.overlay = overlay{kind: overlayAgents}
}

// Context is the TUI's context, which ends when it quits.
func (m *model) Context() context.Context { return m.ctx }

// Acting names the action that runs, or is "".
func (m *model) Acting() string { return m.acting }

// updateBackground handles the messages of background work.
func (m *model) updateBackground(msg tea.Msg) (tea.Cmd, bool) {
	if cmd, handled := m.updateJobs(msg); handled {
		return cmd, true
	}
	switch msg := msg.(type) {
	case style.GlintMsg:
		return m.welcome.Glint.Move(msg.Now, m.screen == screenWelcome && m.welcome.Checking()), true
	case frameMsg:
		m.shared.Frame++
		m.shared.Logs.Refresh(false)
		if m.tab == tabHealth {
			// A check gains or loses its fix line as the sandbox changes,
			// which moves the log under the checks.
			m.health.Layout()
		}
		return tea.Batch(frameTick(), m.welcomeGlint()), true
	case pollMsg:
		m.tickPlanned = false
		return m.poll(), true
	case statusMsg:
		m.shared.Status, m.shared.Read = msg.status, true
		return m.polled(), true
	case podsMsg:
		m.onPods(msg)
		return m.polled(), true
	}
	return nil, false
}

// updateJobs handles what the bring-up, the setup, an action and the agent
// report.
func (m *model) updateJobs(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case stepStartMsg:
		m.onStepStart(msg)
	case stepDoneMsg:
		m.onStepDone(msg)
	case bringUpDoneMsg:
		return m.onBringUpDone(msg), true
	case actionDoneMsg:
		return m.onActionDone(msg), true
	case execDoneMsg:
		return m.onExecDone(msg), true
	case page.RequestDoneMsg:
		m.agents.OnRequestDone(msg)
	case eventsMsg:
		m.onEvents(msg)
	case agentAddedMsg:
		m.onAgentAdded(msg)
	default:
		return m.updateOnboarding(msg)
	}
	return nil, true
}

// updateOnboarding handles what the machine checks and the setup report.
func (m *model) updateOnboarding(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case page.CheckDoneMsg:
		return m.welcome.OnCheck(msg), true
	case page.SectionMsg:
		m.setup.OnSection(msg)
	case setupDoneMsg:
		m.onSetupDone(msg)
	case forgetDoneMsg:
		m.onForgetDone(msg)
	case settingsMsg:
		m.shared.Describe, m.shared.Problems = msg.describe, msg.problems
	case page.PresetMsg:
		m.setup.OnPreset(msg)
	default:
		return nil, false
	}
	return nil, true
}

// welcomeGlint starts the sheen across the mark while the welcome screen
// checks the machine, however the screen came to show.
func (m *model) welcomeGlint() tea.Cmd {
	if m.screen != screenWelcome || !m.welcome.Checking() {
		return nil
	}
	return m.welcome.Glint.Start()
}

// keyPress routes a key: to the overlay when one is open, else to the
// screen.
func (m *model) keyPress(msg tea.KeyPressMsg) tea.Cmd {
	if cmd, handled := m.overlayKeys(msg); handled {
		return cmd
	}
	if m.shared.Logs.Searching() {
		return m.shared.Logs.SearchKeys(msg)
	}
	switch {
	case m.askTitle != "" && msg.String() == component.KeyEsc:
		// Between the questions for a new agent, esc stops adding it.
		m.stopSetup()
		return nil
	case msg.String() == component.KeyCtrlC, msg.String() == "q":
		return m.quit()
	case key.Matches(msg, m.shared.Keys.Help):
		m.overlay = overlay{kind: overlayHelp}
		return nil
	case key.Matches(msg, m.shared.Keys.Mouse):
		m.mouse = !m.mouse
		m.shared.Notify(map[bool]string{
			true:  "Mouse on: the wheel scrolls; hold Option or Shift to select text.",
			false: "Mouse off: select and copy text as usual.",
		}[m.mouse], false)
		return nil
	}
	return m.screenKeys(msg)
}

// screenKeys gives a key to the screen that shows.
func (m *model) screenKeys(msg tea.KeyPressMsg) tea.Cmd {
	switch m.screen {
	case screenWelcome:
		return m.welcomeKeys(msg)
	case screenWizard:
		// A question takes the keys itself. Between questions, esc stops
		// the setup.
		if msg.String() == component.KeyEsc {
			m.stopSetup()
		}
		return nil
	case screenReview:
		return m.reviewKeys(msg)
	case screenSettings:
		return m.settingsKeys(msg)
	case screenDashboard:
	}
	return m.dashboardKeys(msg)
}

// overlayKeys gives a key to the open overlay, if any.
func (m *model) overlayKeys(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch m.overlay.kind {
	case overlayQuestion:
		return m.answer(msg), true
	case overlayConfirm:
		return m.confirmKeys(msg), true
	case overlayQuit:
		return m.quitKeys(msg), true
	case overlayDemo:
		m.demoKeys(msg)
		return nil, true
	case overlayLogs:
		m.logsKeys(msg)
		return nil, true
	case overlayAgents:
		m.agentListKeys(msg)
		return nil, true
	case overlayHelp:
		m.closeOverlay()
		return nil, true
	case overlayNone:
	}
	return nil, false
}

// quit asks how to leave. By default the sandbox keeps running: the
// tunnel, the mirror and the cluster stay up, and they show again next
// time. D stops everything first. With nothing running, it only asks.
func (m *model) quit() tea.Cmd {
	m.overlay = overlay{kind: overlayQuit, title: "Quit " + style.AppName + "?", keepsRunning: m.anythingRunning()}
	m.overlay.body = "The sandbox keeps running. The tunnel, the mirror and the cluster stay up,\n" +
		"and they show here again next time."
	if m.shared.Session.Local().Existing() {
		m.overlay.body = "The sandbox keeps running in the cluster, and shows here again next time."
	}
	switch {
	case m.shared.Running:
		m.overlay.body = "A bring-up is running, and quitting stops it where it is.\n" + m.overlay.body
	case m.acting != "":
		m.overlay.body = m.acting + " is running, and quitting stops it where it is.\n" + m.overlay.body
	case m.asking:
		m.overlay.body = setupStops + "\n" + m.overlay.body
	}
	return nil
}

// setupStops is what quitting does to a setup that runs.
const setupStops = "Setup stops, and keeps none of its answers."

// closeOverlay closes the dialog that shows, and shows the setup's
// question again when one waits.
func (m *model) closeOverlay() {
	m.overlay = overlay{}
	if m.setup.Asking() {
		m.overlay.kind = overlayQuestion
	}
}

// anythingRunning reports whether a part of the sandbox runs that D
// stops: a bring-up, an action, the tunnel, the mirror or the local
// cluster.
func (m *model) anythingRunning() bool {
	status := m.shared.Status
	local := status.ClusterExists && !m.shared.Session.Local().Existing()
	return m.shared.Running || m.acting != "" || status.Tunnel.Running || status.Mirror.Running || local
}

func (m *model) quitKeys(msg tea.KeyPressMsg) tea.Cmd {
	keepsRunning := m.overlay.keepsRunning
	m.closeOverlay()
	switch msg.String() {
	case component.KeyEnter, "q", "y", component.KeyCtrlC:
		m.stop()
		m.stopSetup()
		return tea.Quit
	case "D":
		if !keepsRunning {
			return nil
		}
		if m.acting != "" {
			m.busy()
			return nil
		}
		m.stopSetup()
		m.quitAfterAction = true
		m.down()
	}
	return nil
}

// confirm asks a yes or no question, and runs yes on y. Any other key,
// enter too, says no.
func (m *model) confirm(title, body string, yes func() tea.Cmd) {
	m.overlay = overlay{kind: overlayConfirm, title: title, body: body, confirm: yes}
}

func (m *model) confirmKeys(msg tea.KeyPressMsg) tea.Cmd {
	yes := m.overlay.confirm
	m.closeOverlay()
	if msg.String() != "y" && msg.String() != "Y" {
		return nil
	}
	return yes()
}

// dashboardKeys handles a key on the dashboard: the tabs first, then the
// actions, then the tab that shows.
func (m *model) dashboardKeys(msg tea.KeyPressMsg) tea.Cmd {
	if m.switchTab(msg) {
		return nil
	}
	if m.actionKeys(msg) {
		return nil
	}
	if cmd, ok := m.agentActionKeys(msg); ok {
		return cmd
	}
	return m.tabKeys(msg)
}

// switchTab moves to the tab a number key names.
func (m *model) switchTab(msg tea.KeyPressMsg) bool {
	keys := m.shared.Keys
	for i, binding := range []key.Binding{keys.Tab1, keys.Tab2, keys.Tab3, keys.Tab4} {
		if key.Matches(msg, binding) {
			m.openTab(tab(i))
			return true
		}
	}
	return false
}

// openTab shows a tab, and points the log pane at the log it shows: the
// agent's on the Agents tab, the proxy's decisions on the Traffic tab, and
// on the Health tab the step that runs or stopped the bring-up, or else
// the log chosen there.
func (m *model) openTab(next tab) {
	m.tab = next
	m.shared.Logs.ClearFilter()
	switch next {
	case tabAgent:
		m.agents.ShowRight()
	case tabTraffic:
		m.traffic.Show()
	case tabHealth:
		m.health.Open()
	case tabHome, tabCount:
	}
	m.layout()
}

// tabKeys gives a key to the tab that shows. X on the Agents tab removes
// the selected agent.
func (m *model) tabKeys(msg tea.KeyPressMsg) tea.Cmd {
	switch m.tab {
	case tabAgent:
		if key.Matches(msg, m.shared.Keys.RemoveAgent) {
			m.removeAgent()
			return nil
		}
		return m.agents.Keys(msg)
	case tabTraffic:
		return m.traffic.Keys(msg)
	case tabHealth:
		return m.health.Keys(msg)
	case tabHome, tabCount:
	}
	return nil
}

// actionKeys handles the keys that act on the sandbox, and reports whether
// msg was one.
func (m *model) actionKeys(msg tea.KeyPressMsg) bool {
	keys := m.shared.Keys
	switch {
	case key.Matches(msg, keys.Start):
		m.toggleBringUp()
	case key.Matches(msg, keys.Checks):
		m.runChecks()
	case key.Matches(msg, keys.Restart):
		m.restartAgent()
	case key.Matches(msg, keys.Setup):
		m.openSettings()
	case key.Matches(msg, keys.Demo):
		m.openDemoPicker()
	case key.Matches(msg, keys.StopAll):
		m.stopAll()
	default:
		return false
	}
	return true
}

// agentActionKeys handles the keys that work with the agent, and the
// activity log.
func (m *model) agentActionKeys(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	keys := m.shared.Keys
	switch {
	case key.Matches(msg, keys.Agent):
		return m.agent(), true
	case key.Matches(msg, keys.Shell):
		return m.openAgent(true), true
	case key.Matches(msg, keys.Send):
		if m.tab != tabAgent {
			m.openTab(tabAgent)
		}
		return m.agents.Send(), true
	case key.Matches(msg, keys.Events):
		return m.openEvents(), true
	case key.Matches(msg, keys.AddAgent):
		m.addAgent()
		return nil, true
	case key.Matches(msg, keys.Activity):
		entries := m.health.LogEntries()
		m.showLog(entries[len(entries)-1])
	case key.Matches(msg, keys.Logs):
		m.health.OpenLogs()
		m.overlay = overlay{kind: overlayLogs}
	default:
		return nil, false
	}
	return nil, true
}

// showLog shows a log from the list of every log on the Health tab.
func (m *model) showLog(entry page.LogEntry) {
	if m.tab != tabHealth {
		m.openTab(tabHealth)
	}
	m.health.ShowLog(entry)
}

// logsKeys moves through the list of every log, and shows the one chosen
// on the Health tab.
func (m *model) logsKeys(msg tea.KeyPressMsg) {
	entry, pick := m.health.LogsKeys(msg)
	switch pick {
	case component.PickChosen:
		m.closeOverlay()
		m.showLog(entry)
	case component.PickClosed:
		m.closeOverlay()
	case component.PickNone:
	}
}

// agentListKeys moves through the agents, and switches to the one chosen.
func (m *model) agentListKeys(msg tea.KeyPressMsg) {
	email, pick := m.agents.PickerKeys(msg)
	switch pick {
	case component.PickChosen:
		m.closeOverlay()
		if email != "" {
			m.shared.Session.SelectAgent(email)
			m.syncAgents()
		}
	case component.PickClosed:
		m.closeOverlay()
	case component.PickNone:
	}
}

// openDemoPicker lists the demos, with the cursor on the first one that
// does not run now.
func (m *model) openDemoPicker() {
	if len(m.shared.DemoList()) == 0 {
		m.shared.Notify("No demo loads: "+component.FirstLine(m.shared.DemosErr.Error()), true)
		return
	}
	if m.busy() {
		return
	}
	m.demos.Open()
	m.overlay = overlay{kind: overlayDemo}
}

// demoKeys moves through the demos, and switches to the one chosen.
func (m *model) demoKeys(msg tea.KeyPressMsg) {
	name, pick := m.demos.Keys(msg)
	switch pick {
	case component.PickChosen:
		m.overlay = overlay{}
		m.switchDemo(name)
	case component.PickClosed:
		m.overlay = overlay{}
	case component.PickNone:
	}
}

// switchDemo asks the demo's questions with the demo already chosen. Its
// credentials come next, then the review, where enter brings the sandbox
// up with it.
func (m *model) switchDemo(name string) {
	m.shared.Record("▸", "Switching the demo to "+name)
	m.startWizard(screenDashboard, map[string]string{sandbox.QuestionDemo: name}, sandbox.SectionScenario)
}

// welcomeKeys handles a key on the welcome screen.
func (m *model) welcomeKeys(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case msg.String() == component.KeyEnter:
		m.startWizard(screenWelcome, nil, m.welcome.Fix...)
	case key.Matches(msg, m.shared.Keys.Setup) && !m.shared.FirstRun():
		m.showSettings(screenWelcome)
	case msg.String() == "r" && !m.welcome.Checking():
		return m.welcome.Preflight(m.shared.Session.PreflightItems())
	}
	return nil
}

// reviewKeys handles a key on the review screen.
func (m *model) reviewKeys(msg tea.KeyPressMsg) tea.Cmd {
	keys := m.shared.Keys
	switch {
	case msg.String() == component.KeyEnter, key.Matches(msg, keys.Start):
		return m.launch()
	case key.Matches(msg, keys.Setup):
		m.showSettings(screenReview)
	case key.Matches(msg, keys.Back):
		m.screen = screenDashboard
		if m.shared.SandboxUp() {
			m.shared.Notify("The sandbox still runs with the settings from before. Press u to bring it up "+
				"with the new ones.", false)
		}
	}
	return nil
}

// settingsKeys handles a key on the settings screen.
func (m *model) settingsKeys(msg tea.KeyPressMsg) tea.Cmd {
	keys := m.shared.Keys
	if m.settings.Move(msg) {
		return nil
	}
	switch {
	case msg.String() == component.KeyEnter:
		m.startWizard(screenSettings, nil, m.settings.Selected())
	case msg.String() == "a":
		m.startWizard(screenSettings, nil)
	case msg.String() == "F":
		m.forgetSettings()
	case key.Matches(msg, keys.Start):
		return m.launch()
	case key.Matches(msg, keys.Back):
		return m.leaveSettings()
	}
	return nil
}

// openWelcome shows the welcome screen: for a first run, or to fix the
// problems the settings have.
func (m *model) openWelcome() tea.Cmd {
	m.screen = screenWelcome
	return m.welcome.Open()
}

// startWizard lays the rail out and starts the setup for the sections, or
// for every section when there are none. preset answers choices by
// question, without asking them. While other work runs, the screen stays
// as it is.
func (m *model) startWizard(back screen, preset map[string]string, ids ...string) {
	if m.busy() {
		return
	}
	m.setup.Begin(ids...)
	m.wizardBack = back
	m.screen = screenWizard
	m.startSetup(preset, ids...)
}

// launch brings the sandbox up and shows the dashboard, when the settings
// have no problems.
func (m *model) launch() tea.Cmd {
	if len(m.shared.Problems) > 0 {
		m.shared.Notify("Fix the settings first.", true)
		return nil
	}
	if m.busy() {
		return nil
	}
	m.screen, m.tab = screenDashboard, tabHome
	m.health.Focus = page.PaneChecks
	m.agents.Reset()
	m.startBringUp(0, false)
	return m.poll()
}

// openSettings shows the settings with the demo selected, as the setting
// changed most.
func (m *model) openSettings() {
	if m.busy() {
		return
	}
	m.settings.SelectDemo()
	m.showSettings(screenDashboard)
}

// showSettings shows the settings, and remembers from for esc to go back
// to.
func (m *model) showSettings(from screen) {
	m.settingsBack = from
	m.screen = screenSettings
}

// leaveSettings goes back to the screen the settings came from. The
// welcome screen checks the machine again, and says what still needs a
// fix. With nothing left to fix, the review shows in its place.
func (m *model) leaveSettings() tea.Cmd {
	switch {
	case m.settingsBack != screenWelcome:
		m.screen = m.settingsBack
	case len(m.shared.Problems) == 0 && m.shared.Session.Local().Initialized():
		m.screen = screenReview
	default:
		return m.openWelcome()
	}
	return nil
}

// settingsBackWord names where esc goes from the settings.
func (m *model) settingsBackWord() string {
	switch m.settingsBack {
	case screenWelcome:
		return "back"
	case screenReview:
		return "review"
	case screenDashboard, screenWizard, screenSettings:
	}
	return "dashboard"
}

// forgetDoneMsg says how deleting the saved settings went.
type forgetDoneMsg struct{ err error }

// forgetSettings asks, then deletes the saved settings and the secrets in
// the keychain, in the background. The settings last for this session.
func (m *model) forgetSettings() {
	local := m.shared.Session.Local()
	store := local.Store
	if !store.Saved() {
		return
	}
	m.confirm("Forget the saved settings?", "This deletes "+sandbox.HomeRelative(store.Path())+" and the secrets in "+
		store.KeychainWhere()+".\nThe sandbox keeps them until you quit, and setup asks for them next time.",
		func() tea.Cmd {
			return func() tea.Msg {
				_, err := local.Forget()
				return forgetDoneMsg{err: err}
			}
		})
}

func (m *model) onForgetDone(msg forgetDoneMsg) {
	if msg.err != nil {
		m.shared.Notify("Forgetting the settings: "+component.FirstLine(msg.err.Error()), true)
		return
	}
	m.shared.Record("✓", "Deleted the saved settings")
	m.shared.Notify("Deleted the saved settings. They last until you quit.", false)
}

// ask shows the setup's question: on the wizard's screen, or else in a
// modal. A dialog that shows stays on top until it closes. A question from
// a setup that stopped goes unanswered.
func (m *model) ask(quest *page.Question) tea.Cmd {
	if !m.asking || m.stopAsking == nil {
		return nil
	}
	cmd := m.setup.Ask(quest, m.screen == screenWizard)
	if m.overlay.kind == overlayNone {
		m.overlay = overlay{kind: overlayQuestion}
	}
	return cmd
}

// answer passes msg to the setup's question, and closes it once answered.
// Esc stops the setup.
func (m *model) answer(msg tea.Msg) tea.Cmd {
	cmd, state := m.setup.Answer(msg)
	switch state {
	case page.Stopped:
		m.stopSetup()
		m.overlay = overlay{}
	case page.Answered:
		m.overlay = overlay{}
	case page.Asking:
	}
	return cmd
}

// busy reports whether a bring-up, an action or the setup runs, and says
// which in the footer. Work that changes the sandbox waits for it.
func (m *model) busy() bool {
	switch {
	case m.shared.Running:
		m.shared.Notify("A bring-up is running. Wait for it to finish, or press u to stop it.", true)
	case m.acting != "":
		m.shared.Notify(m.acting+" is running. Wait for it to finish.", true)
	case m.askTitle != "":
		m.shared.Notify("Adding an agent. Answer its question, or press esc to stop.", true)
	case m.asking:
		m.shared.Notify("Setup is open. Finish it first.", true)
	default:
		return false
	}
	return true
}
