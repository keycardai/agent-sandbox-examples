package page

import (
	"cmp"
	"errors"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	// checkNameWidth is the width of a preflight check's name.
	checkNameWidth = 18
	// markGap is the space between the Keycard mark and the text beside it.
	markGap = 4
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

// Welcome greets a first run, or says what the settings lack. It shows
// the Keycard mark beside what the sandbox does, checks the machine, and
// lists the setup's steps.
type Welcome struct {
	view

	// Glint is the sheen across the Keycard mark while the machine checks
	// run.
	Glint style.Glint
}

// NewWelcome returns the welcome screen.
func NewWelcome(ui *UI, sandbox *state.Sandbox) *Welcome {
	return &Welcome{ui: ui, state: sandbox}
}

// View shows the Keycard mark beside what the sandbox does, checks the
// machine, lists the setup's steps and says what to do next, in the middle
// of the window. For settings with problems, it says what needs fixing in
// place of what the sandbox does. A short window leaves out the diagram,
// then the steps, then the paragraph, so that what to do next still shows.
func (w *Welcome) View() string {
	width := max(min(w.textWidth(), readWidth), 1)
	text := w.text(width, detailAll)
	for detail := detailAll + 1; detail <= detailLeast &&
		lipgloss.Height(text) > w.ui.Height-component.ChromeRows-1; detail++ {
		text = w.text(width, detail)
	}
	body := lipgloss.NewStyle().Width(width).Render(text)
	if w.logoFits() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, style.Logo(w.ui.Styles, w.ui.Background, w.Glint),
			strings.Repeat(" ", markGap), body)
	}
	return w.ui.Page(w.title(), w.centered(body), w.Hints())
}

