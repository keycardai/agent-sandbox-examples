package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

// welcomeModel is a first run's welcome screen at width by height, with
// the machine checks given.
func welcomeModel(t *testing.T, width, height int, checks ...sandbox.Check) *model {
	t.Helper()
	root := testModel(t)
	root.screen = screenWelcome
	root.preflight([]sandbox.PreflightItem{{Name: sampleCheck}, {Name: sampleTools}})
	for i, check := range checks {
		root.Update(checkDoneMsg{run: root.state.Setup.Preflight.Run, index: i, check: check})
	}
	root.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return root
}

// TestWelcomeAction says what to do next: wait while the checks run, then
// press enter, with a fix first when a check failed.
func TestWelcomeAction(t *testing.T) {
	t.Parallel()
	passed := sandbox.Check{Name: sampleCheck, Detail: sampleDocker}
	failed := sandbox.Check{Name: sampleTools, State: sandbox.CheckFailed, Detail: "helm is missing"}
	for _, tc := range []struct {
		name   string
		checks []sandbox.Check
		want   []string
		absent []string
	}{
		{
			"checking",
			[]sandbox.Check{passed},
			[]string{"This machine\n", "Checking this machine…"},
			[]string{"Start setup", "is ready"},
		},
		{
			"ready",
			[]sandbox.Check{passed, {Name: sampleTools, Detail: "k3d"}},
			[]string{"This machine is ready", "enter  Start setup"},
			[]string{"Checking", "Fix what failed"},
		},
		{
			"failed",
			[]sandbox.Check{passed, failed},
			[]string{"This machine is not ready", "Fix what failed before you launch", "enter  Start setup"},
			nil,
		},
	} {
		root := welcomeModel(t, 150, 42, tc.checks...)
		view := ansi.Strip(root.View().Content)
		for _, want := range tc.want {
			if !strings.Contains(view, want) && !strings.Contains(trimLines(view), want) {
				t.Errorf("%s: welcome lacks %q:\n%s", tc.name, want, view)
			}
		}
		for _, absent := range tc.absent {
			if strings.Contains(view, absent) {
				t.Errorf("%s: welcome has %q:\n%s", tc.name, absent, view)
			}
		}
	}
}

// trimLines drops the spaces at the end of each line, so that a line can
// be matched with its newline.
func trimLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.Join(lines, "\n")
}

// TestWelcomeLayout centers the welcome text with the setup's steps in a
// tall window, and keeps what to do next on screen in a short one, where
// the text stands alone without the mark and the diagram.
func TestWelcomeLayout(t *testing.T) {
	t.Parallel()
	ready := []sandbox.Check{{Name: sampleCheck, Detail: sampleDocker}, {Name: sampleTools, Detail: "k3d"}}
	tall := welcomeModel(t, 150, 42, ready...)
	view := ansi.Strip(tall.View().Content)
	lines := strings.Split(view, "\n")
	if strings.TrimSpace(lines[2]) != "" {
		t.Errorf("in a tall window, the text starts at the top, not in the middle:\n%s", view)
	}
	for _, step := range page.PlannedSteps() {
		if !strings.Contains(view, step.Title) {
			t.Errorf("the setup's steps lack %q", step.Title)
		}
	}
	mark := ansi.Strip(strings.SplitN(style.Logo(tall.ui.Styles, nil, style.Glint{}), "\n", 2)[0])
	if !strings.Contains(view, mark) {
		t.Errorf("at 150 columns, the mark does not show:\n%s", view)
	}
	short := welcomeModel(t, 80, 22, ready...)
	view = ansi.Strip(short.View().Content)
	if !strings.Contains(view, "enter  Start setup") {
		t.Errorf("at 80x22, what to do next is cut off:\n%s", view)
	}
	if strings.Contains(view, "───▶") || strings.Contains(view, mark) {
		t.Errorf("at 80x22, the diagram or the mark shows instead of making room:\n%s", view)
	}
}

