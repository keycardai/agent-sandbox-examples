package main

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

// renderResults shows what each user's agent got from each API: a row per
// user, a column per API, and a line that says whether every call matched
// the user's access.
func renderResults(results []sandbox.Result, width int) string {
	var apis, emails []string
	cells := map[string]map[string]sandbox.Result{}
	access := map[string]bool{}
	for _, r := range results {
		if !contains(apis, r.API) {
			apis = append(apis, r.API)
		}
		email := r.User.Email
		if cells[email] == nil {
			cells[email] = map[string]sandbox.Result{}
			emails = append(emails, email)
		}
		cells[email][r.API] = r
		access[email] = r.User.Access
	}

	rows := make([][]string, 0, len(emails))
	for _, email := range emails {
		row := []string{email, accessText(access[email])}
		for _, api := range apis {
			r, ok := cells[email][api]
			if !ok {
				row = append(row, "")
				continue
			}
			row = append(row, resultCell(r))
		}
		rows = append(rows, row)
	}

	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(styleFaint).
		Headers(append([]string{"User", "Access"}, apis...)...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			cell := lipgloss.NewStyle().Padding(0, 1)
			switch {
			case row == table.HeaderRow:
				return cell.Bold(true).Foreground(colorAccent)
			case col == 1 && rows[row][col] == accessText(true):
				return cell.Foreground(colorGreen)
			case col == 1:
				return cell.Foreground(colorYellow)
			case col > 1 && strings.HasPrefix(rows[row][col], "✓"):
				return cell.Foreground(colorGreen)
			case col > 1 && strings.HasPrefix(rows[row][col], "✗"):
				return cell.Foreground(colorRed)
			}
			return cell
		})

	matched, allowed, refused := 0, 0, 0
	for _, r := range results {
		if r.Matches {
			matched++
			if r.User.Access {
				allowed++
			} else {
				refused++
			}
		}
	}
	summary := styleGreen.Render(
		fmt.Sprintf(
			"✓ Every call got what the user's access allows: %d allowed, %d refused by the zone.",
			allowed,
			refused,
		),
	)
	if matched != len(results) {
		summary = styleRed.Render(
			fmt.Sprintf("✗ %d of %d calls did not get what the user's access allows.", len(results)-matched, len(results)),
		)
	}
	box := lipgloss.NewStyle().MarginLeft(2)
	return "\n  " + styleAccent.Render("▍") + styleBold.Render("Results") + "\n" +
		box.Render(t.String()) + "\n" +
		lipgloss.NewStyle().MarginLeft(2).Width(min(width-2, 90)).Render(summary)
}

func resultCell(r sandbox.Result) string {
	switch {
	case r.Matches && r.User.Access:
		return "✓ allowed"
	case r.Matches:
		return "✓ refused"
	case !r.User.Access:
		return "✗ went through"
	case r.Reason != "":
		return "✗ " + r.Reason
	default:
		return fmt.Sprintf("✗ HTTP %d", r.Fetch.Status)
	}
}

func accessText(access bool) string {
	if access {
		return "with access"
	}
	return "no access"
}

// renderCommands shows the commands that repeat the run without prompts.
func renderCommands(state *sandbox.State, run sandbox.RunInput, width int) string {
	var lines []string
	resources := ""
	for _, r := range state.Resources {
		resources += fmt.Sprintf(" --resource %s=%s_API_KEY", r.Slug, strings.ToUpper(strings.ReplaceAll(r.Slug, "-", "_")))
	}
	lines = append(lines, "sandbox setup --zone "+state.ZoneID+resources)
	for _, email := range run.Emails {
		line := "sandbox add-user --email " + email
		if u := state.Users[strings.ToLower(email)]; u != nil && !u.Access {
			line += " --no-access"
		}
		lines = append(lines, line)
	}
	if run.Read != "" {
		lines = append(lines, "sandbox run --read "+run.Read)
	} else {
		lines = append(lines, "sandbox run")
	}
	for i, l := range lines {
		lines[i] = styleFaint.Render("$ ") + l
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(colorFaint).
		Padding(0, 1).MarginLeft(2).MaxWidth(width - 2)
	return "\n  " + styleDim.Render("Run it again without prompts") + "\n" + box.Render(strings.Join(lines, "\n"))
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
