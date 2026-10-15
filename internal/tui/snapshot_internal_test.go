package tui

import (
	"flag"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

// update rewrites the snapshots' golden files from the screens drawn now.
//
//nolint:gochecknoglobals // A test flag is a package variable.
var update = flag.Bool("update", false, "rewrite the snapshots' golden files in testdata")

// clockTime matches a time of day, which differs on every run.
var clockTime = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}\b`)

// TestSnapshots draws each screen, with sample state, at a large size and
// the smallest, and compares it with its golden file in testdata: the text
// without its colors, the spaces at the end of each line, and the times of
// day. -update writes the golden files from what the screens draw now.
// TUI_SNAPSHOTS names a directory to write each screen to with its colors,
// as ANSI text, for a person to look at.
func TestSnapshots(t *testing.T) {
	t.Parallel()
	colors := os.Getenv("TUI_SNAPSHOTS")
	for name, build := range snapshots() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := build(t)
			for _, size := range []struct {
				suffix        string
				width, height int
			}{{"", 150, 42}, {"-small", component.MinWidth, component.MinHeight}} {
				root.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
				content := root.View().Content
				checkGolden(t, filepath.Join("testdata", name+size.suffix+".golden"), plainScreen(content))
				if colors != "" {
					//nolint:gosec // G703: the directory is the one the person running the test named.
					if err := os.WriteFile(filepath.Join(colors, name+size.suffix+".ans"), []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

// plainScreen is a screen as its golden file keeps it: no colors, no spaces
// at the end of a line, and each time of day as hh:mm:ss.
func plainScreen(content string) string {
	lines := strings.Split(ansi.Strip(content), "\n")
	for i, line := range lines {
		lines[i] = clockTime.ReplaceAllString(strings.TrimRight(line, " "), "hh:mm:ss")
	}
	return strings.Join(lines, "\n") + "\n"
}

// checkGolden compares got with the golden file at path, or writes it with
// -update.
func checkGolden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: the path is a golden file under testdata.
	if err != nil {
		t.Fatalf("%v. Run go test ./internal/tui -run TestSnapshots -update to write it.", err)
	}
	if got == string(want) {
		return
	}
	gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
	for i := range max(len(gotLines), len(wantLines)) {
		var gotLine, wantLine string
		if i < len(gotLines) {
			gotLine = gotLines[i]
		}
		if i < len(wantLines) {
			wantLine = wantLines[i]
		}
		if gotLine != wantLine {
			t.Errorf("%s differs at line %d:\n got %q\nwant %q\nThe screen now:\n%s", path, i+1, gotLine, wantLine, got)
			return
		}
	}
}

// snapshots builds each screen the snapshots draw, by name.
func snapshots() map[string]func(t *testing.T) *model {
	return map[string]func(t *testing.T) *model{
		"welcome":   snapshotWelcome,
		"ready":     snapshotReady,
		"not-ready": snapshotNotReady,
		"light":     snapshotLight,
		"fix":       snapshotFix,
		"wizard":    snapshotWizard,
		"w-input":   snapshotWizardInput,
		"w-secret":  snapshotWizardSecret,
		"w-confirm": snapshotWizardConfirm,
		"w-long":    snapshotWizardLong,
		"review":    snapshotReview,
		"bringup":   snapshotBringUp,
		"home":      snapshotUp,
		"w-save":    snapshotSave,
		"demos":     snapshotDemos,
	}
}

func snapshotModel(t *testing.T) *model {
	t.Helper()
	root := testModel(t)
	root.Update(tea.WindowSizeMsg{Width: 150, Height: 42})
	root.shared.Frame = 3
	return root
}

// sampleImages is the machine check of the sandbox's images.
const sampleImages = "Sandbox images"

// snapshotChecks are the machine checks the welcome snapshots show, in the
// order they run.
func snapshotChecks() []sandbox.PreflightItem {
	return []sandbox.PreflightItem{
		{Name: sampleCheck}, {Name: sampleImages}, {Name: sampleTools}, {Name: sampleAccount},
	}
}

// checked reports the results of the machine checks, from the first.
func checked(root *model, checks ...sandbox.Check) {
	for i, check := range checks {
		root.Update(page.CheckDoneMsg{Index: i, Check: check})
	}
}

// snapshotWelcome is a first run's welcome screen with two of its four
// machine checks done.
func snapshotWelcome(t *testing.T) *model {
	t.Helper()
	root := snapshotModel(t)
	root.screen = screenWelcome
	root.welcome.Preflight(snapshotChecks())
	checked(root, sandbox.Check{Name: sampleCheck, Detail: sampleDocker},
		sandbox.Check{Name: sampleImages, Detail: "the release v0.2.1 from ghcr.io/keycardai"})
	root.shared.Background = snapshotBackground
	return root
}

// snapshotBackground is the dark terminal background the snapshots assume.
//
//nolint:gochecknoglobals // A test constant of a struct type.
var snapshotBackground = color.RGBA{R: 0x1E, G: 0x1F, B: 0x2E, A: 0xFF}

// readyChecks are the four machine checks, passed.
func readyChecks() []sandbox.Check {
	return []sandbox.Check{
		{Name: sampleCheck, Detail: sampleDocker},
		{Name: sampleImages, Detail: "the release v0.2.1 from ghcr.io/keycardai"},
		{Name: sampleTools, Detail: "k3d, kubectl, helm, cloudflared (from mise.toml)"},
		{Name: sampleAccount, Detail: "from KEYCARD_CLIENT_ID and KEYCARD_CLIENT_SECRET"},
	}
}

// snapshotReady is the welcome screen once every check passed.
func snapshotReady(t *testing.T) *model {
	t.Helper()
	root := snapshotWelcome(t)
	root.welcome.Preflight(snapshotChecks())
	checked(root, readyChecks()...)
	return root
}

// snapshotNotReady is the welcome screen with a check that failed.
func snapshotNotReady(t *testing.T) *model {
	t.Helper()
	root := snapshotWelcome(t)
	checks := readyChecks()
	checks[0] = sandbox.Check{
		Name: sampleCheck, State: sandbox.CheckFailed, Detail: "Docker is not running",
		Fix: "Start OrbStack or Docker Desktop.",
	}
	root.welcome.Preflight(snapshotChecks())
	checked(root, checks...)
	return root
}

// snapshotLight is the welcome screen on a light terminal.
func snapshotLight(t *testing.T) *model {
	t.Helper()
	root := snapshotWelcome(t)
	root.Update(tea.BackgroundColorMsg{Color: color.White})
	return root
}

func snapshotFix(t *testing.T) *model {
	t.Helper()
	root := snapshotWelcome(t)
	writeSettings(t, root)
	root.shared.Problems = []error{fmt.Errorf("%w: zone-setup.yaml: resources[2]: set exactly one of "+
		"credential_provider, secret_env and secret_file", zonesetup.ErrValues)}
	root.Update(page.CheckDoneMsg{Index: 2, Check: sandbox.Check{
		Name: sampleTools, Detail: "k3d, kubectl, helm and cloudflared, as mise.toml pins them",
	}})
	root.Update(page.CheckDoneMsg{Index: 3, Check: sandbox.Check{
		Name: sampleAccount, Detail: "KEYCARD_CLIENT_ID and KEYCARD_CLIENT_SECRET are set",
	}})
	return root
}

// writeSettings gives the model settings, as setup keeps them for the
// session.
func writeSettings(t *testing.T, root *model) {
	t.Helper()
	root.shared.Session.Local().Store = sandbox.NewStore(sandbox.NoConfig, &sandbox.Config{}, nil)
}

// ask shows a setup question, as the wizard sends it while setup runs.
func ask(root *model, quest *page.Question) {
	if !root.asking {
		root.asking, root.stopAsking = true, make(chan struct{})
	}
	root.Update(page.AskMsg{Question: quest})
}

func snapshotWizard(t *testing.T) *model {
	t.Helper()
	root := snapshotModel(t)
	root.setup.Rail = page.RailSteps()[:5]
	root.setup.Rail[0].State, root.setup.Rail[0].Answers = page.RailKept, []string{
		"wTJoY8LF3nZBzx_UGS17O (from KEYCARD_CLIENT_ID)",
	}
	root.screen = screenWizard
	root.Update(page.SectionMsg{ID: sandbox.SectionZone})
	root.setup.Rail[1].Answers = []string{sampleZone, "ada@example.com"}
	root.Update(page.SectionMsg{ID: sandbox.SectionScenario})
	root.shared.Logs.Log(page.SourceSetup).Add("Using the client ID from KEYCARD_CLIENT_ID.",
		"Using the client secret from KEYCARD_CLIENT_SECRET.", "Signed in to Keycard as the organization Acme.")
	ask(root, &page.Question{
		Kind: page.AskChoose, Title: "Which demo?", Value: sandbox.DefaultScenario, Options: []sandbox.Option{
			{Label: "Smoke checks (smoke)", Value: sandbox.DefaultScenario},
			{Label: "pi coding agent (pi)", Value: "pi"},
			{Label: "Slack agent for GitHub and Linear (slack)", Value: sampleSlack},
		},
	})
	return root
}

// wizardAt is the wizard on its first step, with the steps a first run
// shows.
func wizardAt(t *testing.T, section string) *model {
	t.Helper()
	root := snapshotModel(t)
	root.setup.Rail = page.PlannedSteps()
	root.screen = screenWizard
	root.Update(page.SectionMsg{ID: section})
	return root
}

func snapshotWizardInput(t *testing.T) *model {
	t.Helper()
	root := wizardAt(t, sandbox.SectionAccount)
	ask(root, &page.Question{Kind: page.AskInput, Title: sampleAPITitle, Value: sampleAPIURL})
	return root
}

func snapshotWizardSecret(t *testing.T) *model {
	t.Helper()
	root := wizardAt(t, sandbox.SectionAccount)
	ask(root, &page.Question{Kind: page.AskSecret, Title: "Service account client secret"})
	return root
}

func snapshotWizardConfirm(t *testing.T) *model {
	t.Helper()
	root := wizardAt(t, sandbox.SectionZone)
	root.Update(page.SectionMsg{ID: sandbox.SectionPolicy})
	ask(root, &page.Question{Kind: page.AskConfirm, Title: `The zone's active policy set is "default". Only one ` +
		`set can be active. Replace it with "sandbox" when the sandbox runs? Its policies will stop applying to ` +
		`everyone in the zone.`})
	return root
}

