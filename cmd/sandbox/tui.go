package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

// The terminal UI runs one Bubble Tea program for a whole command. The
// command runs in its own goroutine and reports through the program: steps
// arrive as events and show a spinner while they run, and a prompt is a form
// the program shows until the person answers it. Finished steps and answers
// move into the terminal's scrollback in order, so the live area holds only
// what is happening now.

// quitWait is how long the CLI waits for the command to stop after the
// program quits.
const quitWait = 5 * time.Second

var (
	// errCancelled is the person stopping the command.
	errCancelled = errors.New("cancelled")
	errTUI       = errors.New("the terminal UI failed")
)

// presenter is the command's side of the terminal UI.
type presenter struct {
	program *tea.Program
	theme   theme
	exited  chan struct{}
}

func (p *presenter) send(msg tea.Msg) { p.program.Send(msg) }

// ask shows the form and waits for the answer.
func (p *presenter) ask(form *huh.Form) error {
	reply := make(chan error, 1)
	p.send(askMsg{form: form, reply: reply})
	select {
	case err := <-reply:
		if errors.Is(err, huh.ErrUserAborted) {
			return errCancelled
		}
		return err
	case <-p.exited:
		return errCancelled
	}
}

// show adds a block to the scrollback, rendered at the terminal's width.
func (p *presenter) show(render func(width int) string) { p.send(renderMsg(render)) }

// finish shows how the command ended, unless it already did, and lets the
// program quit once the scrollback has everything.
func (p *presenter) finish(err error) error {
	defer p.send(finishMsg{})
	switch {
	case err == nil, errors.Is(err, errReported):
		return err
	case errors.Is(err, errCancelled), errors.Is(err, context.Canceled):
		p.show(func(int) string { return "\n  " + p.theme.paint(p.theme.yellow, "■") + " Stopped." })
		return errCancelled
	default:
		p.show(func(width int) string { return p.theme.failure("Could not continue", err, width) })
		return errReported
	}
}

// runTUI runs fn under the terminal UI. phases, when set, are the names of
// the steps the tracker above the live area shows.
func runTUI(ctx context.Context, look theme, phases []string, action func(context.Context, *presenter) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	view := &presenter{program: tea.NewProgram(newModel(look, phases, cancel)), theme: look, exited: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- view.finish(action(ctx, view)) }()

	_, runErr := view.program.Run()
	close(view.exited)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			return err
		}
		if runErr != nil {
			return fmt.Errorf("%w: %w", errTUI, runErr)
		}
		return nil
	case <-time.After(quitWait):
		return errCancelled
	}
}

type (
	askMsg struct {
		form  *huh.Form
		reply chan error
	}
	formDoneMsg   struct{}
	formCancelMsg struct{}
	phaseMsg      int
	renderMsg     func(width int) string
	finishMsg     struct{}
	printedMsg    struct{}
)

type liveStep struct {
	id    int
	title string
	start time.Time
}

type model struct {
	theme  theme
	phases []string
	phase  int
	// steps are the steps running now.
	steps   []liveStep
	spinner spinner.Model
	form    *huh.Form
	reply   chan error
	width   int
	// queue is text waiting to print into the scrollback, and printing says
	// a print is under way.
	queue    []string
	printing bool
	// finishing says the command has ended, and done that the program is
	// quitting.
	finishing  bool
	done       bool
	cancel     context.CancelFunc
	cancelling bool
}

// newModel makes the program's model. cancel stops the command.
func newModel(look theme, phases []string, cancel context.CancelFunc) *model {
	return &model{
		theme:  look,
		phases: phases,
		cancel: cancel,
		spinner: spinner.New(
			spinner.WithSpinner(spinner.MiniDot),
			spinner.WithStyle(lipgloss.NewStyle().Foreground(look.accent)),
		),
	}
}

func (m *model) Init() tea.Cmd { return m.spinner.Tick }

