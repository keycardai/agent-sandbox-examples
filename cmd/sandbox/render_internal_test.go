package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

const (
	aliceEmail = "alice@acme.example"
	bobEmail   = "bob@acme.example"
	carolEmail = "carol@acme.example"
	githubAPI  = "GitHub API"
	linearAPI  = "Linear API"
	withAccess = "with access"
	allowed    = "✓ allowed"
	refused    = "✓ refused"
	zoneStep   = "Zone"
	// clusterStep is a step that runs long enough to watch.
	clusterStep = "Sandbox cluster"
	// wide is a terminal wide enough that nothing wraps.
	wide = 200
)

var errBoom = errors.New("boom")

// lines is rendered text as a person reads it: no colors, and no padding at
// the end of a line.
func lines(rendered string) []string {
	out := strings.Split(ansi.Strip(rendered), "\n")
	for i, line := range out {
		out[i] = strings.TrimRight(line, " ")
	}
	return out
}

// tableRows is each row of a rendered table, as its trimmed cells.
func tableRows(rendered string) [][]string {
	var rows [][]string
	for _, line := range lines(rendered) {
		cells := strings.Split(line, "│")
		if len(cells) < 3 {
			continue
		}
		cells = cells[1 : len(cells)-1]
		for i, cell := range cells {
			cells[i] = strings.TrimSpace(cell)
		}
		rows = append(rows, cells)
	}
	return rows
}

func TestResultsShowWhatEachCallGot(t *testing.T) {
	t.Parallel()
	alice := &sandbox.User{Email: aliceEmail, Access: true}
	bob := &sandbox.User{Email: bobEmail}
	results := []sandbox.Result{
		{User: alice, API: githubAPI, Matches: true},
		{User: alice, API: linearAPI, Matches: true},
		{User: bob, API: githubAPI, Matches: true},
		{User: bob, API: linearAPI, Matches: true},
	}

	rendered := newTheme().results(results, wide)

	want := [][]string{
		{"User", "Access", githubAPI, linearAPI},
		{aliceEmail, withAccess, allowed, allowed},
		{bobEmail, "without access", refused, refused},
	}
	if rows := tableRows(rendered); !slices.EqualFunc(rows, want, slices.Equal) {
		t.Errorf("rows = %q, want %q", rows, want)
	}
	const summary = "✓ Every call got what the user's access allows: 2 allowed, 2 refused by the zone."
	if !slices.Contains(lines(rendered), "  "+summary) {
		t.Errorf("no summary %q in\n%s", summary, ansi.Strip(rendered))
	}
}

// TestResultsNameEachMismatch checks that a call that did not get what the
// user's access allows says what went wrong, and that a user the run did not
// call an API for gets an empty cell.
func TestResultsNameEachMismatch(t *testing.T) {
	t.Parallel()
	alice := &sandbox.User{Email: aliceEmail, Access: true}
	bob := &sandbox.User{Email: bobEmail}
	carol := &sandbox.User{Email: carolEmail, Access: true}
	results := []sandbox.Result{
		{User: alice, API: githubAPI, Reason: "exchange_denied"},
		{User: alice, API: linearAPI, Fetch: sandbox.Fetch{Status: 404}},
		{User: bob, API: githubAPI},
		{User: bob, API: linearAPI, Matches: true},
		{User: carol, API: githubAPI, Matches: true},
	}

	rendered := newTheme().results(results, wide)

	want := [][]string{
		{"User", "Access", githubAPI, linearAPI},
		{aliceEmail, withAccess, "✗ exchange_denied", "✗ HTTP 404"},
		{bobEmail, "without access", "✗ went through", refused},
		{carolEmail, withAccess, allowed, ""},
	}
	if rows := tableRows(rendered); !slices.EqualFunc(rows, want, slices.Equal) {
		t.Errorf("rows = %q, want %q", rows, want)
	}
	const summary = "✗ 3 of 5 calls did not get what the user's access allows."
	if !slices.Contains(lines(rendered), "  "+summary) {
		t.Errorf("no summary %q in\n%s", summary, ansi.Strip(rendered))
	}
}

