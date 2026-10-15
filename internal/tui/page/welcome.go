package page

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

const (
	// checkNameWidth is the width of a preflight check's name.
	checkNameWidth = 18
	// markGap is the space between the Keycard mark and the text beside it.
	markGap = 4
	// minCheck is the least time a machine check shows its spinner.
	minCheck = 350 * time.Millisecond
	// checkTimeout bounds one machine check.
	checkTimeout = 10 * time.Second
	// stepColumns is the most columns the setup's steps take on the welcome
	// screen, and stepGap the space after each.
	stepColumns = 3
	stepGap     = 3
	// stepIndent indents the setup's steps under their title, as the checks
	// are.
	stepIndent = "  "
	// opticalMiddle is where a centered block's top goes, as a share of the
	// space it leaves. A block exactly in the middle reads as low.
	opticalMiddle = 0.4
	// anchorRows is the height the wizard's question is placed for. A
	// longer question grows down from there.
	anchorRows = 24
	// wordCluster names the cluster: a setup step and a setting. wordZone
	// names the zone in the welcome's diagram.
	wordCluster = "Cluster"
	wordZone    = "Keycard zone"
)

// How much the welcome screen says, from all of it to the least.
const (
	detailAll = iota
	detailNoDiagram
	detailNoSteps
	detailLeast
)

// CheckDoneMsg is one machine check's result.
type CheckDoneMsg struct {
	Index int
	Check sandbox.Check
}

// Welcome greets a first run, or says what the settings lack. It shows
// the Keycard mark beside what the sandbox does, checks the machine, and
// lists the setup's steps.
type Welcome struct {
	shared *Shared
	// Fix lists the sections setup asks to fix the problems. Empty asks
	// every section, as on a first run.
	Fix []string
	// Glint is the sheen across the Keycard mark while the machine checks
	// run.
	Glint    style.Glint
	items    []sandbox.PreflightItem
	checks   []sandbox.Check
	checking bool
}

// NewWelcome returns the welcome screen.
func NewWelcome(shared *Shared) *Welcome { return &Welcome{shared: shared} }

// Open finds the sections that fix the settings' problems, and runs the
// machine checks.
func (w *Welcome) Open() tea.Cmd {
	w.Fix = nil
	for _, problem := range w.shared.Problems {
		section := sandbox.ProblemSection(problem)
		if section == "" {
			w.Fix = nil
			break
		}
		if !slices.Contains(w.Fix, section) {
			w.Fix = append(w.Fix, section)
		}
	}
	return w.Preflight(w.shared.Session.PreflightItems())
}

// Preflight runs the machine checks in items one after another in the
// background, so the screen shows each as it runs.
func (w *Welcome) Preflight(items []sandbox.PreflightItem) tea.Cmd {
	w.items, w.checks = items, nil
	return w.nextCheck()
}

// OnCheck keeps a check's result, and starts the next check.
func (w *Welcome) OnCheck(msg CheckDoneMsg) tea.Cmd {
	if msg.Index != len(w.checks) {
		return nil
	}
	w.checks = append(w.checks, msg.Check)
	mark := map[sandbox.CheckState]string{sandbox.CheckOK: "✓", sandbox.CheckNote: "•", sandbox.CheckFailed: "✕"}
	w.shared.Record(mark[msg.Check.State], msg.Check.Name+": "+msg.Check.Detail)
	return w.nextCheck()
}

// View shows the Keycard mark beside what the sandbox does, checks the
// machine, lists the setup's steps and says what to do next, in the middle
// of the window. For settings with problems, it says what needs fixing in
// place of what the sandbox does. A short window leaves out the diagram,
// then the steps, then the paragraph, so that what to do next still shows.
func (w *Welcome) View() string {
	shared := w.shared
	width := max(min(w.textWidth(), readWidth), 1)
	text := w.text(width, detailAll)
	for detail := detailAll + 1; detail <= detailLeast &&
		lipgloss.Height(text) > shared.Height-component.ChromeRows-1; detail++ {
		text = w.text(width, detail)
	}
	body := lipgloss.NewStyle().Width(width).Render(text)
	if w.logoFits() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, style.Logo(shared.Styles, shared.Background, w.Glint),
			strings.Repeat(" ", markGap), body)
	}
	return shared.Page(w.title(), shared.centered(body), w.Hints())
}

