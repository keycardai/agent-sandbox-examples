package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

// Sample data the tests share.
const (
	sampleMethod   = "GET"
	sampleDenied   = "example.com"
	sampleCheck    = "Docker"
	sampleDocker   = "OrbStack, engine 29.4.0"
	sampleZone     = "Sandbox Test"
	samplePod      = "u7k2vjw9ab"
	verdictAllow   = "allow"
	verdictDeny    = "deny"
	sampleAllowed  = "endpoint_allowed"
	sampleReason   = "no_matching_endpoint"
	sampleTools    = "Tools"
	sampleAccount  = "Keycard account"
	sampleAPITitle = "Keycard API URL"
	sampleAPIURL   = "https://api.keycard.ai"
	sampleSlack    = "slack"
	samplePi       = "pi coding agent"
	sampleSmoke    = "Smoke checks"
	// sampleUserPath is the GitHub request the Agents tab sends.
	sampleUserPath = "api.github.com/user"
)

// testSession is a session for a checkout with no settings.
func testSession(t *testing.T) *sandbox.Session {
	t.Helper()
	local := &sandbox.Local{Root: t.TempDir(), Settings: sandbox.Settings{}, Out: io.Discard}
	return local.NewSession("127.0.0.1:8480", time.Second)
}

// testModel returns a model for a checkout with no settings, sized like a
// laptop terminal, as after the first read of the sandbox. Its work in the
// background ends with the test.
func testModel(t *testing.T) *model {
	t.Helper()
	root := newModel(t.Context(), testSession(t))
	root.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	root.shared.Read = true
	return root
}

func press(text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: []rune(text)[0], Text: text}
}

// TestStepsFollowTheBringUp marks steps as the runner reports them, and
// starts again at the step that failed.
func TestStepsFollowTheBringUp(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	steps := root.shared.Steps
	if len(steps) != 10 || steps[0].Step.ID != sandbox.StepTunnel {
		t.Fatalf("steps = %d, first %q; want the 10 k3d steps from the tunnel", len(steps), steps[0].Step.ID)
	}
	root.Update(stepStartMsg{index: 0})
	root.Update(stepDoneMsg{index: 0})
	root.Update(stepStartMsg{index: 1})
	root.Update(stepDoneMsg{index: 1, err: sandbox.ErrDocker})
	if steps[0].State != page.StepDone || steps[1].State != page.StepFailed {
		t.Errorf("states = %v, %v; want done, failed", steps[0].State, steps[1].State)
	}
	if got := root.shared.ResumeIndex(); got != 0 {
		t.Errorf("ResumeIndex with the tunnel stopped = %d, want 0", got)
	}
	root.shared.Status.Tunnel.Running = true
	if got := root.shared.ResumeIndex(); got != 1 {
		t.Errorf("ResumeIndex = %d, want 1", got)
	}
	lines, _ := steps[1].Log.Snapshot()
	if want := "→ Start Docker"; !strings.Contains(strings.Join(lines, "\n"), want) {
		t.Errorf("failed step's log = %q, want the hint %q", lines, want)
	}
	if badge := ansi.Strip(root.statusBadge()); !strings.Contains(badge, "stopped at Create or check the cluster") {
		t.Errorf("badge = %q, want it to name the failed step", badge)
	}
}

// TestRecreateAsks offers to recreate a cluster made for an older tunnel
// URL, or one that creating stopped partway, and says which. Recreating
// deletes the cluster, so only y says yes: enter says no.
func TestRecreateAsks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, why string
		err       error
	}{
		{"other issuer", "older tunnel URL", errors.Join(sandbox.ErrIssuer, sandbox.ErrOtherIssuer)},
		{"no kubeconfig context", "stopped partway", sandbox.ErrBrokenCluster},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := testModel(t)
			root.Update(stepDoneMsg{index: 1, err: tc.err})
			view := ansi.Strip(root.View().Content)
			if root.overlay.kind != overlayConfirm || !strings.Contains(view, "Recreate the cluster?") ||
				!strings.Contains(view, tc.why) || !strings.Contains(view, "y  yes") ||
				strings.Contains(view, "enter  yes") {
				t.Errorf("overlay %v; want the question, why, and y and not enter to answer yes:\n%s",
					root.overlay.kind, view)
			}
		})
	}
}

