package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

// Init opens the welcome screen when the settings have a problem, else the
// dashboard. It brings the sandbox up only when the bring-up starts with
// the tunnel and no tunnel runs from before.
func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{frameTick(), m.poll(), m.bus.next(), tea.RequestWindowSize, tea.RequestBackgroundColor}
	switch {
	case len(m.state.Setup.Problems) > 0:
		return tea.Batch(append(cmds, m.openWelcome())...)
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
		return m, m.setup.Resize()
	case tea.BackgroundColorMsg:
		m.ui.Styles, m.ui.Background = style.New(msg.IsDark()), msg.Color
	case askMsg:
		return m, m.ask(msg)
	case tea.KeyPressMsg:
		return m, m.keyPress(msg)
	}
	if m.overlay.kind == overlayQuestion {
		return m, m.answer(msg)
	}
	return m, nil
}

func (m *model) updateBackground(msg tea.Msg) (tea.Cmd, bool) {
	if cmd, handled := m.updateJobs(msg); handled {
		return cmd, true
	}
	if cmd, handled := m.updateAnswers(msg); handled {
		return cmd, true
	}
	switch msg := msg.(type) {
	case style.GlintMsg:
		return m.welcome.Glint.Move(msg.Now, m.screen == screenWelcome && m.state.Setup.Preflight.Checking()), true
	case frameMsg:
		m.ui.Frame++
		return tea.Batch(frameTick(), m.welcomeGlint()), true
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

// updateJobs handles what the jobs and the agent report.
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
		return m.updateOnboarding(msg)
	}
	return nil, true
}

// updateOnboarding handles what the machine checks and the setup report.
func (m *model) updateOnboarding(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case checkDoneMsg:
		return m.onCheck(msg), true
	case page.SectionMsg:
		m.setup.OnSection(msg)
	case setupDoneMsg:
		m.onSetupDone(msg)
	case forgetDoneMsg:
		m.onForgetDone(msg)
	case page.PresetMsg:
		m.setup.OnPreset(msg)
	default:
		return nil, false
	}
	return nil, true
}

// welcomeGlint starts the sheen while the welcome screen checks the machine.
func (m *model) welcomeGlint() tea.Cmd {
	if m.screen != screenWelcome || !m.state.Setup.Preflight.Checking() {
		return nil
	}
	return m.welcome.Glint.Start()
}

// keyPress gives a key to the open overlay, else to the screen. A question
// that waited opens once the overlay closes or its answer is handled.
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
	return m.screenLayer().keys(msg)
}

// wizardKeys stops the setup on esc between its questions. A question
// takes the keys itself.
func (m *model) wizardKeys(msg tea.KeyPressMsg) tea.Cmd {
	if msg.String() == component.KeyEsc {
		m.stopSetup()
	}
	return nil
}

func (m *model) overlayKeys(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	open, ok := m.overlayLayer()
	if !ok {
		return nil, false
	}
	return open.keys(msg), true
}

// closeKeys closes the overlay on any key.
func (m *model) closeKeys(tea.KeyPressMsg) tea.Cmd {
	m.closeOverlay()
	return nil
}

// quit asks how to leave. By default the sandbox keeps running. D asks,
// then stops everything first. The dialog reads what runs when it is drawn.
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
	case state.JobSetup:
		body = setupStops + "\n" + body
	case state.JobNone:
	}
	return body
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

// anythingRunning reports whether anything runs that D stops.
func (m *model) anythingRunning() bool {
	status := m.state.Reads.Status
	local := status.ClusterExists && !m.session.Local().Existing()
	return m.state.Run.Job.Busy() || status.Tunnel.Running || status.Mirror.Running || local
}

func (m *model) quitKeys(msg tea.KeyPressMsg) tea.Cmd {
	keepsRunning := m.anythingRunning()
	m.closeOverlay()
	switch msg.String() {
	case component.KeyEnter, "q", "y", component.KeyCtrlC:
		m.state.Run.CancelJob()
		m.stopSetup()
		return tea.Quit
	case "D":
		if !keepsRunning || m.stopAllWaits(true) {
			return nil
		}
		m.askStopAll(true)
	}
	return nil
}

// confirm asks a yes or no question. Only y or Y sends yes to Update.
func (m *model) confirm(title, body string, yes tea.Msg) {
	m.overlay = overlay{kind: overlayConfirm, title: title, body: body, yes: yes}
}

// confirmLater is confirm for background work. It waits while another
// overlay is open, so a key meant for that overlay does not answer it.
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
	m.closeOverlay()
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

// actionKeys handles the keys that act on the sandbox.
func (m *model) actionKeys(msg tea.KeyPressMsg) bool {
	keys := m.ui.Keys
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

// openDemoPicker lists the demos, with the cursor on the first one that
// does not run now.
func (m *model) openDemoPicker() {
	if len(m.state.DemoList()) == 0 {
		m.ui.Notify("No demo loads: "+component.FirstLine(m.state.Demos.Err.Error()), true)
		return
	}
	if m.busy() {
		return
	}
	m.demos.Open()
	m.overlay = overlay{kind: overlayDemo}
}

// demoKeys moves through the demos, and switches to the one chosen.
func (m *model) demoKeys(msg tea.KeyPressMsg) tea.Cmd {
	name, pick := m.demos.Keys(msg)
	switch pick {
	case component.PickChosen:
		m.overlay = overlay{}
		m.switchDemo(name)
	case component.PickClosed:
		m.overlay = overlay{}
	case component.PickNone:
	}
	return nil
}

// switchDemo asks the demo's questions with the demo already chosen.
func (m *model) switchDemo(name string) {
	m.state.Record("▸", "Switching the demo to "+name)
	m.startWizard(screenDashboard, map[string]string{sandbox.QuestionDemo: name}, sandbox.SectionScenario)
}

func (m *model) welcomeKeys(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case msg.String() == component.KeyEnter:
		m.startWizard(screenWelcome, nil, m.state.Setup.FixSections()...)
	case key.Matches(msg, m.ui.Keys.Setup) && !m.state.FirstRun():
		m.showSettings(screenWelcome)
	case msg.String() == "r" && !m.state.Setup.Preflight.Checking():
		return m.checkAgain()
	}
	return nil
}

