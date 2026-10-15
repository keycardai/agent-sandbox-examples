package page

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
)

// demoNameWidth is the width of a demo's name in the picker.
const demoNameWidth = 8

// The kinds of a demo's inputs, as its scenario file names them.
const (
	inputGitHub = "github"
	inputText   = "text"
)

// Demos is the list of the checkout's demos, to switch to another.
type Demos struct {
	view

	picker component.Picker
}

// NewDemos returns the list of demos.
func NewDemos(ui *UI, sandbox *state.Sandbox) *Demos {
	return &Demos{ui: ui, state: sandbox}
}

// Open puts the cursor on the first demo the settings do not name.
func (d *Demos) Open() {
	current := d.state.DemoName()
	d.picker.Cursor = max(slices.IndexFunc(d.state.DemoList(), func(s *sandbox.Scenario) bool {
		return s.Name != current
	}), 0)
}

// Keys moves through the demos, and returns the name of the one chosen.
func (d *Demos) Keys(msg tea.KeyPressMsg) (string, component.Pick) {
	demos := d.state.DemoList()
	pick := d.picker.Update(msg, d.ui.Keys, len(demos))
	if pick != component.PickChosen {
		return "", pick
	}
	return demos[d.picker.Cursor].Name, pick
}

// View draws the demos, compact when they do not fit the window.
func (d *Demos) View() string {
	if modal := d.draw(false); lipgloss.Height(modal) <= d.ui.Height-component.ChromeRows {
		return modal
	}
	return d.draw(true)
}

func (d *Demos) draw(compact bool) string {
	theme := d.ui.Styles
	width := component.ModalInnerWidth(d.ui.Width)
	about := lipgloss.NewStyle().Width(max(width-demoNameWidth-half, 1))
	rows := []string{theme.ModalTitle.Render("Switch the demo"), theme.Muted.Render(
		"Setup asks for the demo's credentials, then enter brings the sandbox up with it.",
	), ""}
	for i, scenario := range d.state.DemoList() {
		mark, name := "  ", theme.Text.Bold(true).Render(component.Pad(scenario.Name, demoNameWidth))
		if i == d.picker.Cursor {
			mark = theme.Accent.Render("▸ ")
		}
		block := mark + name + theme.Text.Render(scenario.Title) + d.status(scenario)
		if !compact || i == d.picker.Cursor {
			text := about.Render(theme.Muted.Render(demoAbout(scenario)) + "\n" + theme.Dim.Render(
				"Asks for "+demoNeeds(scenario),
			))
			block = lipgloss.JoinVertical(lipgloss.Left, block,
				lipgloss.JoinHorizontal(lipgloss.Top, strings.Repeat(" ", demoNameWidth+half), text))
		}
		if i == d.picker.Cursor {
			block = d.highlight(block, width)
		}
		rows = append(rows, block)
		if !compact {
			rows = append(rows, "")
		}
	}
	if compact {
		rows = append(rows, "")
	}
	if d.state.Demos.Err != nil {
		rows = append(
			rows,
			about.Render(theme.Attention.Render("Left out: "+component.FirstLine(d.state.Demos.Err.Error()))),
			"",
		)
	}
	rows = append(rows, component.Chips(theme, component.Hint{Key: "↑↓", Desc: wordChoose},
		component.Hint{Key: component.KeyEnter, Desc: "switch"}, component.Hint{Key: component.KeyEsc, Desc: "cancel"}))
	return theme.Modal.Width(component.ModalWidth(d.ui.Width)).Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// status marks the demo that runs now and the one the settings name.
func (d *Demos) status(scenario *sandbox.Scenario) string {
	switch {
	case scenario.Name == d.state.Demos.Launched && d.state.Up():
		return d.ui.Styles.OK.Render("  ● running now")
	case scenario.Name == d.state.DemoName():
		return d.ui.Styles.Muted.Render("  ● chosen")
	}
	return ""
}

func (d *Demos) highlight(block string, width int) string {
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		lines[i] = component.Highlight(line, d.ui.Styles.Colors.Selection, width)
	}
	return strings.Join(lines, "\n")
}

// demoAbout says what the demo's agent does, by how it runs.
func demoAbout(scenario *sandbox.Scenario) string {
	switch scenario.Run.Mode {
	case sandbox.ModeShell:
		if len(scenario.Run.Command) > 0 {
			return "You chat with it in the terminal: a opens " + scenario.Run.Command[0] + "."
		}
		return "You work with it in the terminal: a opens it."
	case sandbox.ModeBackground:
		return "It runs in the background and answers you where it listens. Its log is on the Agents tab."
	}
	return "It runs the checks, then stays idle. s sends a request from it."
}

// demoNeeds lists what setup asks for, leaving out texts with a default.
func demoNeeds(scenario *sandbox.Scenario) string {
	needs := []string{}
	for _, input := range scenario.Inputs {
		switch {
		case input.Kind == inputGitHub:
			needs = append(needs, "GitHub")
		case input.Kind == inputText && input.Default != "":
		default:
			needs = append(needs, shortPrompt(input.Prompt))
		}
	}
	if len(needs) == 0 {
		return "nothing."
	}
	return strings.Join(needs, ", ") + "."
}

// shortPrompt keeps the first phrase of a question.
func shortPrompt(prompt string) string {
	if i := strings.IndexAny(prompt, "(,."); i > 0 {
		prompt = prompt[:i]
	}
	return strings.TrimPrefix(strings.TrimSpace(prompt), "Your ")
}