// TestQuitLeavesTheSandboxRunning asks how to leave, and Enter quits
// without stopping anything.
func TestQuitLeavesTheSandboxRunning(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	if _, cmd := root.Update(press("q")); cmd != nil || root.overlay.kind != overlayQuit {
		t.Fatalf("q: overlay %v; want the quit dialog and no command yet", root.overlay.kind)
	}
	_, cmd := root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter in the quit dialog returned no command, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok || root.quitAfterAction {
		t.Errorf("Enter: quit %v, stop everything %v; want a plain quit", ok, root.quitAfterAction)
	}
}

// TestTabsAndPanes switches tabs with the number keys, moves through the
// Health tab's checks, and shows the log of the one selected.
func TestTabsAndPanes(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	for _, tc := range []struct {
		key    tea.KeyPressMsg
		tab    tab
		source string
	}{
		{press("4"), tabHealth, page.ContainerSource("", page.AgentContainer)},
		{press("j"), tabHealth, page.SourceTraffic},
		{tea.KeyPressMsg{Code: tea.KeyTab}, tabHealth, page.SourceTraffic},
		{press("3"), tabTraffic, page.SourceTraffic},
		{press("d"), tabTraffic, page.SourceTrafficAll},
		{press("t"), tabHealth, page.SourceActivity},
		{press("1"), tabHome, page.SourceActivity},
	} {
		root.Update(tc.key)
		if source := root.shared.Logs.Source(); root.tab != tc.tab || source != tc.source {
			t.Errorf("%s: tab %v, source %q; want %v, %q", tc.key, root.tab, source, tc.tab, tc.source)
		}
	}
	root.Update(press("4"))
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if root.health.Focus != page.PaneChecks || root.shared.Logs.Source() != page.SourceTrafficAll {
		t.Errorf("esc from the log: focus %v, source %q; want the requests check", root.health.Focus,
			root.shared.Logs.Source())
	}
	// On the Health tab, a number still switches tabs: the panes have no
	// numbers.
	if view := ansi.Strip(root.View().Content); strings.Contains(view, "1 Bring-up") {
		t.Errorf("the Bring-up pane still shows a number:\n%s", view)
	}
	root.Update(press("1"))
	if root.tab != tabHome {
		t.Errorf("1 on the Health tab: tab %v, want Home", root.tab)
	}
}

// TestViewSizes draws every screen at a laptop size and a small size, and
// says when the window is too small.
func TestViewSizes(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	for _, size := range []tea.WindowSizeMsg{{Width: 140, Height: 40}, {Width: 80, Height: 22}} {
		root.Update(size)
		for _, view := range []screen{screenDashboard, screenWelcome, screenWizard, screenReview, screenSettings} {
			root.screen = view
			content := ansi.Strip(root.View().Content)
			if lines := strings.Split(content, "\n"); len(lines) > size.Height {
				t.Errorf("%dx%d screen %d: %d lines, want at most %d", size.Width, size.Height, view, len(lines),
					size.Height)
			}
			for line := range strings.SplitSeq(content, "\n") {
				if ansi.StringWidth(line) > size.Width {
					t.Errorf("%dx%d screen %d: line wider than the window: %q", size.Width, size.Height, view, line)
					break
				}
			}
		}
	}
	root.Update(tea.WindowSizeMsg{Width: 60, Height: 15})
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "too small") {
		t.Errorf("60x15 view = %q, want it to say the window is too small", view)
	}
}

