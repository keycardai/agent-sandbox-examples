package main

import (
	"fmt"
	"image/color"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

// Text widths. Lines start two columns in, and a step's quote or error four.
const (
	defaultWidth = 80  // the width before the terminal reports its own
	maxWidth     = 100 // the widest the CLI lays text out
	blockWidth   = 78  // banners, callouts and quotes
	formWidth    = 76
	indent       = 2
	nestedIndent = 4
)

// theme is the CLI's colors and the styles made from them.
type theme struct {
	accent, accentDeep, green, red, yellow, dim, faint color.Color
}

func newTheme() theme {
	return theme{
		accent:     lipgloss.Color("#A78BFA"),
		accentDeep: lipgloss.Color("#7C3AED"),
		green:      lipgloss.Color("#34D399"),
		red:        lipgloss.Color("#F87171"),
		yellow:     lipgloss.Color("#FBBF24"),
		dim:        lipgloss.Color("#8B8FA3"),
		faint:      lipgloss.Color("#4B5068"),
	}
}

func (t theme) paint(c color.Color, text string) string {
	return lipgloss.NewStyle().Foreground(c).Render(text)
}

func (t theme) bold(text string) string { return lipgloss.NewStyle().Bold(true).Render(text) }

// forms styles huh forms in the CLI's colors.
func (t theme) forms() huh.ThemeFunc {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		styles := huh.ThemeCharm(isDark)
		focused := &styles.Focused
		focused.Base = focused.Base.BorderForeground(t.accentDeep)
		focused.Card = focused.Base
		focused.Title = focused.Title.Foreground(t.accent).Bold(true)
		focused.NoteTitle = focused.NoteTitle.Foreground(t.accent).Bold(true)
		focused.Description = focused.Description.Foreground(t.dim)
		focused.ErrorIndicator = focused.ErrorIndicator.Foreground(t.red)
		focused.ErrorMessage = focused.ErrorMessage.Foreground(t.red)
		focused.SelectSelector = lipgloss.NewStyle().Foreground(t.accent).SetString("❯ ")
		focused.MultiSelectSelector = lipgloss.NewStyle().Foreground(t.accent).SetString("❯ ")
		focused.NextIndicator = focused.NextIndicator.Foreground(t.accent)
		focused.PrevIndicator = focused.PrevIndicator.Foreground(t.accent)
		focused.SelectedOption = focused.SelectedOption.Foreground(t.green)
		focused.SelectedPrefix = lipgloss.NewStyle().Foreground(t.green).SetString("◉ ")
		focused.UnselectedPrefix = lipgloss.NewStyle().Foreground(t.dim).SetString("○ ")
		focused.FocusedButton = focused.FocusedButton.Foreground(lipgloss.Color("#FFFFFF")).Background(t.accentDeep)
		focused.Next = focused.FocusedButton
		focused.TextInput.Cursor = focused.TextInput.Cursor.Foreground(t.accent)
		focused.TextInput.Prompt = focused.TextInput.Prompt.Foreground(t.accent)
		styles.Blurred = *focused
		styles.Blurred.Base = focused.Base.BorderStyle(lipgloss.HiddenBorder())
		styles.Blurred.Card = styles.Blurred.Base
		styles.Blurred.NextIndicator = lipgloss.NewStyle()
		styles.Blurred.PrevIndicator = lipgloss.NewStyle()
		styles.Group.Title = focused.Title
		styles.Group.Description = focused.Description
		return styles
	})
}

// event renders an event for the scrollback. A step's start has no line,
// because its end reports it.
func (t theme) event(event sandbox.Event, width int) string {
	switch event.Kind {
	case sandbox.EventHeader:
		return t.banner(event.Text, width)
	case sandbox.EventSection:
		return t.section(event.Text)
	case sandbox.EventOK:
		return "  " + t.paint(t.green, "✓") + " " + event.Text
	case sandbox.EventWarn:
		return t.callout(event.Text, t.yellow, width)
	case sandbox.EventInfo:
		return lipgloss.NewStyle().Foreground(t.dim).PaddingLeft(nestedIndent).Width(width).Render(event.Text)
	case sandbox.EventQuote:
		return t.quote(event.Text, width)
	case sandbox.EventStepEnd:
		return t.stepEnd(event, width)
	case sandbox.EventStepStart:
		return ""
	default:
		return ""
	}
}

