package page

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
)

// demoNameWidth is the width of a demo's name in the picker.
const demoNameWidth = 8

// The kinds of a demo's inputs, as its scenario file names them.
const (
	inputGitHub = "github"
	inputText   = "text"
)

// Demos is the list of the checkout's demos, to switch to another. Each
// says what its agent does and what setup asks for it.
type Demos struct {
	shared *Shared
	picker component.Picker
}

// NewDemos returns the list of demos.
func NewDemos(shared *Shared) *Demos { return &Demos{shared: shared} }

// Open puts the cursor on the first demo the settings do not name.
func (d *Demos) Open() {
	current := d.shared.CurrentDemo()
	d.picker.Cursor = max(slices.IndexFunc(d.shared.DemoList(), func(s *sandbox.Scenario) bool {
		return s.Name != current
	}), 0)
}

// Keys moves through the demos, and returns the name of the one chosen.
func (d *Demos) Keys(msg tea.KeyPressMsg) (string, component.Pick) {
	demos := d.shared.DemoList()
	pick := d.picker.Update(msg, d.shared.Keys, len(demos))
	if pick != component.PickChosen {
		return "", pick
	}
	return demos[d.picker.Cursor].Name, pick
}

// View draws the demos: the name, what the agent does, and what setup asks
// for it. When that does not fit the window, only the selected demo says
// what it does.
func (d *Demos) View() string {
	if modal := d.view(false); lipgloss.Height(modal) <= d.shared.Height-component.ChromeRows {
		return modal
	}
	return d.view(true)
}

func (d *Demos) view(compact bool) string {
	shared := d.shared
	theme := shared.Styles
	width := component.ModalInnerWidth(shared.Width)
	about := lipgloss.NewStyle().Width(max(width-demoNameWidth-half, 1))
	rows := []string{theme.ModalTitle.Render("Switch the demo"), theme.Muted.Render(
		"Setup asks for the demo's credentials, then enter brings the sandbox up with it.",
	), ""}
	for i, scenario := range shared.DemoList() {
		mark, name := "  ", theme.Text.Bold(true).Render(component.Pad(scenario.Name, demoNameWidth))
		if i == d.picker.Cursor {
			mark = theme.Accent.Render("▸ ")
		}
		block := mark + name + theme.Text.Render(scenario.Title) + d.state(scenario)
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
	if shared.DemosErr != nil {
		rows = append(rows, about.Render(theme.Attention.Render("Left out: "+component.FirstLine(shared.DemosErr.Error()))),
			"")
	}
	rows = append(rows, component.Chips(theme, component.Hint{Key: "↑↓", Desc: wordChoose},
		component.Hint{Key: component.KeyEnter, Desc: "switch"}, component.Hint{Key: component.KeyEsc, Desc: "cancel"}))
	return theme.Modal.Width(component.ModalWidth(shared.Width)).Render(lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// state marks the demo that runs now, and the one the settings name when
// it does not run.
func (d *Demos) state(scenario *sandbox.Scenario) string {
	shared := d.shared
	switch {
	case scenario.Name == shared.Launched && shared.SandboxUp():
		return shared.Styles.OK.Render("  ● running now")
	case scenario.Name == shared.CurrentDemo():
		return shared.Styles.Muted.Render("  ● chosen")
	}
	return ""
}

// highlight puts the selection color behind each line of a block across
// width.
func (d *Demos) highlight(block string, width int) string {
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		lines[i] = component.Highlight(line, d.shared.Styles.Colors.Selection, width)
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
		return "It runs in the background and answers you where it listens. Run sandbox logs to follow its log."
	}
	return "It runs the checks, then stays idle. c runs them again."
}

// demoNeeds lists what setup asks for the demo: each credential, and each
// text with no default.
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

// shortPrompt keeps a question's first phrase, such as "Slack bot token"
// of "Slack bot token (xoxb-), with the im:write scope".
func shortPrompt(prompt string) string {
	if i := strings.IndexAny(prompt, "(,."); i > 0 {
		prompt = prompt[:i]
	}
	return strings.TrimPrefix(strings.TrimSpace(prompt), "Your ")
}