// checkAgain reads the settings and checks the machine again.
func (m *model) checkAgain() tea.Cmd {
	m.state.RefreshSettings()
	if len(m.state.Setup.Problems) == 0 && m.session.Local().Initialized() {
		m.screen = screenReview
		return nil
	}
	return m.startPreflight()
}

func (m *model) reviewKeys(msg tea.KeyPressMsg) tea.Cmd {
	keys := m.ui.Keys
	switch {
	case msg.String() == component.KeyEnter, key.Matches(msg, keys.Start):
		return m.launch()
	case key.Matches(msg, keys.Setup):
		m.showSettings(screenReview)
	case key.Matches(msg, keys.Back):
		m.screen = screenDashboard
		if m.state.Up() {
			m.ui.Notify("The sandbox still runs with the settings from before. Press u to bring it up "+
				"with the new ones.", false)
		}
	}
	return nil
}

func (m *model) settingsKeys(msg tea.KeyPressMsg) tea.Cmd {
	keys := m.ui.Keys
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

// openWelcome shows the welcome screen and checks the machine.
func (m *model) openWelcome() tea.Cmd {
	m.screen = screenWelcome
	return m.startPreflight()
}

// startWizard starts the setup for the sections, or for every section when
// there are none. preset answers choices by question without asking them.
func (m *model) startWizard(back screen, preset map[string]string, ids ...string) {
	if m.busy() {
		return
	}
	m.setup.Begin(ids...)
	m.wizardBack = back
	m.screen = screenWizard
	m.startSetup(preset, ids...)
}

// launch brings the sandbox up and shows the dashboard.
func (m *model) launch() tea.Cmd {
	if len(m.state.Setup.Problems) > 0 {
		m.ui.Notify("Fix the settings first.", true)
		return nil
	}
	if m.busy() {
		return nil
	}
	m.screen = screenDashboard
	m.startBringUp(0, false)
	return m.poll()
}

// openSettings shows the settings with the demo selected.
func (m *model) openSettings() {
	if m.busy() {
		return
	}
	m.settings.SelectDemo()
	m.showSettings(screenDashboard)
}

// showSettings shows the settings, and esc goes back to from.
func (m *model) showSettings(from screen) {
	m.settingsBack = from
	m.screen = screenSettings
}

// leaveSettings goes back to the screen the settings came from.
func (m *model) leaveSettings() tea.Cmd {
	switch {
	case m.settingsBack != screenWelcome:
		m.screen = m.settingsBack
	case len(m.state.Setup.Problems) == 0 && m.session.Local().Initialized():
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

// forgetSettings asks whether to delete the saved settings and secrets.
func (m *model) forgetSettings() {
	local := m.session.Local()
	store := local.Store
	if !store.Saved() {
		return
	}
	m.confirm("Forget the saved settings?", "This deletes "+sandbox.HomeRelative(store.Path())+" and the secrets in "+
		store.KeychainWhere()+".\nThe sandbox keeps them until you quit, and setup asks for them next time.",
		forgetSettingsMsg{})
}

// forget deletes the saved settings and secrets in the background.
func (m *model) forget() tea.Cmd {
	local := m.session.Local()
	if !local.Store.Saved() {
		return nil
	}
	return func() tea.Msg {
		_, err := local.Forget()
		return forgetDoneMsg{err: err}
	}
}

func (m *model) onForgetDone(msg forgetDoneMsg) {
	if msg.err != nil {
		m.ui.Notify("Forgetting the settings: "+component.FirstLine(msg.err.Error()), true)
		return
	}
	m.state.Record("✓", "Deleted the saved settings")
	m.ui.Notify("Deleted the saved settings. They last until you quit.", false)
}

// ask shows the setup's question. A dialog that shows stays on top until it
// closes. A question from a setup that stopped goes unanswered.
func (m *model) ask(msg askMsg) tea.Cmd {
	if !m.asking() {
		return nil
	}
	m.replies = msg.replies
	cmd := m.setup.Ask(msg.question)
	if m.overlay.kind == overlayNone {
		m.overlay = overlay{kind: overlayQuestion}
	}
	return cmd
}

// answer passes msg to the setup's question, and closes it once answered.
func (m *model) answer(msg tea.Msg) tea.Cmd {
	cmd, state := m.setup.Answer(msg)
	switch state {
	case page.Stopped:
		m.stopSetup()
		m.overlay = overlay{}
	case page.Answered:
		text, yes := m.setup.Reply()
		m.replies <- reply{text: text, yes: yes}
		m.overlay = overlay{}
	case page.Asking:
	}
	return cmd
}