func (t theme) section(title string) string {
	return "\n  " + t.paint(t.accent, "▍") + t.bold(title)
}

func (t theme) stepEnd(event sandbox.Event, width int) string {
	if event.Err != nil {
		return t.failure(event.Text, event.Err, width)
	}
	line := "  " + t.paint(t.green, "✓") + " " + event.Text
	if event.Detail != "" {
		line += t.paint(t.dim, " · "+event.Detail)
	}
	if elapsed := sandbox.Elapsed(event.Elapsed); elapsed != "" {
		line += t.paint(t.faint, "  "+elapsed)
	}
	return ansi.Truncate(line, width, "…")
}

func (t theme) failure(title string, err error, width int) string {
	head := "  " + t.paint(t.red, "✗") + " " + title
	body := lipgloss.NewStyle().Foreground(t.red).PaddingLeft(nestedIndent).Width(width).Render(err.Error())
	return head + "\n" + body
}

func (t theme) banner(apiURL string, width int) string {
	title := lipgloss.NewStyle().Bold(true).Foreground(t.accent).Render("◆ Keycard Sandbox")
	about := t.paint(t.dim, "Agents in Kubernetes sandboxes that act for your users through Keycard. "+
		"The sandbox never holds an API key.")
	where := t.paint(t.faint, strings.TrimPrefix(apiURL, "https://"))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(t.accentDeep).
		Padding(0, indent).MarginLeft(indent).Width(min(width-indent, blockWidth))
	return "\n" + box.Render(title+"\n"+about+"\n\n"+where)
}

func (t theme) callout(text string, border color.Color, width int) string {
	style := lipgloss.NewStyle().
		BorderStyle(lipgloss.ThickBorder()).BorderLeft(true).BorderForeground(border).
		PaddingLeft(1).MarginLeft(indent).Width(min(width-indent, blockWidth))
	return "\n" + style.Render(text)
}

func (t theme) quote(text string, width int) string {
	style := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).BorderLeft(true).BorderForeground(t.faint).
		PaddingLeft(1).MarginLeft(nestedIndent).Width(min(width-nestedIndent, blockWidth))
	return style.Render(strings.TrimRight(text, "\n"))
}

// answer records what the person chose in a form, once the form is gone.
func (t theme) answer(label, value string) string {
	return "  " + t.paint(t.accent, "◆") + " " + t.paint(t.dim, label) + "  " + value
}

// tracker shows where the run is: done phases, the current one, and those
// still ahead. A narrow terminal gets a short form.
func (t theme) tracker(phases []string, current, width int) string {
	parts := make([]string, 0, len(phases))
	for i, name := range phases {
		switch {
		case i < current:
			parts = append(parts, t.paint(t.green, "●")+" "+t.paint(t.dim, name))
		case i == current:
			parts = append(parts, lipgloss.NewStyle().Foreground(t.accent).Bold(true).Render("◉ "+name))
		default:
			parts = append(parts, t.paint(t.faint, "○ "+name))
		}
	}
	line := "  " + strings.Join(parts, t.paint(t.faint, "  ─  "))
	if lipgloss.Width(line) <= width {
		return line
	}
	return "  " + t.paint(t.accent, fmt.Sprintf("Step %d of %d", current+1, len(phases))) +
		t.paint(t.faint, " · ") + t.bold(phases[current])
}

// results shows what each user's agent got from each API: a row per user, a
// column per API, and a line that says whether every call matched the user's
// access.
func (t theme) results(results []sandbox.Result, width int) string {
	matrix := newResultMatrix(results)
	rows := matrix.rows()
	grid := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(t.faint)).
		Headers(append([]string{"User", "Access"}, matrix.apis...)...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			return t.cell(row, col, rows)
		})
	summary := t.paint(t.green, "✓ "+summarize(results))
	if !allMatch(results) {
		summary = t.paint(t.red, "✗ "+summarize(results))
	}
	margin := lipgloss.NewStyle().MarginLeft(indent)
	return t.section("Results") + "\n" + margin.Render(grid.String()) + "\n" +
		margin.Width(min(width-indent, maxWidth)).Render(summary)
}

