// Package tui is the sandbox's terminal UI. It sets the sandbox up through
// the same questions as `sandbox init`, and brings it up one step at a
// time. Then Home says what Keycard did for the agent and what to try, the
// Agents tab lists an agent for each person, sends requests from the one
// selected and shows what came back, the Traffic tab shows each decision of
// its egress proxy, and the Health tab shows the checks, its pod and every
// log. The tunnel and the mirror run as services, so quitting the TUI
// leaves the sandbox up, and starting it again shows it.
//
// The root model in this package routes each message and key, and does
// the work that spans the screens: the bring-up, the actions, the polling
// and the logs it follows. Each screen and tab is in the page package,
// which draws it and keeps its state. The component package draws the
// parts the screens share, style holds the colors, and proxylog reads the
// egress proxy's audit log.
package tui

import (
	"context"
	"fmt"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
)

const (
	// frameEvery is how often the TUI redraws a log that changed, and turns
	// its spinners. Log lines between frames draw together.
	frameEvery = 120 * time.Millisecond
	// pollEvery is how often the TUI reads the pods and the services.
	pollEvery = 2 * time.Second
	// logTail is how many lines of a container's log the TUI starts with.
	logTail = 500
	// busSize is how many messages background work can queue before Send
	// waits.
	busSize = 1024
	// shutdownWait bounds how long Run waits for the work in the background
	// to end, once it cancels it.
	shutdownWait = 5 * time.Second
)

// screen is what fills the TUI.
type screen int

const (
	// screenDashboard shows the sandbox in tabs: Home, Agents, Traffic and
	// Health.
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

// tab is a view of the dashboard. Home says what happens and what to do,
// Agents is for working with the agents, Traffic for what they ask for,
// and Health for how the sandbox is provisioned.
type tab int

const (
	tabHome tab = iota
	tabAgent
	tabTraffic
	tabHealth
	tabCount
)

// Options configure the TUI.
type Options struct {
	// Session runs the steps. Its Local must have Getenv set, so that setup
	// reads KEYCARD_CLIENT_ID and KEYCARD_CLIENT_SECRET.
	Session *sandbox.Session
}

// model is the root of the TUI. Update runs on one goroutine. Background
// work writes only to logs, which lock, and sends messages through bus.
type model struct {
	shared *page.Shared
	bus    *bus
	// ctx is what the work in the background runs in, and ends when Run
	// returns. jobs counts that work's goroutines, for Run to wait for.
	ctx  context.Context //nolint:containedctx // The model owns its background work, which ends with Run's context.
	jobs sync.WaitGroup

	screen  screen
	tab     tab
	overlay overlay

	welcome  *page.Welcome
	setup    *page.Setup
	settings *page.Settings
	home     *page.Home
	agents   *page.Agents
	traffic  *page.Traffic
	health   *page.Health
	demos    *page.Demos

	// cancel stops a running bring-up, and downAfterBringUp stops
	// everything once the bring-up has stopped.
	cancel           context.CancelFunc
	downAfterBringUp bool
	// acting names the action that runs, or is "". One action runs at a
	// time, and never with a bring-up or the setup.
	acting string
	// polling counts the reads of the sandbox on their way. The next poll
	// starts pollEvery after the last read comes back, and tickPlanned is
	// set while it waits, so that one poll is planned at a time.
	polling     int
	tickPlanned bool
	// stream names the container log the TUI follows, and streamStop stops
	// following it. trafficStop stops following the proxy's audit log.
	stream      string
	streamStop  context.CancelFunc
	trafficStop context.CancelFunc
	// shownPod is the selected agent's pod, which every tab shows.
	shownPod string

	// asking is set while the setup wizard runs. stopAsking ends its
	// questions, and cancelSetup the work it waits on. askTitle titles the
	// questions outside the wizard, or is "" for setup.
	asking      bool
	stopAsking  chan struct{}
	cancelSetup context.CancelFunc
	askTitle    string
	// wizardBack is the screen to go back to when the wizard stops, and
	// settingsBack the one the settings came from, where esc goes back to.
	wizardBack   screen
	settingsBack screen

	// mouse turns on mouse reporting, for the wheel. It is off by default,
	// so the terminal can select and copy text.
	mouse bool
	// quitAfterAction quits once the running action is done.
	quitAfterAction bool
}

// overlay is what covers the screen: nothing, the help, a confirmation, a
// setup question, the quit dialog or a list to choose from.
type overlay struct {
	kind    overlayKind
	title   string
	body    string
	confirm func() tea.Cmd
	// keepsRunning is set on the quit dialog when part of the sandbox runs
	// and stays up after the quit.
	keepsRunning bool
}

type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayHelp
	overlayConfirm
	// overlayQuestion is the setup's question outside the wizard, which
	// the setup page keeps. In the wizard, it is part of the screen.
	overlayQuestion
	overlayQuit
	overlayDemo
	// overlayLogs lists every log, for looking into a problem.
	overlayLogs
	// overlayAgents lists every agent, to switch to another.
	overlayAgents
)

// bus queues messages from background work for Update, so that a step
// never waits for the TUI.
type bus struct {
	ch chan tea.Msg
}

// busMsg is a message that came through the bus.
type busMsg struct{ msg tea.Msg }

// Run runs the TUI until the operator quits, or ctx ends. The sandbox
// keeps running. Before it returns, Run stops the work the TUI started in
// the background, such as a bring-up or an action, and waits for it.
func Run(ctx context.Context, opts Options) error {
	ctx, cancel := context.WithCancel(ctx)
	root := newModel(ctx, opts.Session)
	defer root.shutdown(cancel)
	// Reading the settings' problems reads the keychain, which can wait for
	// the operator, so it runs before the program starts.
	root.shared.RefreshSettings()
	if _, err := tea.NewProgram(root, tea.WithContext(ctx)).Run(); err != nil {
		return fmt.Errorf("running the program: %w", err)
	}
	return nil
}

// newModel returns the TUI for a session. Its work in the background runs
// in ctx.
func newModel(ctx context.Context, session *sandbox.Session) *model {
	shared := page.NewShared(session)
	root := &model{shared: shared, bus: newBus(), ctx: ctx}
	root.welcome = page.NewWelcome(shared)
	root.setup = page.NewSetup(shared)
	root.settings = page.NewSettings(shared)
	root.home = page.NewHome(shared)
	root.agents = page.NewAgents(shared, root)
	root.traffic = page.NewTraffic(shared)
	root.health = page.NewHealth(shared, root)
	root.demos = page.NewDemos(shared)
	if len(shared.Steps) > 0 {
		shared.Logs.Show(page.StepSource(shared.Steps[0].Step.ID))
	}
	return root
}

// shutdown cancels the work in the background with cancel, ends the setup,
// and waits up to shutdownWait for the work to end.
func (m *model) shutdown(cancel context.CancelFunc) {
	cancel()
	m.stopSetup()
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

func newBus() *bus {
	return &bus{ch: make(chan tea.Msg, busSize)}
}

// Send queues msg for Update.
func (b *bus) Send(msg any) {
	b.ch <- msg
}

// next waits for the next queued message.
func (b *bus) next() tea.Cmd {
	return func() tea.Msg { return busMsg{msg: <-b.ch} }
}