// TestHealthTabShowsTheChecks draws the tabs, the checks, the log pane and
// the footer's keys.
func TestHealthTabShowsTheChecks(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.screen = screenDashboard
	root.Update(press("4"))
	view := ansi.Strip(root.View().Content)
	for _, want := range []string{
		"keycard sandbox", "1 Home", "2 Agent", "3 Traffic", "4 Health", "The sandbox is not running",
		"The agent runs", "Its requests go through Keycard", "Keycard can verify the agent", "The checks pass",
		"every log", "bring up", "quit",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("Health tab lacks %q:\n%s", want, view)
		}
	}
}

// TestChecksScore counts the passed checks from the checks step's log, and
// the Health tab shows the score.
func TestChecksScore(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	last := len(root.shared.Steps) - 1
	row := root.shared.Steps[last]
	row.Log.Add("  PASS  allowed", "  FAIL  credentialed", "  PASS  denied", "  MANUAL  event log")
	root.Update(stepStartMsg{index: last})
	root.Update(stepDoneMsg{index: last, err: sandbox.ErrCheck})
	root.Update(press("4"))
	if view := ansi.Strip(root.View().Content); row.Passed != 2 || row.Total != 3 ||
		!strings.Contains(view, "2/3 passed") {
		t.Errorf("checks = %d/%d, want 2/3 passed on the Health tab:\n%s", row.Passed, row.Total, view)
	}
}

// TestFirstRunWelcome greets a checkout with no settings: the mark, what
// the sandbox does, and the machine checks as they run.
func TestFirstRunWelcome(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.screen = screenWelcome
	root.welcome.Preflight([]sandbox.PreflightItem{{Name: sampleCheck}, {Name: sampleTools}})
	root.Update(page.CheckDoneMsg{Index: 0, Check: sandbox.Check{Name: sampleCheck, Detail: sampleDocker}})
	mark := style.Logo(root.shared.Styles, root.shared.Background, style.Glint{})
	view := ansi.Strip(root.View().Content)
	for _, want := range []string{
		"Run an AI agent that never holds a secret.", "egress proxy", "Keycard zone", "This machine",
		sampleDocker, "checking…", "start setup", ansi.Strip(strings.SplitN(mark, "\n", 2)[0]),
	} {
		if !strings.Contains(view, want) {
			t.Errorf("welcome lacks %q:\n%s", want, view)
		}
	}
	activity, _ := root.shared.Logs.Log(page.SourceActivity).Snapshot()
	if len(activity) != 1 || !strings.Contains(activity[0], "✓ Docker: OrbStack") {
		t.Errorf("activity = %q, want the Docker check", activity)
	}
}

// TestStoppedSetupGoesBack shows the screen the wizard started from when
// the setup stops before it saves. A setup that failed says why there,
// until the next setup starts.
func TestStoppedSetupGoesBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"stopped", sandbox.ErrInterrupted, ""},
		{"refused", fmt.Errorf("%w: signing in: refused", sandbox.ErrSettings), "✕ Setup stopped: settings"},
	} {
		root := testModel(t)
		root.screen, root.wizardBack = screenWizard, screenWelcome
		root.Update(setupDoneMsg{err: tc.err})
		view := ansi.Strip(root.View().Content)
		if root.screen != screenWelcome || strings.Contains(view, "✕ Setup stopped") != (tc.want != "") ||
			!strings.Contains(view, tc.want) {
			t.Errorf("%s: screen %v; want the welcome screen, with %q:\n%s", tc.name, root.screen, tc.want, view)
		}
	}
}

// pump hands what background work sends to the model until done reports
// true.
func pump(t *testing.T, root *model, done func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !done() {
		select {
		case msg := <-root.bus.ch:
			root.Update(msg)
		case <-deadline:
			t.Fatal("the background work did not finish")
		}
	}
}

// TestEscStopsTheSetup stops a setup between questions, while it works,
// and goes back to the screen it started from.
func TestEscStopsTheSetup(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.screen = screenWelcome
	root.startWizard(screenWelcome, nil)
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	pump(t, root, func() bool { return !root.asking })
	if root.screen != screenWelcome || root.overlay.kind != overlayNone {
		t.Errorf("after esc: screen %v, overlay %v; want the welcome screen", root.screen, root.overlay.kind)
	}
}