//nolint:ireturn // Bubble Tea's Model interface fixes this signature.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" && m.form == nil {
			return m, m.interrupt()
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	default:
		if cmd, handled := m.command(msg); handled {
			return m, cmd
		}
	}
	if m.form != nil {
		_, cmd := m.form.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) View() tea.View {
	if m.done {
		return tea.NewView("")
	}
	var view strings.Builder
	if len(m.phases) > 0 {
		view.WriteString("\n" + m.theme.tracker(m.phases, m.phase, m.contentWidth()) + "\n")
	}
	for _, step := range m.steps {
		line := "  " + m.spinner.View() + " " + step.title
		if elapsed := time.Since(step.start); elapsed >= time.Second {
			line += m.theme.paint(m.theme.faint, "  "+elapsed.Truncate(time.Second).String())
		}
		view.WriteString("\n" + ansi.Truncate(line, m.contentWidth(), "…"))
	}
	if m.cancelling {
		view.WriteString("\n  " + m.theme.paint(m.theme.yellow, "Stopping… press ctrl+c again to quit now"))
	}
	if m.form != nil {
		view.WriteString("\n" + m.form.View())
	}
	return tea.NewView(view.String())
}

// command handles a message from the command's side of the UI.
func (m *model) command(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case sandbox.Event:
		return m.event(msg), true
	case renderMsg:
		return m.commit(msg(m.contentWidth())), true
	case phaseMsg:
		m.phase = int(msg)
		return nil, true
	case askMsg:
		return m.startForm(msg), true
	case formDoneMsg:
		m.endForm(nil)
		return nil, true
	case formCancelMsg:
		m.endForm(huh.ErrUserAborted)
		return nil, true
	case printedMsg:
		m.printing = false
		return m.flush(), true
	case finishMsg:
		m.finishing = true
		return m.flush(), true
	default:
		return nil, false
	}
}

// interrupt stops the command on the first ctrl+c, and quits on the second.
func (m *model) interrupt() tea.Cmd {
	if m.cancelling {
		m.done = true
		return tea.Quit
	}
	m.cancelling = true
	m.cancel()
	return nil
}

func (m *model) startForm(msg askMsg) tea.Cmd {
	m.form, m.reply = msg.form, msg.reply
	m.form.SubmitCmd = func() tea.Msg { return formDoneMsg{} }
	m.form.CancelCmd = func() tea.Msg { return formCancelMsg{} }
	m.form.WithTheme(m.theme.forms()).WithWidth(min(m.contentWidth(), formWidth))
	return m.form.Init()
}

func (m *model) endForm(err error) {
	reply := m.reply
	m.form, m.reply = nil, nil
	reply <- err
}

func (m *model) event(event sandbox.Event) tea.Cmd {
	if event.Kind == sandbox.EventStepStart {
		m.steps = append(m.steps, liveStep{id: event.ID, title: event.Text, start: time.Now()})
		return nil
	}
	if event.Kind == sandbox.EventStepEnd {
		m.steps = slices.DeleteFunc(m.steps, func(step liveStep) bool { return step.id == event.ID })
	}
	return m.commit(m.theme.event(event, m.contentWidth()))
}

// commit queues text for the scrollback.
func (m *model) commit(text string) tea.Cmd {
	m.queue = append(m.queue, text)
	return m.flush()
}

// flush prints the queued text above the live area, one batch at a time so
// the scrollback keeps the order things happened in. Once the command has
// finished and the queue is empty, it quits.
func (m *model) flush() tea.Cmd {
	if m.printing {
		return nil
	}
	if len(m.queue) == 0 {
		if m.finishing {
			m.done = true
			return tea.Quit
		}
		return nil
	}
	text := strings.Join(m.queue, "\n")
	m.queue, m.printing = nil, true
	return tea.Sequence(tea.Println(text), func() tea.Msg { return printedMsg{} })
}

func (m *model) contentWidth() int {
	if m.width <= 0 {
		return defaultWidth
	}
	return min(m.width, maxWidth)
}
