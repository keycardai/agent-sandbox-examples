package page

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	sampleAPITitle = "Keycard API URL"
	sampleAPIURL   = "https://api.keycard.ai"
	sampleZone     = "Sandbox Test"
	samplePi       = "pi coding agent"
	sampleSmoke    = "Smoke checks"
	inputSecret    = "secret"
)

// testView is what the pages draw with for a checkout with no settings,
// in a window of width and height.
func testView(t *testing.T, width, height int) (*UI, *state.Sandbox) {
	t.Helper()
	return testViewWith(t, &sandbox.Local{Root: t.TempDir(), Settings: sandbox.Settings{}, Out: io.Discard},
		width, height)
}

// testViewWith is what the pages draw with for local, in a window of
// width and height.
func testViewWith(t *testing.T, local *sandbox.Local, width, height int) (*UI, *state.Sandbox) {
	t.Helper()
	ui := NewUI()
	ui.Width, ui.Height = width, height
	return ui, state.New(local.NewSession("127.0.0.1:8480", time.Second), ui.Logs.Log)
}

// wizardSetup is the wizard at width by height, asking quest on the first
// step.
func wizardSetup(t *testing.T, width, height int, quest *Question) *Setup {
	t.Helper()
	setup := NewSetup(testView(t, width, height))
	setup.Rail = PlannedSteps()
	setup.OnSection(SectionMsg{ID: sandbox.SectionAccount})
	setup.Ask(quest)
	return setup
}

// TestQuestionTitle shows the sentence that asks as a question's title,
// with the other sentences under it, and keeps a URL whole.
func TestQuestionTitle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ text, title, about string }{
		{sampleAPITitle, sampleAPITitle, ""},
		{"Tunnel hostname, as https://<hostname>", "Tunnel hostname, as https://<hostname>", ""},
		{
			"PEM file of extra trusted roots, as an absolute path. Leave empty for a public CA.",
			"PEM file of extra trusted roots, as an absolute path.", "Leave empty for a public CA.",
		},
		{
			`The active set is "a". Only one set can be active. Replace it? Its policies stop applying.`,
			"Replace it?", `The active set is "a". Only one set can be active. Its policies stop applying.`,
		},
	} {
		lines := strings.Split(ansi.Strip(wizardSetup(t, 150, 42, &Question{Kind: AskInput, Title: tc.text}).WizardView()),
			"\n")
		title := slices.IndexFunc(lines, func(line string) bool {
			return strings.HasSuffix(strings.TrimSpace(line), "  "+tc.title)
		})
		if title < 0 || tc.about != "" && !strings.Contains(lines[title+1], tc.about[:20]) {
			t.Errorf("%q: want the title %q on its own line, and %q under it:\n%s", tc.text, tc.title, tc.about,
				strings.Join(lines, "\n"))
		}
	}
}

// TestWizardRail marks the steps as setup moves through them, and keeps
// what was chosen under each.
func TestWizardRail(t *testing.T) {
	t.Parallel()
	setup := NewSetup(testView(t, 140, 40))
	setup.Rail = RailSteps()[:5]
	setup.OnSection(SectionMsg{ID: sandbox.SectionZone})
	answerWith(setup, &Question{
		Kind: AskChoose, Title: "Which zone?", Value: "z1",
		Options: []sandbox.Option{{Label: sampleZone, Value: "z1"}},
	}, tea.KeyPressMsg{Code: tea.KeyEnter})
	setup.OnSection(SectionMsg{ID: sandbox.SectionScenario})
	zone, demo := setup.Rail[1], setup.Rail[2]
	if zone.State != RailDone || demo.State != RailCurrent {
		t.Fatalf("states = zone %v, demo %v; want done, current", zone.State, demo.State)
	}
	view := ansi.Strip(setup.WizardView())
	for _, want := range []string{
		"✓  Zone and user", sampleZone, "Demo", "Pick what the agent does", "step 3 of 5", "Working…",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("wizard lacks %q:\n%s", want, view)
		}
	}
	setup.OnSection(SectionMsg{ID: sandbox.SectionPolicy})
	if last := setup.Rail[len(setup.Rail)-1]; last.ID != sandbox.SectionPolicy {
		t.Errorf("last step = %q, want the policy step to join the rail", last.ID)
	}
}