// TestSettingsKey leaves the settings out of a first run, which has none,
// and sends esc on the settings back to the screen that opened them.
func TestSettingsKey(t *testing.T) {
	t.Parallel()
	esc := tea.KeyPressMsg{Code: tea.KeyEscape}
	first := welcomeModel(t, 150, 42)
	for _, hint := range first.welcome.Hints() {
		if hint.Key == "e" {
			t.Error("a first run offers the settings")
		}
	}
	if first.Update(press("e")); first.screen != screenWelcome {
		t.Errorf("e on a first run's welcome showed screen %v", first.screen)
	}
	fix := welcomeModel(t, 150, 42)
	writeSettings(t, fix)
	fix.state.Setup.Problems = []error{sandbox.ErrSettings}
	if fix.Update(press("e")); fix.screen != screenSettings || fix.settingsBackWord() != "back" {
		t.Fatalf("e on the fix screen showed screen %v, esc %q", fix.screen, fix.settingsBackWord())
	}
	if fix.Update(esc); fix.screen != screenWelcome {
		t.Errorf("esc on the settings from the fix screen showed screen %v, want the welcome screen", fix.screen)
	}
	for _, from := range []screen{screenReview, screenDashboard} {
		root := welcomeModel(t, 150, 42)
		writeSettings(t, root)
		root.screen = from
		if from == screenReview {
			root.Update(press("e"))
		} else {
			root.openSettings()
		}
		if root.Update(esc); root.screen != from {
			t.Errorf("esc on the settings from screen %v showed screen %v", from, root.screen)
		}
	}
}

// TestQuitWithNothingRunning only asks, with nothing to stop: no word on
// what keeps running, and D stops nothing.
func TestQuitWithNothingRunning(t *testing.T) {
	t.Parallel()
	root := welcomeModel(t, 150, 42)
	root.Update(press("q"))
	modal := ansi.Strip(root.quitModal())
	if !strings.Contains(modal, "Quit "+style.AppName+"?") || strings.Contains(modal, "tunnel") ||
		strings.Contains(modal, "stop everything") {
		t.Errorf("quit with nothing running:\n%s", modal)
	}
	if _, cmd := root.Update(press("D")); cmd != nil || root.state.Run.Job.Busy() {
		t.Errorf("D with nothing running stopped something: job %+v", root.state.Run.Job)
	}
	root.state.Reads.Status.Tunnel.Running = true
	root.Update(press("q"))
	if modal := ansi.Strip(root.quitModal()); !strings.Contains(modal, "tunnel") ||
		!strings.Contains(modal, "stop everything") {
		t.Errorf("quit with the tunnel up does not say what keeps running:\n%s", modal)
	}
}

// TestWelcomeDropsAnEarlierRun keeps the result of a check from an earlier
// run of the checks out of the run that replaced it.
func TestWelcomeDropsAnEarlierRun(t *testing.T) {
	t.Parallel()
	root := welcomeModel(t, 120, 40)
	earlier := root.state.Setup.Preflight.Run
	root.preflight([]sandbox.PreflightItem{{Name: sampleCheck}, {Name: sampleTools}})
	root.Update(checkDoneMsg{run: earlier, index: 0, check: sandbox.Check{Name: sampleCheck, Detail: "stale"}})
	if view := ansi.Strip(root.View().Content); strings.Contains(view, "stale") {
		t.Errorf("the screen shows a result from the earlier run:\n%s", view)
	}
}

// TestWelcomeChecksTheSettingsAgain reads the settings again on r, as
// after the keychain was unlocked, and not only the machine.
func TestWelcomeChecksTheSettingsAgain(t *testing.T) {
	t.Parallel()
	root := welcomeModel(t, 120, 40, sandbox.Check{Name: sampleCheck}, sandbox.Check{Name: sampleTools})
	root.state.Setup.Problems = []error{fmt.Errorf("%w: the keychain is locked", sandbox.ErrKeychain)}
	root.Update(press("r"))
	if len(root.state.Setup.Problems) != 1 || !errors.Is(root.state.Setup.Problems[0], sandbox.ErrNoSetup) {
		t.Errorf("problems after r = %v, want them read again", root.state.Setup.Problems)
	}
}
