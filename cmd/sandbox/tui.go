package main

import (
	"context"
	"errors"
	"fmt"
	"image/color"
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

var (
	colorAccent     = lipgloss.Color("#A78BFA")
	colorAccentDeep = lipgloss.Color("#7C3AED")
	colorGreen      = lipgloss.Color("#34D399")
	colorRed        = lipgloss.Color("#F87171")
	colorYellow     = lipgloss.Color("#FBBF24")
	colorDim        = lipgloss.Color("#8B8FA3")
	colorFaint      = lipgloss.Color("#4B5068")

	styleAccent = lipgloss.NewStyle().Foreground(colorAccent)
	styleBold   = lipgloss.NewStyle().Bold(true)
	styleGreen  = lipgloss.NewStyle().Foreground(colorGreen)
	styleRed    = lipgloss.NewStyle().Foreground(colorRed)
	styleYellow = lipgloss.NewStyle().Foreground(colorYellow)
	styleDim    = lipgloss.NewStyle().Foreground(colorDim)
	styleFaint  = lipgloss.NewStyle().Foreground(colorFaint)
)

// keycardTheme styles the forms in the CLI's colors.
var keycardTheme = huh.ThemeFunc(func(isDark bool) *huh.Styles {
	t := huh.ThemeCharm(isDark)
	t.Focused.Base = t.Focused.Base.BorderForeground(colorAccentDeep)
	t.Focused.Card = t.Focused.Base
	t.Focused.Title = t.Focused.Title.Foreground(colorAccent).Bold(true)
	t.Focused.NoteTitle = t.Focused.NoteTitle.Foreground(colorAccent).Bold(true)
	t.Focused.Description = t.Focused.Description.Foreground(colorDim)
	t.Focused.ErrorIndicator = t.Focused.ErrorIndicator.Foreground(colorRed)
	t.Focused.ErrorMessage = t.Focused.ErrorMessage.Foreground(colorRed)
	t.Focused.SelectSelector = lipgloss.NewStyle().Foreground(colorAccent).SetString("❯ ")
	t.Focused.MultiSelectSelector = lipgloss.NewStyle().Foreground(colorAccent).SetString("❯ ")
	t.Focused.NextIndicator = t.Focused.NextIndicator.Foreground(colorAccent)
	t.Focused.PrevIndicator = t.Focused.PrevIndicator.Foreground(colorAccent)
	t.Focused.SelectedOption = t.Focused.SelectedOption.Foreground(colorGreen)
	t.Focused.SelectedPrefix = lipgloss.NewStyle().Foreground(colorGreen).SetString("◉ ")
	t.Focused.UnselectedPrefix = lipgloss.NewStyle().Foreground(colorDim).SetString("○ ")
	t.Focused.FocusedButton = t.Focused.FocusedButton.Foreground(lipgloss.Color("#FFFFFF")).Background(colorAccentDeep)
	t.Focused.Next = t.Focused.FocusedButton
	t.Focused.TextInput.Cursor = t.Focused.TextInput.Cursor.Foreground(colorAccent)
	t.Focused.TextInput.Prompt = t.Focused.TextInput.Prompt.Foreground(colorAccent)
	t.Blurred = t.Focused
	t.Blurred.Base = t.Focused.Base.BorderStyle(lipgloss.HiddenBorder())
	t.Blurred.Card = t.Blurred.Base
	t.Blurred.NextIndicator = lipgloss.NewStyle()
	t.Blurred.PrevIndicator = lipgloss.NewStyle()
	t.Group.Title = t.Focused.Title
	t.Group.Description = t.Focused.Description
	return t
})

// errCancelled is the person stopping the command.
var errCancelled = errors.New("cancelled")

// presenter is the command's side of the terminal UI.
type presenter struct {
	program *tea.Program
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

// runTUI runs fn under the terminal UI. phases, when set, are the names of
// the steps the tracker above the live area shows.
func runTUI(ctx context.Context, phases []string, fn func(context.Context, *presenter) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := &model{
		phases: phases,
		cancel: cancel,
		spinner: spinner.New(
			spinner.WithSpinner(spinner.MiniDot),
			spinner.WithStyle(styleAccent),
		),
	}
	p := &presenter{program: tea.NewProgram(m), exited: make(chan struct{})}
	errc := make(chan error, 1)
	go func() {
		err := fn(ctx, p)
		switch {
		case err == nil, errors.Is(err, errReported):
		case errors.Is(err, errCancelled), errors.Is(err, context.Canceled):
			err = errCancelled
			p.show(func(int) string { return "\n  " + styleYellow.Render("■") + " Stopped." })
		default:
			p.show(func(width int) string { return renderFailure("Could not continue", err, width) })
			err = errReported
		}
		errc <- err
		p.send(finishMsg{})
	}()
	_, runErr := p.program.Run()
	close(p.exited)
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			return err
		}
		return runErr
	case <-time.After(5 * time.Second):
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
	phases     []string
	phase      int
	spinner    spinner.Model
	steps      []liveStep
	form       *huh.Form
	reply      chan error
	width      int
	queue      []string
	printing   bool
	finishing  bool
	done       bool
	cancel     context.CancelFunc
	cancelling bool
}

func (m *model) Init() tea.Cmd { return m.spinner.Tick }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" && m.form == nil {
			if m.cancelling {
				m.done = true
				return m, tea.Quit
			}
			m.cancelling = true
			m.cancel()
			return m, nil
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case sandbox.Event:
		return m, m.event(msg)
	case renderMsg:
		return m, m.commit(msg(m.contentWidth()))
	case phaseMsg:
		m.phase = int(msg)
		return m, nil
	case askMsg:
		m.form, m.reply = msg.form, msg.reply
		m.form.SubmitCmd = func() tea.Msg { return formDoneMsg{} }
		m.form.CancelCmd = func() tea.Msg { return formCancelMsg{} }
		m.form.WithTheme(keycardTheme).WithWidth(min(m.contentWidth(), 76))
		return m, m.form.Init()
	case formDoneMsg:
		m.endForm(nil)
		return m, nil
	case formCancelMsg:
		m.endForm(huh.ErrUserAborted)
		return m, nil
	case printedMsg:
		m.printing = false
		return m, m.flush()
	case finishMsg:
		m.finishing = true
		return m, m.flush()
	}
	if m.form != nil {
		_, cmd := m.form.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) endForm(err error) {
	reply := m.reply
	m.form, m.reply = nil, nil
	reply <- err
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

func (m *model) event(e sandbox.Event) tea.Cmd {
	switch e.Kind {
	case sandbox.EventStepStart:
		m.steps = append(m.steps, liveStep{id: e.ID, title: e.Text, start: time.Now()})
		return nil
	case sandbox.EventStepEnd:
		for i, s := range m.steps {
			if s.id == e.ID {
				m.steps = append(m.steps[:i], m.steps[i+1:]...)
				break
			}
		}
	}
	return m.commit(renderEvent(e, m.contentWidth()))
}

func (m *model) contentWidth() int {
	if m.width <= 0 {
		return 80
	}
	return min(m.width, 100)
}

func (m *model) View() tea.View {
	if m.done {
		return tea.NewView("")
	}
	var b strings.Builder
	if len(m.phases) > 0 && m.phase >= 0 {
		b.WriteString("\n" + m.tracker() + "\n")
	}
	for _, s := range m.steps {
		line := "  " + m.spinner.View() + " " + s.title
		if elapsed := time.Since(s.start); elapsed >= time.Second {
			line += styleFaint.Render("  " + elapsed.Truncate(time.Second).String())
		}
		b.WriteString("\n" + ansi.Truncate(line, m.contentWidth(), "…"))
	}
	if m.cancelling {
		b.WriteString("\n  " + styleYellow.Render("Stopping… press ctrl+c again to quit now"))
	}
	if m.form != nil {
		b.WriteString("\n" + m.form.View())
	}
	return tea.NewView(b.String())
}

// tracker shows where the run is: done phases, the current one, and those
// still ahead. A narrow terminal gets a short form.
func (m *model) tracker() string {
	parts := make([]string, 0, len(m.phases))
	for i, name := range m.phases {
		switch {
		case i < m.phase:
			parts = append(parts, styleGreen.Render("●")+" "+styleDim.Render(name))
		case i == m.phase:
			parts = append(parts, styleAccent.Bold(true).Render("◉ "+name))
		default:
			parts = append(parts, styleFaint.Render("○ "+name))
		}
	}
	line := "  " + strings.Join(parts, styleFaint.Render("  ─  "))
	if lipgloss.Width(line) <= m.contentWidth() {
		return line
	}
	return "  " + styleAccent.Render(fmt.Sprintf("Step %d of %d", m.phase+1, len(m.phases))) +
		styleFaint.Render(" · ") + styleBold.Render(m.phases[m.phase])
}

// renderEvent renders an event for the scrollback.
func renderEvent(e sandbox.Event, width int) string {
	switch e.Kind {
	case sandbox.EventHeader:
		return renderBanner(e.Text, width)
	case sandbox.EventSection:
		return "\n  " + styleAccent.Render("▍") + styleBold.Render(e.Text)
	case sandbox.EventOK:
		return "  " + styleGreen.Render("✓") + " " + e.Text
	case sandbox.EventWarn:
		return renderCallout(e.Text, colorYellow, width)
	case sandbox.EventInfo:
		return lipgloss.NewStyle().Foreground(colorDim).PaddingLeft(4).Width(width).Render(e.Text)
	case sandbox.EventQuote:
		return renderQuote(e.Text, width)
	case sandbox.EventStepEnd:
		if e.Err != nil {
			return renderFailure(e.Text, e.Err, width)
		}
		line := "  " + styleGreen.Render("✓") + " " + e.Text
		if e.Detail != "" {
			line += styleDim.Render(" · " + e.Detail)
		}
		if e.Elapsed > 500*time.Millisecond {
			line += styleFaint.Render("  " + e.Elapsed.Round(100*time.Millisecond).String())
		}
		return ansi.Truncate(line, width, "…")
	default:
		return ""
	}
}

func renderBanner(apiURL string, width int) string {
	title := lipgloss.NewStyle().Bold(true).Foreground(colorAccent).Render("◆ Keycard Sandbox")
	about := styleDim.Render(
		"Agents in Kubernetes sandboxes that act for your users through Keycard. The sandbox never holds an API key.",
	)
	where := styleFaint.Render(strings.TrimPrefix(apiURL, "https://"))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(colorAccentDeep).
		Padding(0, 2).MarginLeft(2).Width(min(width-2, 74))
	return "\n" + box.Render(title+"\n"+about+"\n\n"+where)
}

func renderCallout(text string, c color.Color, width int) string {
	style := lipgloss.NewStyle().
		BorderStyle(lipgloss.ThickBorder()).BorderLeft(true).BorderForeground(c).
		PaddingLeft(1).MarginLeft(2).Width(min(width-2, 78))
	return "\n" + style.Render(text)
}

func renderQuote(text string, width int) string {
	style := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).BorderLeft(true).BorderForeground(colorFaint).
		PaddingLeft(1).MarginLeft(4).Width(min(width-4, 80))
	return style.Render(strings.TrimRight(text, "\n"))
}

func renderFailure(title string, err error, width int) string {
	head := "  " + styleRed.Render("✗") + " " + title
	body := lipgloss.NewStyle().Foreground(colorRed).PaddingLeft(4).Width(min(width, 96)).Render(err.Error())
	return head + "\n" + body
}