// longOptions is more zones than a choice shows at once.
const longOptions = 12

func snapshotWizardLong(t *testing.T) *model {
	t.Helper()
	root := wizardAt(t, sandbox.SectionZone)
	options := make([]sandbox.Option, 0, longOptions)
	for i := range longOptions {
		name := fmt.Sprintf("zone-%02d", i+1)
		options = append(options, sandbox.Option{Label: name + "  https://" + name + ".keycard.cloud", Value: name})
	}
	ask(root, &page.Question{
		Kind: page.AskChoose, Title: "Which zone does the sandbox use?", Value: "zone-01", Options: options,
	})
	return root
}

func snapshotReview(t *testing.T) *model {
	t.Helper()
	root := snapshotModel(t)
	root.screen = screenReview
	root.shared.Describe = map[string]string{
		sandbox.SectionAccount: "api.keycard.ai, with the service account from KEYCARD_CLIENT_ID",
		sandbox.SectionZone:    "ada@example.com, in the zone at wvqowjom3ctf3eysdimuj4h745.keycard.cloud",
		sandbox.SectionScenario: "Check the proxy with GitHub and OpenRouter (smoke). GitHub: saved key, " +
			"GitHub git: saved key, OpenRouter: saved key",
		sandbox.SectionCluster: "A local cluster on this machine, reached through a Cloudflare quick tunnel",
	}
	root.shared.Problems = nil
	root.setup.Rail = page.PlannedSteps()
	for i, answers := range [][]string{
		{sampleAPIURL},
		{sampleZone, "ada@example.com"},
		{"Check the proxy with GitHub and OpenRouter (smoke)"},
		{
			"One personal access token that everyone shares. The agent acts as its owner.",
			"GitHub personal access token saved", "Add an OpenRouter API key? Yes", "OpenRouter API key saved",
		},
		{
			"On this machine, in a new local cluster",
			"A Cloudflare quick tunnel. No account needed, but each start gets a new URL and a new cluster.",
		},
	} {
		root.setup.Rail[i].State, root.setup.Rail[i].Answers = page.RailDone, answers
	}
	return root
}

