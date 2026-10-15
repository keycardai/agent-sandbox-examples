// Package tui is the sandbox's terminal UI. It sets the sandbox up through
// the same questions as `sandbox init`, and brings it up one step at a
// time. Then Home says what Keycard did for the agent and what to try. The
// tunnel and the mirror run as services, so quitting the TUI leaves the
// sandbox up, and starting it again shows it.
//
// The TUI keeps its model, its views and its controller apart. The state
// package holds what the TUI knows about the sandbox, and the rules that
// read and change it. The page package draws it: each screen, with the UI
// the pages share. The root model in this package is the controller: it
// routes each message and key, does the work in the background, such as
// the bring-up, the actions, the polling and the logs it follows, and
// changes the state with what that work finds. The component package draws
// the parts of the screen, style holds the colors, and proxylog reads the
// egress proxy's audit log.
package tui

import (
	"context"
	"fmt"
	"sync"
	"time"

	"charm.land/bubbles/v2/help"
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
	// busSize is how many messages can queue before Send waits.
	busSize = 1024
	// shutdownWait bounds Run's wait for the background work to end.
	shutdownWait = 5 * time.Second
)

// screen is what fills the TUI.
type screen int

const (
	// screenDashboard shows the sandbox's Home.
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
	help    help.Model
	// Background work runs in ctx, and Run waits for its goroutines in jobs.
	ctx  context.Context //nolint:containedctx // The model owns its background work, which ends with Run's context.
	jobs sync.WaitGroup

	screen  screen
	overlay overlay
	// waiting is a question that waits for the open overlay to close.
	waiting *overlay

	welcome  *page.Welcome
	setup    *page.Setup
	settings *page.Settings
	home     *page.Home
	demos    *page.Demos

	// polling counts the reads on their way. tickPlanned is set while the
	// next poll waits, and stale when a read started before a job ended.
	polling     int
	tickPlanned bool
	stale       bool
	// trafficStop stops following the proxy's audit log.
	trafficStop context.CancelFunc
	// shownPod is the selected agent's pod, which Home shows.
	shownPod string

	// wizardBack is the screen to go back to when the wizard stops, and
	// settingsBack the one the settings came from, where esc goes back to.
	wizardBack   screen
	settingsBack screen
}

// overlay is what covers the screen: nothing, the help, a confirmation, a
// setup question, the quit dialog or the list of demos. It holds data only:
// a confirmation's yes is the message that y sends, which Update acts on
// then.
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
	// overlayQuestion is the setup's question, which the setup page keeps.
	// It is part of the wizard's screen.
	overlayQuestion
	overlayQuit
	overlayDemo
)

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
	// Reading the settings' problems reads the keychain, which can wait for
	// the operator, so it runs before the program starts.
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
	root := &model{session: session, state: sandbox, ui: pageUI, bus: newBus(ctx), help: help.New(), ctx: ctx}
	root.welcome = page.NewWelcome(pageUI, sandbox)
	root.setup = page.NewSetup(pageUI, sandbox)
	root.settings = page.NewSettings(pageUI, sandbox)
	root.home = page.NewHome(pageUI, sandbox)
	root.demos = page.NewDemos(pageUI, sandbox)
	return root
}

// shutdown cancels the work in the background with cancel, ends the setup,
// and waits up to shutdownWait for the work to end.
func (m *model) shutdown(cancel context.CancelFunc) {
	cancel()
	m.stopSetup()
	m.welcome.Stop()
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