func (t theme) cell(row, col int, rows [][]string) lipgloss.Style {
	style := lipgloss.NewStyle().Padding(0, 1)
	if row == table.HeaderRow {
		return style.Bold(true).Foreground(t.accent)
	}
	text := rows[row][col]
	switch {
	case col == 1 && text == accessLabel(true):
		return style.Foreground(t.green)
	case col == 1:
		return style.Foreground(t.yellow)
	case strings.HasPrefix(text, "✓"):
		return style.Foreground(t.green)
	case strings.HasPrefix(text, "✗"):
		return style.Foreground(t.red)
	default:
		return style
	}
}

// resultMatrix arranges results by user and API, in the order the run made
// them.
type resultMatrix struct {
	apis  []string
	users []*sandbox.User
	cells map[*sandbox.User]map[string]sandbox.Result
}

func newResultMatrix(results []sandbox.Result) resultMatrix {
	matrix := resultMatrix{cells: map[*sandbox.User]map[string]sandbox.Result{}}
	for _, result := range results {
		if !slices.Contains(matrix.apis, result.API) {
			matrix.apis = append(matrix.apis, result.API)
		}
		if matrix.cells[result.User] == nil {
			matrix.cells[result.User] = map[string]sandbox.Result{}
			matrix.users = append(matrix.users, result.User)
		}
		matrix.cells[result.User][result.API] = result
	}
	return matrix
}

func (m resultMatrix) rows() [][]string {
	rows := make([][]string, 0, len(m.users))
	for _, user := range m.users {
		row := []string{user.Email, accessLabel(user.Access)}
		for _, api := range m.apis {
			if result, ok := m.cells[user][api]; ok {
				row = append(row, resultCell(result))
			} else {
				row = append(row, "")
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func resultCell(result sandbox.Result) string {
	switch {
	case result.Matches && result.User.Access:
		return "✓ allowed"
	case result.Matches:
		return "✓ refused"
	case !result.User.Access:
		return "✗ went through"
	case result.Reason != "":
		return "✗ " + result.Reason
	default:
		return fmt.Sprintf("✗ HTTP %d", result.Fetch.Status)
	}
}

func allMatch(results []sandbox.Result) bool {
	return !slices.ContainsFunc(results, func(result sandbox.Result) bool { return !result.Matches })
}

func summarize(results []sandbox.Result) string {
	allowed, refused, missed := 0, 0, 0
	for _, result := range results {
		switch {
		case !result.Matches:
			missed++
		case result.User.Access:
			allowed++
		default:
			refused++
		}
	}
	if missed > 0 {
		return fmt.Sprintf("%d of %d calls did not get what the user's access allows.", missed, len(results))
	}
	return fmt.Sprintf("Every call got what the user's access allows: %d allowed, %d refused by the zone.",
		allowed, refused)
}

// commands shows the commands that repeat the run without prompts.
func (t theme) commands(state *sandbox.State, run sandbox.RunInput, width int) string {
	var setup strings.Builder
	setup.WriteString("sandbox setup --zone " + state.ZoneID)
	for _, resource := range state.Resources {
		setup.WriteString(" --resource " + resource.Slug + "=" + keyVariable(resource.Slug))
	}
	lines := []string{setup.String()}
	for _, email := range run.Emails {
		line := "sandbox add-user --email " + email
		if user := state.Users[strings.ToLower(email)]; user != nil && !user.Access {
			line += " --no-access"
		}
		lines = append(lines, line)
	}
	if run.Read != "" {
		lines = append(lines, "sandbox run --read "+run.Read)
	} else {
		lines = append(lines, "sandbox run")
	}
	for i, line := range lines {
		lines[i] = t.paint(t.faint, "$ ") + line
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(t.faint).
		Padding(0, 1).MarginLeft(indent).MaxWidth(width - indent)
	return "\n  " + t.paint(t.dim, "Run it again without prompts") + "\n" + box.Render(strings.Join(lines, "\n"))
}

// keyVariable names the environment variable a command reads an API's key
// from.
func keyVariable(slug string) string {
	return strings.ToUpper(strings.ReplaceAll(slug, "-", "_")) + "_API_KEY"
}

func accessLabel(access bool) string {
	if access {
		return "with access"
	}
	return "without access"
}
