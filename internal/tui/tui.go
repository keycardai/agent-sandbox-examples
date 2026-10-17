// Package tui is the sandbox's terminal UI. It sets the sandbox up through
// the same questions as `sandbox init`, brings it up one step at a time,
// and shows the agents and their traffic. Quitting it leaves the sandbox up.
//
// The state package holds what the TUI knows and the rules that change it,
// and the page package draws it. The root model here is the controller: it
// routes each message and key, runs the work in the background, and changes
// the state with what that work finds.
package tui

import (
	"context"
	"fmt"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
)

const (
	// frameEvery is how often the TUI redraws changed logs and spinners.
	frameEvery = 120 * time.Millisecond
	// pollEvery is how often the TUI reads the pods and the services.
	pollEvery = 2 * time.Second
	// logTail is how many lines of a container's log the TUI starts with.
	logTail = 500
	// busSize is how many messages can queue before Send waits.
	busSize = 1024
	// shutdownWait bounds Run's wait for the background work to end.
	shutdownWait = 5 * time.Second
)

// screen is what fills the TUI.
type screen int

const (
	// screenDashboard shows the sandbox in tabs: Home, Agents and Traffic.
	screenDashboard screen = iota
	// screenWelcome greets a first run, or says what the settings lack.
	screenWelcome
	// screenWizard asks the setup's questions one step at a time.
	screenWizard
	// screenReview shows what setup chose, before the launch.
	screenReview
	// screenSettings lists the settings to change one.
	screenSettings
)

// tab is a view of the dashboard.
type tab int

const (
	tabHome tab = iota
	tabAgent
	tabTraffic
	tabCount
)

// Options configure the TUI.
type Options struct {
	// Session runs the steps. Its Local must have Getenv set, so that setup
	// reads KEYCARD_CLIENT_ID and KEYCARD_CLIENT_SECRET.
	Session *sandbox.Session
}

// model is the TUI's controller. Update runs on one goroutine. Background
// work writes only to logs, which lock, and sends messages through bus.
type model struct {
	session *sandbox.Session
	state   *state.Sandbox
	ui      *page.UI
	bus     *bus
	// Background work runs in ctx, and Run waits for its goroutines in jobs.
	ctx  context.Context //nolint:containedctx // The model owns its background work, which ends with Run's context.
	jobs sync.WaitGroup

	screen  screen
	tab     tab
	overlay overlay
	// waiting is a question that waits for the open overlay to close.
	waiting *overlay

	welcome  *page.Welcome
	setup    *page.Setup
	settings *page.Settings
	home     *page.Home
	agents   *page.Agents
	traffic  *page.Traffic
	logs     *page.Logs
	demos    *page.Demos

	// polling counts the reads on their way. tickPlanned is set while the
	// next poll waits, and stale when a read started before a job ended.
	polling     int
	tickPlanned bool
	stale       bool
	// stream names the container log the TUI follows, and streamFinished is
	// set when it was read once. trafficStop stops following the proxy's log.
	stream         string
	streamStop     context.CancelFunc
	streamFinished bool
	trafficStop    context.CancelFunc
	// The last machine checks run in preflightCtx, until preflightStop.
	preflightCtx  context.Context //nolint:containedctx // The checks of one run go on across messages.
	preflightStop context.CancelFunc
	// shownPod is the selected agent's pod, which every tab shows.
	shownPod string
	// questions ends the setup's run of questions while it asks, and
	// replies takes the answer to the question that shows.
	questions chan struct{}
	replies   chan<- reply

	// askTitle titles the questions outside the wizard, or is "" for setup.
	askTitle string
	// The wizard and the settings go back to wizardBack and settingsBack.
	wizardBack   screen
	settingsBack screen

	// mouse turns on mouse reporting. It is off by default, for copying text.
	mouse bool
}

// overlay is what covers the screen. It holds data only: a confirmation's
// yes is the message that y sends, which Update acts on then.
type overlay struct {
	kind  overlayKind
	title string
	body  string
	yes   tea.Msg
}

type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayHelp
	overlayConfirm
	// overlayQuestion is the setup's question. In the wizard, it is part of
	// the screen.
	overlayQuestion
	overlayQuit
	overlayDemo
	// overlayLogs lists every log, and overlayLog shows the one chosen.
	overlayLogs
	overlayLog
	// overlayAgents lists every agent, to switch to another.
	overlayAgents
)

// layer is how a screen or an overlay draws, and how it takes a key.
type layer struct {
	view func() string
	keys func(msg tea.KeyPressMsg) tea.Cmd
}

