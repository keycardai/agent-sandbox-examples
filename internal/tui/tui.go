// Package tui is the sandbox's terminal UI. It brings the sandbox up one
// step at a time, with the settings `sandbox init` saved. Then Home says
// what Keycard did for the agent and what to try. The tunnel and the
// mirror run as services, so quitting the TUI leaves the sandbox up, and
// starting it again shows it.
//
// The TUI keeps its model, its views and its controller apart. The state
// package holds what the TUI knows about the sandbox, and the rules that
// read and change it. The page package draws it: Home, with the UI the
// pages share. The root model in this package is the controller: it routes
// each message and key, does the work in the background, such as the
// bring-up, the actions, the polling and the logs it follows, and changes
// the state with what that work finds. The component package draws the
// parts of the screen, style holds the colors, and proxylog reads the
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
	// frameEvery is how often the TUI redraws a log that changed, and turns
	// its spinners. Log lines between frames draw together.
	frameEvery = 120 * time.Millisecond
	// pollEvery is how often the TUI reads the pods and the services.
	pollEvery = 2 * time.Second
	// busSize is how many messages background work can queue before Send
	// waits.
	busSize = 1024
	// shutdownWait bounds how long Run waits for the work in the background
	// to end, once it cancels it.
	shutdownWait = 5 * time.Second
)

// Options configure the TUI.
type Options struct {
	// Session runs the steps. Its Local must have Getenv set, so that setup
	// reads KEYCARD_CLIENT_ID and KEYCARD_CLIENT_SECRET.
	Session *sandbox.Session
}

// model is the root of the TUI, its controller. It routes each message and
// key, runs the work that changes the sandbox, and keeps what that work
// found in state, which the pages draw with ui. Update runs on one
// goroutine. Background work writes only to logs, which lock, and sends
// messages through bus.
type model struct {
	session *sandbox.Session
	state   *state.Sandbox
	ui      *page.UI
	bus     *bus
	help    help.Model
	// ctx is what the work in the background runs in, and ends when Run
	// returns. jobs counts that work's goroutines, for Run to wait for.
	ctx  context.Context //nolint:containedctx // The model owns its background work, which ends with Run's context.
	jobs sync.WaitGroup

	overlay overlay
	// waiting is a question that came from background work while another
	// overlay was open. It opens once that overlay closes.
	waiting *overlay

	home *page.Home

	// polling counts the reads of the sandbox on their way. The next poll
	// starts pollEvery after the last read comes back, and tickPlanned is
	// set while it waits, so that one poll is planned at a time.
	polling     int
	tickPlanned bool
	// trafficStop stops following the proxy's audit log.
	trafficStop context.CancelFunc
	// shownPod is the selected agent's pod, which Home shows.
	shownPod string
}

// overlay is what covers the screen: nothing, the help, a confirmation or
// the quit dialog. It holds data only: a confirmation's yes is the message
// that y sends, which Update acts on then.
type overlay struct {
	kind  overlayKind
	title string
	body  string
	yes   tea.Msg
	// keepsRunning is set on the quit dialog when part of the sandbox runs
	// and stays up after the quit.
	keepsRunning bool
}

type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayHelp
	overlayConfirm
	overlayQuit
)

// bus queues messages from background work for Update, so that a step
// never waits for the TUI.
type bus struct {
	ch chan tea.Msg
	// done ends a wait on the bus once the TUI stops.
	done <-chan struct{}
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
	if _, err := tea.NewProgram(root, tea.WithContext(ctx)).Run(); err != nil {
		return fmt.Errorf("running the program: %w", err)
	}
	return nil
}

// newModel returns the TUI for a session. Its work in the background runs
// in ctx.
func newModel(ctx context.Context, session *sandbox.Session) *model {
	ui := page.NewUI()
	sandbox := state.New(session, ui.Logs.Log)
	root := &model{session: session, state: sandbox, ui: ui, bus: newBus(ctx), help: help.New(), ctx: ctx}
	root.home = page.NewHome(ui, sandbox)
	return root
}

// shutdown cancels the work in the background with cancel, and waits up to
// shutdownWait for the work to end.
func (m *model) shutdown(cancel context.CancelFunc) {
	cancel()
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