// TestQuestionWaitsForTheDialog keeps the quit dialog over a setup question
// that comes while it shows, so that a key meant for the dialog does not
// answer the question. The question shows once the dialog closes.
func TestQuestionWaitsForTheDialog(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.screen, root.asking, root.stopAsking = screenWizard, true, make(chan struct{})
	defer root.stopSetup()
	root.Update(press("q"))
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "Setup stops, and keeps none") {
		t.Errorf("the quit dialog does not say the setup stops:\n%s", view)
	}
	asker := page.Asker{Send: root.bus.Send, Done: root.stopAsking}
	go func() { _, _ = asker.Confirm(t.Context(), "Turn on the sandbox's access rules in this zone?", false) }()
	root.Update(<-root.bus.ch)
	if root.overlay.kind != overlayQuit {
		t.Fatalf("overlay %v once the question came, want the quit dialog", root.overlay.kind)
	}
	root.Update(press("n"))
	if root.overlay.kind != overlayQuestion {
		t.Errorf("overlay %v once the dialog closed, want the question", root.overlay.kind)
	}
}

// TestQuestionWaitsForTheLists keeps a question for a new agent that comes
// while the list of every log shows, and shows it once the list closes.
func TestQuestionWaitsForTheLists(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.asking, root.stopAsking, root.askTitle = true, make(chan struct{}), newAgentTitle
	defer root.stopSetup()
	root.Update(press("l"))
	asker := page.Asker{Send: root.bus.Send, Done: root.stopAsking}
	go func() { _, _ = asker.Input(t.Context(), "Who gets an agent?", "", nil) }()
	root.Update(<-root.bus.ch)
	if root.overlay.kind != overlayLogs {
		t.Fatalf("overlay %v once the question came, want the list of every log", root.overlay.kind)
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if root.overlay.kind != overlayQuestion {
		t.Errorf("overlay %v once the list closed, want the question", root.overlay.kind)
	}
}

// TestAddingAnAgentWaits says, while it signs in for a new agent, that the
// operator can stop it with esc, and esc stops it.
func TestAddingAnAgentWaits(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.asking, root.stopAsking, root.askTitle = true, make(chan struct{}), newAgentTitle
	if root.Update(press("u")); !strings.Contains(root.shared.Toast.Text, "Adding an agent") {
		t.Errorf("u while adding an agent: notice %q", root.shared.Toast.Text)
	}
	stop := root.stopAsking
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	select {
	case <-stop:
	default:
		t.Error("esc did not stop adding the agent")
	}
}

// TestActionLogStays keeps an action's log in the pane when the pods are
// read again, so that how it ended stays on screen, and lists it with
// every log.
func TestActionLogStays(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(press("2"))
	root.Update(press("R"))
	root.Update(press("y"))
	root.Update(podsMsg{rows: root.shared.AllPods, agents: root.shared.Agents})
	if source := root.shared.Logs.Source(); source != page.SourceAction {
		t.Errorf("the pane shows %q once the pods were read, want the action's log", source)
	}
	if root.Update(press("l")); !strings.Contains(ansi.Strip(root.View().Content), "Last action") {
		t.Errorf("every log lacks the last action's:\n%s", ansi.Strip(root.View().Content))
	}
}

// TestDeployWaitsForTheSandbox brings a new agent up only when the
// sandbox is up, and shows that it is coming up.
func TestDeployWaitsForTheSandbox(t *testing.T) {
	t.Parallel()
	const newcomer = "grace@example.com"
	for _, sandboxUp := range []bool{true, false} {
		root := upModel(t)
		root.shared.Status.Tunnel.Running = sandboxUp
		agents := append(slices.Clone(root.shared.Agents), sandbox.Agent{Email: newcomer})
		root.Update(press("2"))
		root.Update(agentAddedMsg{email: newcomer, agents: agents})
		view := ansi.Strip(root.View().Content)
		switch {
		case sandboxUp && (root.acting != page.DeployTitle(newcomer) || !strings.Contains(view, "coming up")):
			t.Errorf("sandbox up: action %q; want the agent coming up:\n%s", root.acting, view)
		case !sandboxUp && (root.acting != "" || !strings.Contains(root.shared.Toast.Text, "comes up with the sandbox")):
			t.Errorf("sandbox down: action %q, notice %q; want only a notice", root.acting, root.shared.Toast.Text)
		}
		root.stop()
	}
}

// TestPolicyAnswerHolds takes the answer about the zone's policy set from
// the last setup that looked at the zone, so that a no after a yes holds.
func TestPolicyAnswerHolds(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	for _, tc := range []struct {
		name          string
		asks          []string
		replace, want bool
	}{
		{"yes", []string{sandbox.SectionZone, sandbox.SectionPolicy}, true, true},
		{"the cluster alone", []string{sandbox.SectionCluster}, false, true},
		{"no", []string{sandbox.SectionZone, sandbox.SectionPolicy}, false, false},
	} {
		root.setup.Begin(tc.asks[0])
		for _, id := range tc.asks {
			root.Update(page.SectionMsg{ID: id})
		}
		root.Update(setupDoneMsg{result: sandbox.InitResult{ReplaceActive: tc.replace}})
		if got := root.shared.Session.ReplaceActive; got != tc.want {
			t.Errorf("after %s: replace the active set %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestBringUpWaitsForTheSettings refuses u while the settings have a
// problem, and says to fix them.
func TestBringUpWaitsForTheSettings(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.shared.Problems = []error{sandbox.ErrSettings}
	if root.Update(press("u")); root.shared.Running || !strings.Contains(root.shared.Toast.Text, "Fix the settings") {
		t.Errorf("u with a problem: running %v, notice %q; want a notice to fix the settings",
			root.shared.Running, root.shared.Toast.Text)
	}
}

// TestChecksThatDidNotRun says that the checks did not run when they stop
// before any check, and does not say the sandbox is up after a run of the
// checks alone. On a sandbox that is down, c does not run them.
func TestChecksThatDidNotRun(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	if root.Update(press("c")); root.shared.Running || !strings.Contains(root.shared.Toast.Text, "sandbox is down") {
		t.Errorf("c on a sandbox that is down: running %v, notice %q; want a notice that it is down",
			root.shared.Running, root.shared.Toast.Text)
	}
	last := len(root.shared.Steps) - 1
	root.shared.RunFrom = last
	root.Update(stepStartMsg{index: last})
	root.Update(stepDoneMsg{index: last, err: sandbox.ErrPod})
	root.Update(bringUpDoneMsg{})
	activity, _ := root.shared.Logs.Log(page.SourceActivity).Snapshot()
	text := strings.Join(activity, "\n")
	if !strings.Contains(text, "✕ The checks did not run") || strings.Contains(text, "The sandbox is up") {
		t.Errorf("activity = %q, want the checks not run, and nothing about the sandbox being up", activity)
	}
}

// TestBlockedLookupInTheFeed shows a refused lookup of a host on the
// internet in the feed of requests, where the summary names it blocked, and
// keeps the routine lookups to the feed with DNS.
func TestBlockedLookupInTheFeed(t *testing.T) {
	t.Parallel()
	lookup := func(host, verdict string) string {
		return `{"ts":"2026-10-11T03:19:20Z","scope":"dns","destination":{"host":"` + host + `."},` +
			`"query":{"type":"A","rcode":"REFUSED"},"decision":{"verdict":"` + verdict + `",` +
			`"reason":"no_matching_endpoint"}}`
	}
	feeds := [2]*component.Log{{}, {}}
	readRecords(strings.NewReader(strings.Join([]string{
		lookup(sampleDenied, verdictDeny),
		lookup(sampleDenied+".sandbox.svc.cluster.local", verdictDeny),
		lookup("pypi.org", verdictAllow),
	}, "\n")), feeds, &proxylog.Store{})
	requests, _ := feeds[0].Snapshot()
	everything, _ := feeds[1].Snapshot()
	if len(requests) != 1 || !strings.Contains(requests[0], sampleDenied) || !strings.Contains(requests[0], "deny") {
		t.Errorf("feed of requests = %q, want only the refused lookup of %s", requests, sampleDenied)
	}
	if len(everything) != len(feeds)+1 {
		t.Errorf("feed with DNS = %q, want all three lookups", everything)
	}
}

// busyKeys are the keys that change the sandbox, and so wait for the
// bring-up, the action or the setup that runs.
func busyKeys() []string { return []string{"u", "c", "R", "D", "n", "X", "x", "e", "p"} }

// TestOneJobAtATime refuses, with a notice, to start work that changes the
// sandbox while a bring-up, an action or the setup runs. A running
// bring-up still stops with u.
func TestOneJobAtATime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, notice string
		start        func(root *model)
		keys         []string
	}{
		{"an action", "pod is running", func(root *model) {
			root.Update(press("R"))
			root.Update(press("y"))
		}, busyKeys()},
		{"the setup", "Setup is open", func(root *model) { root.asking = true }, busyKeys()},
		{
			"a bring-up", "bring-up is running", func(root *model) { root.shared.Running = true },
			slices.DeleteFunc(busyKeys(), func(key string) bool { return key == "u" || key == "D" }),
		},
	} {
		for _, key := range tc.keys {
			root := quietModel(t)
			root.Update(press("2"))
			tc.start(root)
			running := root.shared.Running
			_, cmd := root.Update(press(key))
			if root.overlay.kind != overlayNone || root.screen != screenDashboard || root.shared.Running != running ||
				cmd != nil || !strings.Contains(root.shared.Toast.Text, tc.notice) {
				t.Errorf("%s while %s runs: overlay %v, screen %v, running %v, command %v, notice %q; want only "+
					"a notice that says %q", key, tc.name, root.overlay.kind, root.screen, root.shared.Running, cmd != nil,
					root.shared.Toast.Text, tc.notice)
			}
			root.stop()
		}
	}
}

// TestDownWaitsForTheBringUp stops a running bring-up first, from D in the
// quit dialog, and stops everything once it has.
func TestDownWaitsForTheBringUp(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.shared.Running = true
	root.Update(press("q"))
	root.Update(press("D"))
	if root.acting != "" {
		t.Errorf("%q runs while the bring-up runs, want it to wait", root.acting)
	}
	root.Update(bringUpDoneMsg{err: context.Canceled})
	if root.acting != "Stopping everything" {
		t.Errorf("after the bring-up stopped, %q runs, want everything to stop", root.acting)
	}
}

// TestNothingToStopOnAnExistingCluster says that D has nothing to stop on
// an existing cluster, and does not ask to delete a k3d cluster.
func TestNothingToStopOnAnExistingCluster(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.shared.Status = sandbox.Status{Deployed: true, ClusterExists: true}
	root.Update(press("D"))
	if root.overlay.kind != overlayNone || !strings.Contains(root.shared.Toast.Text, "nothing for D to stop") {
		t.Errorf("D: overlay %v, notice %q; want only a notice", root.overlay.kind, root.shared.Toast.Text)
	}
	if root.Update(press("q")); root.overlay.keepsRunning {
		t.Error("the quit dialog offers to stop the existing cluster")
	}
}

// TestQuitAfterAFailedStop stays when stopping everything before a quit
// fails, so that the failure shows.
func TestQuitAfterAFailedStop(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.quitAfterAction, root.acting = true, "Stopping everything"
	if _, cmd := root.Update(actionDoneMsg{what: root.acting, err: sandbox.ErrDocker}); cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Error("the TUI quit after stopping everything failed")
		}
	}
	if !strings.Contains(root.shared.Toast.Text, "Stopping everything failed") {
		t.Errorf("notice = %q, want the failure", root.shared.Toast.Text)
	}
}

// TestWizardWaitsForSetup leaves the dashboard as it is when the settings
// and the wizard are asked for while the setup for a new agent runs, and
// its question shows over the dashboard.
func TestWizardWaitsForSetup(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.asking, root.stopAsking, root.askTitle = true, make(chan struct{}), newAgentTitle
	for _, key := range []string{"e", "a"} {
		root.Update(press(key))
	}
	root.Update(page.AskMsg{Question: &page.Question{Kind: page.AskInput, Title: sampleAPITitle}})
	if view := ansi.Strip(root.View().Content); root.screen != screenDashboard ||
		!strings.Contains(view, newAgentTitle) {
		t.Errorf("e and a while a new agent's setup runs: screen %v, want its question over the dashboard:\n%s",
			root.screen, view)
	}
}

// TestQuitNamesTheAction says which action quitting stops.
func TestQuitNamesTheAction(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	for _, key := range []string{"R", "y", "q"} {
		root.Update(press(key))
	}
	if modal := ansi.Strip(root.quitModal()); !strings.Contains(modal, "pod is running") {
		t.Errorf("quit while an action runs:\n%s", modal)
	}
}

// TestPollsDoNotOverlap reads the pods and the services once at a time,
// and reads them again a while after the last read came back.
func TestPollsDoNotOverlap(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	_, first := root.Update(pollMsg{})
	_, second := root.Update(pollMsg{})
	if first == nil || second != nil {
		t.Fatalf("two ticks: first read %v, second read %v; want only the first", first != nil, second != nil)
	}
	root.Update(statusMsg{status: root.shared.Status})
	if _, next := root.Update(podsMsg{rows: root.shared.AllPods, agents: root.shared.Agents}); next == nil {
		t.Error("the last read came back, and no next read is planned")
	}
	// An action that ends reads the sandbox at once, and the poll planned
	// above stays the only one.
	if _, read := root.Update(actionDoneMsg{what: "Waiting"}); read == nil {
		t.Fatal("an action ended, and the sandbox was not read again")
	}
	root.Update(statusMsg{status: root.shared.Status})
	if _, next := root.Update(podsMsg{rows: root.shared.AllPods, agents: root.shared.Agents}); next != nil {
		t.Error("a second poll was planned while one waits")
	}
}

// TestShellExitStatus comes back from the shell without an error when sh
// exits with the status of the last command it ran.
func TestShellExitStatus(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	err := exec.CommandContext(t.Context(), "sh", "-c", "exit 7").Run()
	root.Update(execDoneMsg{plain: true, err: fmt.Errorf("sh in the agent: %w", err)})
	activity, _ := root.shared.Logs.Log(page.SourceActivity).Snapshot()
	if root.shared.Toast.Err || !strings.Contains(strings.Join(activity, "\n"), "exited with status 7") {
		t.Errorf("notice %q (error %v), activity %q; want the status noted, and no error",
			root.shared.Toast.Text, root.shared.Toast.Err, activity)
	}
}

// TestTrafficFollowerError shows in the feed why the proxy's log cannot be
// followed.
func TestTrafficFollowerError(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	feeds := [2]*component.Log{{}, {}}
	followTraffic(t.Context(), root.shared.Session, samplePod, feeds, &proxylog.Store{})
	if lines, _ := feeds[0].Snapshot(); len(lines) == 0 || !strings.Contains(lines[0], "cluster name") {
		t.Errorf("feed = %q, want why the proxy's log cannot be read", lines)
	}
}

// TestShutdownStopsTheWork cancels the work in the background, and waits
// for it to end.
func TestShutdownStopsTheWork(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	root := newModel(ctx, testSession(t))
	var ended atomic.Bool
	root.runAction("Waiting", func(ctx context.Context, _ io.Writer) error {
		<-ctx.Done()
		ended.Store(true)
		return ctx.Err()
	})
	root.shutdown(cancel)
	if !ended.Load() {
		t.Error("shutdown returned before the action ended")
	}
}