// TestWizardQuestion draws a question's field with every option, says how
// to answer it, and offers only the keys it takes.
func TestWizardQuestion(t *testing.T) {
	t.Parallel()
	demos := &Question{
		Kind:  AskChoose,
		Title: sandbox.QuestionDemo,
		Value: sandbox.DefaultScenario,
		Options: []sandbox.Option{
			{Label: sampleSmoke, Value: sandbox.DefaultScenario},
			{Label: samplePi, Value: "pi"},
			{Label: "Slack agent", Value: "slack"},
		},
	}
	url := &Question{Kind: AskInput, Title: sampleAPITitle, Value: sampleAPIURL}
	secret := &Question{Kind: AskSecret, Title: sandbox.QuestionClientSecret}
	for _, tc := range []struct {
		name   string
		quest  *Question
		want   []string
		choose bool
	}{
		{"choice", demos, []string{sampleSmoke, samplePi, "Slack agent"}, true},
		{"input", url, []string{sampleAPIURL, "enter keeps it. ctrl+u clears it"}, false},
		{"secret", secret, []string{"Hidden as you type", sandbox.EnvClientSecret}, false},
	} {
		view := ansi.Strip(wizardSetup(t, 150, 42, tc.quest).WizardView())
		footer := view[strings.LastIndex(view, "\n")+1:]
		for _, want := range append(tc.want, tc.quest.Title) {
			if !strings.Contains(view, want) {
				t.Errorf("%s: wizard lacks %q:\n%s", tc.name, want, view)
			}
		}
		if !strings.Contains(footer, "enter  continue") || !strings.Contains(footer, "esc  stop setup") ||
			strings.Contains(footer, "↑↓") != tc.choose {
			t.Errorf("%s: footer = %q, want enter and esc, and ↑↓ only for a choice", tc.name, footer)
		}
	}
}

// TestWizardRailFits shows the steps beside the question in a wide window,
// and leaves them out of a narrow one.
func TestWizardRailFits(t *testing.T) {
	t.Parallel()
	quest := &Question{Kind: AskInput, Title: sampleAPITitle}
	if view := ansi.Strip(wizardSetup(t, 150, 42, quest).WizardView()); !strings.Contains(view, "○  Cluster") {
		t.Errorf("a wide wizard lacks the steps:\n%s", view)
	}
	narrow := wizardSetup(t, component.MinWidth, component.MinHeight, quest)
	if view := ansi.Strip(narrow.WizardView()); strings.Contains(view, "○  Cluster") ||
		!strings.Contains(view, "step 1 of 5") {
		t.Errorf("a narrow wizard shows the steps, or not which step it is on:\n%s", view)
	}
}

// TestNotesOfTheStep shows only the notes of the step setup is on: the
// earlier steps' notes stay with them.
func TestNotesOfTheStep(t *testing.T) {
	t.Parallel()
	setup := wizardSetup(t, 150, 42, &Question{Kind: AskInput, Title: sampleAPITitle})
	log := setup.ui.Logs.Log(state.SourceSetup)
	log.Add("Using the client ID from KEYCARD_CLIENT_ID.")
	asker := Asker{Send: func(msg any) {
		if section, ok := msg.(SectionMsg); ok {
			setup.OnSection(section)
		}
	}, Log: log}
	asker.Section(sandbox.SectionZone)
	log.Add("The zone is on this machine or a private network.")
	view := ansi.Strip(setup.WizardView())
	if strings.Contains(view, "client ID") || !strings.Contains(view, "private network") {
		t.Errorf("the zone step shows other notes than its own:\n%s", view)
	}
}

// answerWith asks quest in setup and gives it msgs, then the messages its
// form's commands make, as the TUI's loop does, until it is answered.
func answerWith(setup *Setup, quest *Question, msgs ...tea.Msg) {
	quest.reply = make(chan answer, 1)
	msgs = append(settle(setup.Ask(quest)), msgs...)
	for len(msgs) > 0 && setup.Asking() {
		cmd, _ := setup.Answer(msgs[0])
		msgs = append(msgs[1:], settle(cmd)...)
	}
}