// Hints lists the screen's keys.
func (w *Welcome) Hints() []component.Hint {
	// A first run has no settings yet, so it offers setup alone.
	if w.state.FirstRun() {
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

// text is the welcome screen's text, with as much as detail keeps.
func (w *Welcome) text(width, detail int) string {
	parts := append(w.lead(width, detail), w.machineTitle(), w.checkList(width), "")
	if w.state.FirstRun() && detail < detailNoSteps {
		parts = append(parts, w.setupPreview(width), "")
	}
	if failed := w.setupFailure(width); failed != "" {
		parts = append(parts, failed, "")
	}
	return lipgloss.JoinVertical(lipgloss.Left, append(parts, w.action(width))...)
}

// lead opens the welcome text: what the sandbox does on a first run, else
// what the settings lack.
func (w *Welcome) lead(width, detail int) []string {
	theme := w.ui.Styles
	if !w.state.FirstRun() {
		return []string{theme.Title.Render(w.fixTitle()), "", w.problemList(width), ""}
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
	theme := w.ui.Styles
	switch {
	case w.state.Preflight.Checking():
		return theme.SectionTitle.Render("This machine")
	case w.state.Preflight.Failed() > 0:
		return theme.Attention.Bold(true).Render("This machine is not ready")
	}
	return theme.SectionTitle.Render("This machine is ready")
}

// setupPreview lists the steps setup goes through, numbered as the wizard
// counts them, in as many columns as fit, and what setup costs.
func (w *Welcome) setupPreview(width int) string {
	theme := w.ui.Styles
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
	theme := w.ui.Styles
	if w.state.Preflight.Checking() {
		return theme.Running.Render(w.ui.spinner() + " Checking this machine…")
	}
	verb := "Start setup"
	if !w.state.FirstRun() {
		verb = "Fix the settings"
	}
	action := theme.KeyChip.Render(component.KeyEnter) + " " + theme.Title.Render(verb)
	if w.state.Preflight.Failed() == 0 {
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
	return w.ui.Width-component.PageIndent*half-style.LogoWidth(w.ui.Styles)-markGap >=
		lipgloss.Width(w.diagram())
}

// textWidth is the width the welcome text has, beside the mark when it
// fits.
func (w *Welcome) textWidth() int {
	if w.logoFits() {
		return w.ui.Width - component.PageIndent*half - style.LogoWidth(w.ui.Styles) - markGap
	}
	return w.ui.Width - component.PageIndent*half
}

func (w *Welcome) title() string {
	if w.state.FirstRun() {
		return "welcome"
	}
	return "setup needs a fix"
}

func (w *Welcome) fixTitle() string {
	if len(w.state.Problems) == 1 {
		return "One setting needs a fix before the next bring-up."
	}
	return "Some settings need a fix before the next bring-up."
}

// diagram draws where a request goes.
func (w *Welcome) diagram() string {
	theme := w.ui.Styles
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
	theme := w.ui.Styles
	wrap := lipgloss.NewStyle().Width(max(width-checkNameWidth-component.BorderCols, 1))
	run := w.state.Preflight
	lines := make([]string, 0, len(run.Items))
	for i, item := range run.Items {
		icon, name, text := theme.Faint.Render("·"), theme.Dim.Render(item.Name), theme.Faint.Render("waiting")
		switch {
		case i < len(run.Results):
			icon, name, text = w.checkResult(run.Results[i])
		case i == len(run.Results):
			icon = theme.Running.Render(w.ui.spinner())
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
	theme := w.ui.Styles
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
func (v view) problemList(width int) string {
	theme := v.ui.Styles
	wrap := lipgloss.NewStyle().Width(max(width-checkNameWidth-component.BorderCols, 1))
	root := v.state.Root() + "/"
	titles := map[string]string{}
	for _, step := range RailSteps() {
		titles[step.ID] = step.Title
	}
	lines := make([]string, 0, len(v.state.Problems))
	for _, problem := range v.state.Problems {
		name := cmp.Or(titles[sandbox.ProblemSection(problem)], "Setup")
		detail := strings.ReplaceAll(problem.Error(), root, "")
		text := theme.Text.Render(detail)
		if summary := problemSummary(problem); summary != "" {
			text = theme.Text.Render(summary) + "\n" + theme.Muted.Render(detail)
		}
		lines = append(lines, lipgloss.JoinHorizontal(lipgloss.Top,
			theme.Attention.Render("! "), lipgloss.NewStyle().Width(checkNameWidth).Render(theme.Title.Render(name)),
			wrap.Render(text)))
	}
	return strings.Join(lines, "\n\n")
}

// setupFailure says why the last setup failed, with what to do when
// HintFor knows, or is "" when it did not fail.
func (v view) setupFailure(width int) string {
	if v.state.SetupErr == nil {
		return ""
	}
	theme := v.ui.Styles
	wrap := lipgloss.NewStyle().Width(max(width, 1))
	lines := []string{
		wrap.Render(theme.Failed.Render("✕ Setup stopped: " + component.FirstLine(v.state.SetupErr.Error()))),
	}
	if hint := HintFor(v.state.SetupErr); hint != "" {
		lines = append(lines, wrap.Render(theme.Hint.Render("→ "+hint)))
	}
	return strings.Join(lines, "\n")
}

// centered puts a block in the middle of the page's body, a little above
// the middle, where it reads as centered. A block that does not fit stays
// at the top left.
func (v view) centered(block string) string {
	return v.place(block, lipgloss.Height(block))
}

// anchored centers a block that grows and shrinks, such as the wizard's
// question, across the page, with its top where a block of anchorRows
// would sit centered. It stays put from one question to the next.
func (v view) anchored(block string) string {
	return v.place(block, anchorRows)
}

// place puts a block in the middle of the page's width, with its top
// where a block of the given height would sit centered, a little above
// the middle.
func (v view) place(block string, rows int) string {
	free := v.ui.Height - component.ChromeRows - 1 - rows
	top := int(float64(max(free, 0)) * opticalMiddle)
	left := max(v.ui.Width-component.PageIndent*half-lipgloss.Width(block), 0) / half
	return lipgloss.NewStyle().PaddingTop(top).PaddingLeft(left).Render(block)
}

// problemSummary says a problem in words, without file paths, or returns
// "" when the error says it best.
func problemSummary(problem error) string {
	switch {
	case errors.Is(problem, sandbox.ErrNoSetup):
		return "Setup has not run yet."
	case errors.Is(problem, sandbox.ErrKeychain):
		return "The keychain refused to read a saved secret. Unlock it, then check again."
	case errors.Is(problem, zonesetup.ErrMissingSecret) && sandbox.ProblemSection(problem) == sandbox.SectionAccount:
		return "The service account's client secret is missing."
	case errors.Is(problem, zonesetup.ErrMissingSecret):
		return "A credential of the demo has no value."
	}
	return ""
}

// indentLines puts indent before each line of text.
func indentLines(text, indent string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}