// Hints lists the screen's keys.
func (w *Welcome) Hints() []component.Hint {
	// A first run has no settings yet, so it offers setup alone.
	if w.shared.FirstRun() {
		return []component.Hint{
			{Key: component.KeyEnter, Desc: "start setup"}, {Key: "r", Desc: "check again"}, {Key: "q", Desc: wordQuit},
		}
	}
	return []component.Hint{
		{Key: component.KeyEnter, Desc: "fix the settings"},
		{Key: "e", Desc: "all settings"},
		{Key: "r", Desc: "check again"},
		{Key: "q", Desc: wordQuit},
	}
}

// Checking reports whether a machine check has no result yet.
func (w *Welcome) Checking() bool {
	return w.checking || len(w.checks) < len(w.items)
}

// nextCheck runs the first check that has no result yet. Each check shows
// for at least minCheck, so a fast one does not flash past.
func (w *Welcome) nextCheck() tea.Cmd {
	index := len(w.checks)
	if index >= len(w.items) {
		w.checking = false
		return nil
	}
	w.checking = true
	item := w.items[index]
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
		defer cancel()
		started := time.Now()
		check := item.Run(ctx)
		if wait := minCheck - time.Since(started); wait > 0 {
			time.Sleep(wait)
		}
		return CheckDoneMsg{Index: index, Check: check}
	}
}

// text is the welcome screen's text, with as much as detail keeps.
func (w *Welcome) text(width, detail int) string {
	parts := append(w.lead(width, detail), w.machineTitle(), w.checkList(width), "")
	if w.shared.FirstRun() && detail < detailNoSteps {
		parts = append(parts, w.setupPreview(width), "")
	}
	if failed := w.shared.setupFailure(width); failed != "" {
		parts = append(parts, failed, "")
	}
	return lipgloss.JoinVertical(lipgloss.Left, append(parts, w.action(width))...)
}

// lead opens the welcome text: what the sandbox does on a first run, else
// what the settings lack.
func (w *Welcome) lead(width, detail int) []string {
	shared := w.shared
	theme := shared.Styles
	if !shared.FirstRun() {
		return []string{theme.Title.Render(w.fixTitle()), "", shared.problemList(width), ""}
	}
	lead := []string{theme.Title.Render("Run an AI agent that never holds a secret."), ""}
	if detail < detailLeast {
		lead = append(lead, lipgloss.NewStyle().Width(width).Render(theme.Muted.Render("The agent runs in a "+
			"local Kubernetes cluster. Every HTTPS request it makes goes through the egress proxy, which asks "+
			"your Keycard zone for the user's credential and adds it on the way out.")), "")
	}
	if detail < detailNoDiagram {
		lead = append(lead, w.diagram(), "")
	}
	return lead
}

// machineTitle heads the machine checks, and says how they came out once
// they all ran.
func (w *Welcome) machineTitle() string {
	theme := w.shared.Styles
	switch {
	case w.Checking():
		return theme.SectionTitle.Render("This machine")
	case w.failedChecks() > 0:
		return theme.Attention.Bold(true).Render("This machine is not ready")
	}
	return theme.SectionTitle.Render("This machine is ready")
}

// failedChecks counts the machine checks that failed.
func (w *Welcome) failedChecks() int {
	failed := 0
	for _, check := range w.checks {
		if check.State == sandbox.CheckFailed {
			failed++
		}
	}
	return failed
}