// settle runs cmd and the commands it batches or sequences, and returns
// the messages they make at once. A timer, such as the cursor's blink, is
// left out.
func settle(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	got := make(chan tea.Msg, 1)
	go func() { got <- cmd() }()
	select {
	case msg := <-got:
		cmds, ok := commands(msg)
		if !ok {
			return []tea.Msg{msg}
		}
		var msgs []tea.Msg
		for _, each := range cmds {
			msgs = append(msgs, settle(each)...)
		}
		return msgs
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

// commands returns the commands of a batch or a sequence, which Bubble Tea
// runs itself. A sequence's type is Bubble Tea's own, a slice of commands.
func commands(msg tea.Msg) ([]tea.Cmd, bool) {
	if batch, ok := msg.(tea.BatchMsg); ok {
		return batch, true
	}
	list := reflect.ValueOf(msg)
	if list.Kind() != reflect.Slice || list.Type().Elem() != reflect.TypeFor[tea.Cmd]() {
		return nil, false
	}
	cmds := make([]tea.Cmd, list.Len())
	for i := range cmds {
		cmds[i], _ = reflect.TypeAssert[tea.Cmd](list.Index(i))
	}
	return cmds, true
}

// TestAnswersOnTheRail keeps answers on the rail that read on their own: a
// secret by what it is, never its value, and a yes or no with its
// question. The review shows them whole.
func TestAnswersOnTheRail(t *testing.T) {
	t.Parallel()
	setup := NewSetup(testView(t, 150, 42))
	setup.Rail = PlannedSteps()
	setup.OnSection(SectionMsg{ID: sandbox.SectionCredentials})
	answerWith(setup, &Question{Kind: AskSecret, Title: "Slack bot token (xoxb-), with the im:write scope"},
		tea.PasteMsg{Content: "xoxb-typed"}, tea.KeyPressMsg{Code: tea.KeyEnter})
	answerWith(
		setup,
		&Question{Kind: AskConfirm, Title: "Add an OpenRouter API key?"},
		tea.KeyPressMsg{Code: 'y', Text: "y"},
	)
	setup.Finish()
	view := ansi.Strip(setup.ReviewView())
	for _, want := range []string{"Slack bot token entered", "Add an OpenRouter API key? Yes"} {
		if !strings.Contains(view, want) {
			t.Errorf("the rail lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "xoxb-typed") {
		t.Errorf("the rail shows the secret:\n%s", view)
	}
}

// TestReview shows each step with what was chosen, what launching does and
// how to launch, side by side in a wide window and stacked in a narrow one.
// A problem left takes the launch's place.
func TestReview(t *testing.T) {
	t.Parallel()
	for _, size := range [][2]int{{150, 42}, {component.MinWidth, component.MinHeight}} {
		setup := NewSetup(testView(t, size[0], size[1]))
		setup.Rail = PlannedSteps()
		for _, step := range setup.Rail {
			step.State = RailDone
		}
		setup.Rail[1].Answers = []string{sampleZone}
		view := ansi.Strip(setup.ReviewView())
		for _, want := range []string{"✓  Zone and user", sampleZone, "Ready to launch", "Launching", "enter  Launch"} {
			if !strings.Contains(view, want) {
				t.Errorf("%dx%d review lacks %q:\n%s", size[0], size[1], want, view)
			}
		}
		setup.state.Setup.Problems = []error{sandbox.ErrSettings}
		if view := ansi.Strip(setup.ReviewView()); strings.Contains(view, "Ready to launch") ||
			strings.Contains(view, "enter  launch") || !strings.Contains(view, "needs a fix") {
			t.Errorf("%dx%d review with a problem:\n%s", size[0], size[1], view)
		}
	}
}

// TestLongOptions sizes a choice by its options' wrapped rows, so that an
// option too long for one row shows whole.
func TestLongOptions(t *testing.T) {
	t.Parallel()
	long := "Each person signs in with their own GitHub account, through the provider the zone has for it, " +
		"and the agent acts as them on GitHub, through the proxy"
	setup := wizardSetup(t, 150, 42, &Question{
		Kind: AskChoose, Title: "How does the agent get into GitHub?",
		Value: "a", Options: []sandbox.Option{{Label: long, Value: "a"}, {Label: "One token", Value: "b"}},
	})
	view := ansi.Strip(setup.WizardView())
	for _, want := range []string{"Each person signs in", "through the proxy", "One token"} {
		if !strings.Contains(view, want) {
			t.Errorf("the choice lacks %q:\n%s", want, view)
		}
	}
}

// TestLongAnswerScrolls pastes an answer wider than its field. The text
// scrolls within one row, between the field's top and bottom borders.
func TestLongAnswerScrolls(t *testing.T) {
	t.Parallel()
	for name, kind := range map[string]QuestionKind{"input": AskInput, "secret": AskSecret} {
		setup := NewSetup(testView(t, 150, 42))
		setup.Rail = PlannedSteps()
		setup.Ask(&Question{Kind: kind, Title: "OpenRouter API key"})
		setup.Answer(tea.PasteMsg{Content: strings.Repeat("k", 200)})
		lines := strings.Split(ansi.Strip(setup.WizardView()), "\n")
		row := slices.IndexFunc(lines, func(line string) bool {
			return strings.Contains(line, "kkkk") || strings.Contains(line, "****")
		})
		if row < 1 || row+1 >= len(lines) || !strings.Contains(lines[row-1], "╭") ||
			!strings.Contains(lines[row+1], "╰") {
			t.Fatalf("%s: the answer leaves its one row:\n%s", name, strings.Join(lines, "\n"))
		}
	}
}

// TestResizeKeepsTheField lays a question out again when the window
// narrows, keeping what was typed, with the field's border whole.
func TestResizeKeepsTheField(t *testing.T) {
	t.Parallel()
	setup := wizardSetup(t, 150, 42, &Question{Kind: AskInput, Title: sampleAPITitle})
	setup.Answer(tea.PasteMsg{Content: "https://typed.example"})
	setup.ui.Width = component.MinWidth
	setup.Resize()
	lines := strings.Split(ansi.Strip(setup.WizardView()), "\n")
	for _, line := range lines {
		if ansi.StringWidth(line) > component.MinWidth ||
			strings.Contains(line, "╭") != strings.Contains(line, "╮") {
			t.Fatalf("at %d columns, the field's border breaks:\n%s", component.MinWidth, strings.Join(lines, "\n"))
		}
	}
	if !slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, "typed.example") }) {
		t.Errorf("the typed answer is gone:\n%s", strings.Join(lines, "\n"))
	}
}

// TestAsker sends a question to the TUI and its answer back, answers a
// preset choice without asking, asks a preset the options lack, and
// returns sandbox.ErrInterrupted once the setup stops.
func TestAsker(t *testing.T) {
	t.Parallel()
	queue := make(chan any, 1)
	done := make(chan struct{})
	options := []sandbox.Option{
		{Label: "pi coding agent (pi)", Value: "pi"},
		{Label: "Smoke (smoke)", Value: sandbox.DefaultScenario},
	}
	asker := Asker{
		Send:   func(msg any) { queue <- msg },
		Done:   done,
		Preset: map[string]string{sandbox.QuestionDemo: "pi"},
	}
	if got, err := asker.Choose(
		t.Context(),
		sandbox.QuestionDemo,
		options,
		sandbox.DefaultScenario,
	); err != nil ||
		got != "pi" {
		t.Errorf("Choose = %q, %v; want the preset pi", got, err)
	}
	if msg, ok := (<-queue).(PresetMsg); !ok || msg.text != "pi" {
		t.Errorf("message = %+v, want the preset for the rail", msg)
	}
	go func() {
		if msg, ok := (<-queue).(AskMsg); ok {
			msg.Question.reply <- answer{text: sandbox.DefaultScenario}
		}
	}()
	asker.Preset = map[string]string{sandbox.QuestionDemo: "gone"}
	if got, err := asker.Choose(t.Context(), sandbox.QuestionDemo, options, sandbox.DefaultScenario); err != nil ||
		got != sandbox.DefaultScenario {
		t.Errorf("Choose with a missing preset = %q, %v; want the question asked", got, err)
	}
	close(done)
	if _, err := asker.Input(t.Context(), "Name", "", nil); !errors.Is(err, sandbox.ErrInterrupted) {
		t.Errorf("Input after stop = %v, want sandbox.ErrInterrupted", err)
	}
}

// TestProblemSummary says each problem in words by its kind, and leaves
// an error in a demo's resource that is not a missing value to the error's
// own words.
func TestProblemSummary(t *testing.T) {
	t.Parallel()
	account := zonesetup.Values{ManagementServiceAccount: zonesetup.ManagementServiceAccountValues{
		ClientSecretEnv: zonesetup.DefaultManagementSecretEnv,
	}}
	_, noAccountSecret := zonesetup.LoadSecrets(&account, func(string) (string, bool) { return "", false }, nil)
	_, badResource := zonesetup.ParseValues(strings.NewReader("management_api_url: https://api.keycard.ai\n" +
		"zone_url: https://acme.keycard.cloud\nmanagement_service_account:\n  client_id: sa-1\n" +
		"issuer_url: https://issuer.example.com\nresources:\n  - name: GitHub\n"))
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: %w", sandbox.ErrSettings, sandbox.ErrNoSetup), "Setup has not run yet."},
		{noAccountSecret, "The service account's client secret is missing."},
		{fmt.Errorf("%w: unset: TOKEN (for resources[0])", zonesetup.ErrMissingSecret), "A credential of the demo"},
		{badResource, ""},
	} {
		if got := problemSummary(tc.err); !strings.Contains(got, tc.want) || (tc.want == "") != (got == "") {
			t.Errorf("problemSummary(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// errNoKeychain is a keychain that does not work.
var errNoKeychain = errors.New("no Secret Service answers")

// brokenKeychain is a keychain that cannot keep secrets.
type brokenKeychain struct{ sandbox.Keychain }

func (brokenKeychain) Check() error  { return errNoKeychain }
func (brokenKeychain) Where() string { return "the Secret Service" }

// TestSettingsSayWhyNotSaved says why the settings cannot be saved when the
// keychain does not work, from the answer read with the settings.
func TestSettingsSayWhyNotSaved(t *testing.T) {
	t.Parallel()
	ui, sandboxState := testViewWith(t, &sandbox.Local{
		Root: t.TempDir(), Settings: sandbox.Settings{}, Out: io.Discard,
		Store: sandbox.NewStore(filepath.Join(t.TempDir(), "sandbox.yaml"), nil, brokenKeychain{}),
	}, 150, 42)
	sandboxState.RefreshSettings()
	view := ansi.Strip(NewSettings(ui, sandboxState).View("back"))
	if words := strings.Join(strings.Fields(view), " "); !strings.Contains(words,
		"The keychain cannot keep the secrets: no Secret Service answers.") {
		t.Errorf("the settings do not say why they are not saved:\n%s", view)
	}
}

// TestSavedSettingsWithoutAKeychain says that changes last until the quit
// when the settings were saved before but the keychain does not work now,
// since setup then saves nothing.
func TestSavedSettingsWithoutAKeychain(t *testing.T) {
	t.Parallel()
	pageUI, sandboxState := testViewWith(t, &sandbox.Local{
		Root: t.TempDir(), Settings: sandbox.Settings{}, Out: io.Discard,
		Store: sandbox.NewStore(filepath.Join(t.TempDir(), "sandbox.yaml"), &sandbox.Config{}, brokenKeychain{}),
	}, 150, 42)
	sandboxState.RefreshSettings()
	words := strings.Join(strings.Fields(ansi.Strip(NewSettings(pageUI, sandboxState).View("back"))), " ")
	if !strings.Contains(words, "and changes last until you quit") || strings.Contains(words, "with the secrets in") {
		t.Errorf("the settings say the secrets are kept, though the keychain does not work:\n%s", words)
	}
	if goes := NewSetup(pageUI, sandboxState).secretGoes(); goes != "It stays in memory until you quit." {
		t.Errorf("a typed secret goes: %q, want it to stay in memory", goes)
	}
}