func TestCommandsRepeatTheRun(t *testing.T) {
	t.Parallel()
	state := &sandbox.State{
		ZoneID:    "zone_1",
		Resources: []sandbox.Resource{{Slug: sandbox.SlugGitHub}, {Slug: "x-api"}},
		Users: map[string]*sandbox.User{
			aliceEmail: {Email: aliceEmail, Access: true},
			bobEmail:   {Email: bobEmail},
		},
	}
	look := newTheme()

	for _, tc := range []struct {
		name string
		run  sandbox.RunInput
		last string
	}{
		{
			name: "with a file to read",
			run:  sandbox.RunInput{Read: "acme/docs/readme.md", Emails: []string{aliceEmail, "Bob@acme.example"}},
			last: "$ sandbox run --read acme/docs/readme.md",
		},
		{
			name: "without one",
			run:  sandbox.RunInput{Emails: []string{aliceEmail, "Bob@acme.example"}},
			last: "$ sandbox run",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, line := range lines(look.commands(state, tc.run, wide)) {
				line = strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "│"))
				if strings.HasPrefix(line, "$ ") {
					got = append(got, line)
				}
			}
			want := []string{
				"$ sandbox setup --zone zone_1 --resource github=GITHUB_API_KEY --resource x-api=X_API_API_KEY",
				"$ sandbox add-user --email alice@acme.example",
				"$ sandbox add-user --email Bob@acme.example --no-access",
				tc.last,
			}
			if !slices.Equal(got, want) {
				t.Errorf("commands = %q, want %q", got, want)
			}
		})
	}
}

func TestEventShowsHowAStepEnded(t *testing.T) {
	t.Parallel()
	look := newTheme()

	for _, tc := range []struct {
		name  string
		event sandbox.Event
		want  []string
	}{
		{
			name: "done, with its detail and how long it took",
			event: sandbox.Event{
				Kind: sandbox.EventStepEnd, Text: zoneStep, Detail: "zone_1", Elapsed: 1234 * time.Millisecond,
			},
			want: []string{"  ✓ Zone · zone_1  1.2s"},
		},
		{
			name:  "done quickly",
			event: sandbox.Event{Kind: sandbox.EventStepEnd, Text: zoneStep, Elapsed: time.Millisecond},
			want:  []string{"  ✓ Zone"},
		},
		{
			name:  "failed, with the error under it",
			event: sandbox.Event{Kind: sandbox.EventStepEnd, Text: clusterStep, Err: errBoom},
			want:  []string{"  ✗ Sandbox cluster", "    boom"},
		},
		{
			name:  "started, which the live area shows instead",
			event: sandbox.Event{Kind: sandbox.EventStepStart, Text: clusterStep},
			want:  []string{""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := lines(look.event(tc.event, wide)); !slices.Equal(got, tc.want) {
				t.Errorf("lines = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEventFitsALongStepToTheTerminal(t *testing.T) {
	t.Parallel()
	event := sandbox.Event{Kind: sandbox.EventStepEnd, Text: clusterStep, Detail: strings.Repeat("x", 100)}

	line := newTheme().event(event, 40)

	if width := ansi.StringWidth(line); width != 40 || !strings.HasSuffix(ansi.Strip(line), "…") {
		t.Errorf("line %q is %d wide, want 40 ending in …", ansi.Strip(line), width)
	}
}

func TestTrackerShortensOnANarrowTerminal(t *testing.T) {
	t.Parallel()
	look := newTheme()

	const full = "  ● Connect  ─  ● Zone  ─  ◉ APIs  ─  ○ Setup  ─  ○ Users  ─  ○ Agent"
	if got := ansi.Strip(look.tracker(wizardPhases(), phaseAPIs, wide)); got != full {
		t.Errorf("tracker = %q, want %q", got, full)
	}
	if got := ansi.Strip(look.tracker(wizardPhases(), phaseAPIs, 40)); got != "  Step 3 of 6 · APIs" {
		t.Errorf("narrow tracker = %q", got)
	}
}