func snapshotSave(t *testing.T) *model {
	t.Helper()
	root := snapshotModel(t)
	root.setup.Rail = page.PlannedSteps()
	root.screen = screenWizard
	root.Update(page.SectionMsg{ID: sandbox.SectionSave})
	ask(root, &page.Question{Kind: page.AskConfirm, Yes: true, Title: sandbox.QuestionSave +
		" Yes saves them in ~/.config/keycard/sandbox.yaml, and the secrets you typed in the macOS keychain. " +
		"No keeps them until you quit."})
	return root
}

// snapshotUp is a sandbox that is up, at the snapshots' size, with what
// happened on the way.
func snapshotUp(t *testing.T) *model {
	t.Helper()
	root := upModel(t)
	root.Update(tea.WindowSizeMsg{Width: 150, Height: 42})
	root.shared.Frame = 3
	now := time.Now()
	for i, row := range root.shared.Steps {
		row.State, row.Started, row.Ended = page.StepDone, now.Add(-time.Duration(300-i*25)*time.Second),
			now.Add(-time.Duration(290-i*25)*time.Second)
	}
	for _, event := range [][2]string{
		{"▸", "Bring-up started"},
		{"✓", "Start the tunnel (3s)"},
		{"✓", "Install the charts (41s)"},
		{"✓", "Checks: 6 of 6 passed"},
		{"✓", "The sandbox is up"},
	} {
		root.shared.Record(event[0], event[1])
	}
	return root
}

func snapshotBringUp(t *testing.T) *model {
	t.Helper()
	root := snapshotUp(t)
	root.shared.Running = true
	now := time.Now()
	for i, row := range root.shared.Steps {
		switch {
		case i == 6:
			row.State, row.Started, row.Ended = page.StepRunning, now.Add(-41*time.Second), time.Time{}
			row.Log.Add(`Release "keycard-sandbox" has been upgraded. Happy Helming!`, "NAME: keycard-sandbox",
				"STATUS: deployed", "Waiting for deployment agent-sandbox-controller to be ready")
		case i > 6:
			row.State = page.StepPending
		}
	}
	root.shared.StepCursor = 6
	return root
}

func snapshotDemos(t *testing.T) *model {
	t.Helper()
	root := snapshotUp(t)
	root.shared.Demos = sampleDemos()
	root.Update(press("p"))
	return root
}