// setupPreview lists the steps setup goes through, numbered as the wizard
// counts them, in as many columns as fit, and what setup costs.
func (w *Welcome) setupPreview(width int) string {
	theme := w.shared.Styles
	steps := PlannedSteps()
	cells := make([]string, 0, len(steps))
	cellWidth := 0
	for i, step := range steps {
		cell := theme.Dim.Render(strconv.Itoa(i+1)) + " " + theme.Text.Render(step.Title)
		cells = append(cells, cell)
		cellWidth = max(cellWidth, lipgloss.Width(cell)+stepGap)
	}
	columns := min(max((width-len(stepIndent))/cellWidth, 1), stepColumns)
	lines := []string{theme.SectionTitle.Render("Setup")}
	for first := 0; first < len(cells); first += columns {
		var line strings.Builder
		for _, cell := range cells[first:min(first+columns, len(cells))] {
			line.WriteString(component.Pad(cell, cellWidth))
		}
		lines = append(lines, stepIndent+strings.TrimRight(line.String(), " "))
	}
	note := lipgloss.NewStyle().Width(max(width-len(stepIndent), 1)).Render(theme.Dim.Render(
		"About three minutes. Setup changes nothing in Keycard, unless you create a zone.",
	))
	return strings.Join(append(lines, indentLines(note, stepIndent)), "\n")
}

// action says what to do next: wait while the checks run, then press
// enter. When a check failed, it says to fix that before the launch. Setup
// does not need the machine, so enter works either way.
func (w *Welcome) action(width int) string {
	theme := w.shared.Styles
	if w.Checking() {
		return theme.Running.Render(w.shared.spinner() + " Checking this machine…")
	}
	verb := "Start setup"
	if !w.shared.FirstRun() {
		verb = "Fix the settings"
	}
	action := theme.KeyChip.Render(component.KeyEnter) + " " + theme.Title.Render(verb)
	if w.failedChecks() == 0 {
		return action
	}
	note := lipgloss.NewStyle().Width(width).Render(theme.Attention.Render(
		"Fix what failed before you launch. Press r to check again.",
	))
	return note + "\n\n" + action
}

// logoFits reports whether the Keycard mark fits beside the welcome text
// with the diagram whole. A narrow window shows the text alone.
func (w *Welcome) logoFits() bool {
	return w.shared.Width-component.PageIndent*half-style.LogoWidth(w.shared.Styles)-markGap >=
		lipgloss.Width(w.diagram())
}

// textWidth is the width the welcome text has, beside the mark when it
// fits.
func (w *Welcome) textWidth() int {
	if w.logoFits() {
		return w.shared.Width - component.PageIndent*half - style.LogoWidth(w.shared.Styles) - markGap
	}
	return w.shared.Width - component.PageIndent*half
}

func (w *Welcome) title() string {
	if w.shared.FirstRun() {
		return "welcome"
	}
	return "setup needs a fix"
}

func (w *Welcome) fixTitle() string {
	if len(w.shared.Problems) == 1 {
		return "One setting needs a fix before the next bring-up."
	}
	return "Some settings need a fix before the next bring-up."
}

// diagram draws where a request goes.
func (w *Welcome) diagram() string {
	theme := w.shared.Styles
	box := func(text string) string { return theme.Accent.Render(text) }
	arrow := theme.Faint.Render(" ───▶ ")
	lines := []string{
		"  " + box("agent") + arrow + box("egress proxy") + arrow + theme.Text.Render("api.github.com, openrouter.ai, …"),
		"                 " + theme.Faint.Render("│ ▲"),
		"                 " + theme.Faint.Render("▼ │") + theme.Muted.Render("  the user's credential"),
		"              " + box(wordZone),
	}
	return strings.Join(lines, "\n")
}

// checkList draws the machine checks: a result for each that ran, with a
// fix under each that failed, a spinner for the one that runs, and the
// rest waiting.
func (w *Welcome) checkList(width int) string {
	theme := w.shared.Styles
	wrap := lipgloss.NewStyle().Width(max(width-checkNameWidth-component.BorderCols, 1))
	lines := make([]string, 0, len(w.items))
	for i, item := range w.items {
		icon, name, text := theme.Faint.Render("·"), theme.Dim.Render(item.Name), theme.Faint.Render("waiting")
		switch {
		case i < len(w.checks):
			icon, name, text = w.checkResult(w.checks[i])
		case i == len(w.checks) && w.checking:
			icon = theme.Running.Render(w.shared.spinner())
			name, text = theme.Text.Render(item.Name), theme.Running.Render("checking…")
		}
		lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top, "  "+icon+" ",
			lipgloss.NewStyle().Width(checkNameWidth).Render(name), wrap.Render(text)))
	}
	return strings.Join(lines, "\n")
}