// screenLayer is how the screen that shows draws and takes a key.
func (m *model) screenLayer() layer {
	switch m.screen {
	case screenWelcome:
		return layer{m.welcome.View, m.welcomeKeys}
	case screenWizard:
		return layer{m.setup.WizardView, m.wizardKeys}
	case screenReview:
		return layer{m.setup.ReviewView, m.reviewKeys}
	case screenSettings:
		return layer{func() string { return m.settings.View(m.settingsBackWord()) }, m.settingsKeys}
	case screenDashboard:
	}
	return layer{m.dashboardView, m.dashboardKeys}
}

// overlayLayer is how the open overlay draws and takes a key, or false
// when none is open. An overlay that draws nothing leaves the screen as is.
func (m *model) overlayLayer() (layer, bool) {
	switch m.overlay.kind {
	case overlayHelp:
		return layer{m.helpModal, m.closeKeys}, true
	case overlayConfirm:
		return layer{m.confirmModal, m.confirmKeys}, true
	case overlayQuit:
		return layer{m.quitModal, m.quitKeys}, true
	case overlayDemo:
		return layer{m.demos.View, m.demoKeys}, true
	case overlayLogs:
		return layer{m.logs.ListView, m.logsKeys}, true
	case overlayLog:
		return layer{m.logs.View, m.logKeys}, true
	case overlayAgents:
		return layer{m.agents.PickerView, m.agentListKeys}, true
	case overlayQuestion:
		return layer{m.questionModal, func(msg tea.KeyPressMsg) tea.Cmd { return m.answer(msg) }}, true
	case overlayNone:
	}
	return layer{}, false
}

// bus queues messages from background work for Update, so a step never waits.
type bus struct {
	ch chan tea.Msg
	// done ends a wait on the bus once the TUI stops.
	done <-chan struct{}
}

// busMsg is a message that came through the bus.
type busMsg struct{ msg tea.Msg }

// Run runs the TUI until the operator quits, or ctx ends. The sandbox keeps
// running. Run stops the TUI's background work and waits for it to end.
func Run(ctx context.Context, opts Options) error {
	ctx, cancel := context.WithCancel(ctx)
	root := newModel(ctx, opts.Session)
	defer root.shutdown(cancel)
	// The keychain can wait for the operator, so this runs before the TUI.
	root.state.RefreshSettings()
	if _, err := tea.NewProgram(root, tea.WithContext(ctx)).Run(); err != nil {
		return fmt.Errorf("running the program: %w", err)
	}
	return nil
}

// newModel returns the TUI for a session, with its background work in ctx.
func newModel(ctx context.Context, session *sandbox.Session) *model {
	pageUI := page.NewUI()
	sandbox := state.New(session, pageUI.Logs.Log)
	root := &model{session: session, state: sandbox, ui: pageUI, bus: newBus(ctx), ctx: ctx}
	root.welcome = page.NewWelcome(pageUI, sandbox)
	root.setup = page.NewSetup(pageUI, sandbox)
	root.settings = page.NewSettings(pageUI, sandbox)
	root.home = page.NewHome(pageUI, sandbox)
	root.agents = page.NewAgents(pageUI, sandbox)
	root.traffic = page.NewTraffic(pageUI, sandbox)
	root.logs = page.NewLogs(pageUI, sandbox)
	root.demos = page.NewDemos(pageUI, sandbox)
	if len(sandbox.Run.Steps) > 0 {
		pageUI.Logs.Show(state.StepSource(sandbox.Run.Steps[0].Step.ID))
	}
	return root
}

// shutdown cancels the background work, and waits up to shutdownWait for it.
func (m *model) shutdown(cancel context.CancelFunc) {
	cancel()
	m.stopSetup()
	m.stopPreflight()
	done := make(chan struct{})
	go func() {
		m.jobs.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownWait):
	}
}

// newBus returns a bus whose waits end with ctx.
func newBus(ctx context.Context) *bus {
	return &bus{ch: make(chan tea.Msg, busSize), done: ctx.Done()}
}

// Send queues msg for Update, or drops it once the TUI stopped.
func (b *bus) Send(msg any) {
	select {
	case b.ch <- msg:
	case <-b.done:
	}
}

// next waits for the next queued message, or for the TUI to stop.
func (b *bus) next() tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-b.ch:
			return busMsg{msg: msg}
		case <-b.done:
			return nil
		}
	}
}