// checkResult draws a check that ran: its mark, its name, and what it
// found, with the fix under a failure.
func (w *Welcome) checkResult(check sandbox.Check) (string, string, string) {
	theme := w.shared.Styles
	icon, detail := theme.OK.Render("✓"), theme.Muted.Render(check.Detail)
	switch check.State {
	case sandbox.CheckFailed:
		icon, detail = theme.Failed.Render("✕"), theme.Failed.Render(check.Detail)
	case sandbox.CheckNote:
		icon = theme.Accent.Render("•")
	case sandbox.CheckOK:
	}
	if check.Fix != "" {
		detail += "\n" + theme.Hint.Render(check.Fix)
	}
	return icon, theme.Text.Render(check.Name), detail
}

// problemList lists the settings' problems in words, each under the step
// that fixes it.
func (s *Shared) problemList(width int) string {
	theme := s.Styles
	wrap := lipgloss.NewStyle().Width(max(width-checkNameWidth-component.BorderCols, 1))
	root := s.Session.Local().Root + "/"
	titles := map[string]string{}
	for _, step := range RailSteps() {
		titles[step.ID] = step.Title
	}
	lines := make([]string, 0, len(s.Problems))
	for _, problem := range s.Problems {
		name := cmp.Or(titles[sandbox.ProblemSection(problem)], "Setup")
		detail := theme.Muted.Render(strings.ReplaceAll(problem.Error(), root, ""))
		summary := theme.Text.Render(problemSummary(problem))
		lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top,
			theme.Attention.Render("! "), lipgloss.NewStyle().Width(checkNameWidth).Render(theme.Title.Render(name)),
			wrap.Render(summary+"\n"+detail)))
	}
	return strings.Join(lines, "\n\n")
}

// setupFailure says why the last setup failed, with what to do when
// HintFor knows, or is "" when it did not fail.
func (s *Shared) setupFailure(width int) string {
	if s.SetupErr == nil {
		return ""
	}
	theme := s.Styles
	wrap := lipgloss.NewStyle().Width(max(width, 1))
	lines := []string{wrap.Render(theme.Failed.Render("✕ Setup stopped: " + component.FirstLine(s.SetupErr.Error())))}
	if hint := HintFor(s.SetupErr); hint != "" {
		lines = append(lines, wrap.Render(theme.Hint.Render("→ "+hint)))
	}
	return strings.Join(lines, "\n")
}

// centered puts a block in the middle of the page's body, a little above
// the middle, where it reads as centered. A block that does not fit stays
// at the top left.
func (s *Shared) centered(block string) string {
	return s.place(block, lipgloss.Height(block))
}

// anchored centers a block that grows and shrinks, such as the wizard's
// question, across the page, with its top where a block of anchorRows
// would sit centered. It stays put from one question to the next.
func (s *Shared) anchored(block string) string {
	return s.place(block, anchorRows)
}

// place puts a block in the middle of the page's width, with its top
// where a block of the given height would sit centered, a little above
// the middle.
func (s *Shared) place(block string, rows int) string {
	free := s.Height - component.ChromeRows - 1 - rows
	top := int(float64(max(free, 0)) * opticalMiddle)
	left := max(s.Width-component.PageIndent*half-lipgloss.Width(block), 0) / half
	return lipgloss.NewStyle().PaddingTop(top).PaddingLeft(left).Render(block)
}

// problemSummary says a problem in words, without file paths.
func problemSummary(problem error) string {
	text := problem.Error()
	switch {
	case strings.Contains(text, "resources["):
		return "A credential of the demo has no value."
	case strings.Contains(text, "setup has not run"):
		return "Setup has not run yet."
	case strings.Contains(text, "unset"):
		return "A secret the settings name is not set."
	}
	return component.FirstLine(text)
}

// indentLines puts indent before each line of text.
func indentLines(text, indent string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}
