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
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
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
	root.state.Reads.Done = true
	return root
}

func press(text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: []rune(text)[0], Text: text}
}

// addingAgent starts the questions for a new agent, as n does, and returns
// their asker.
func addingAgent(root *model) asker {
	root.state.Run.Job, root.askTitle = state.Job{Kind: state.JobSetup, Name: "Adding an agent"}, newAgentTitle
	return root.startQuestions(nil, nil)
}

// answerYes presses y in a confirmation, and hands the answer it sends to
// Update, as the program does.
func answerYes(root *model) {
	if _, cmd := root.Update(press("y")); cmd != nil {
		root.Update(cmd())
	}
}

// TestStepsFollowTheBringUp marks steps as the runner reports them, and
// starts again at the step that failed.
func TestStepsFollowTheBringUp(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	steps := root.state.Run.Steps
	if len(steps) != 10 || steps[0].Step.ID != sandbox.StepTunnel {
		t.Fatalf("steps = %d, first %q; want the 10 k3d steps from the tunnel", len(steps), steps[0].Step.ID)
	}
	root.Update(stepStartMsg{index: 0})
	root.Update(stepDoneMsg{index: 0})
	root.Update(stepStartMsg{index: 1})
	root.Update(stepDoneMsg{index: 1, err: sandbox.ErrDocker})
	if steps[0].State != state.StepDone || steps[1].State != state.StepFailed {
		t.Errorf("states = %v, %v; want done, failed", steps[0].State, steps[1].State)
	}
	if got := root.state.ResumeIndex(); got != 0 {
		t.Errorf("ResumeIndex with the tunnel stopped = %d, want 0", got)
	}
	root.state.Reads.Status.Tunnel.Running = true
	if got := root.state.ResumeIndex(); got != 1 {
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

// TestChecksAlone says that the checks run, and not a bring-up, when c
// runs them on a sandbox that is up: in the band, the terminal's progress,
// Home, the quit dialog and the question u asks. The model's context ends
// at once, so the checks' work stops and the test reads only the screens.
func TestChecksAlone(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := newModel(ctx, testSession(t))
	root.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	root.state.Reads.Status = sandbox.Status{Deployed: true, ClusterExists: true}
	root.state.Reads.Status.Tunnel.Running, root.state.Reads.Done = true, true
	root.Update(press("c"))
	if !root.state.Run.Job.Is(state.JobChecks) {
		t.Fatalf("job = %+v, want the checks", root.state.Run.Job)
	}
	badge := ansi.Strip(root.statusBadge())
	if !strings.Contains(badge, "running the checks") || strings.Contains(badge, "bringing up") {
		t.Errorf("badge = %q, want it to say the checks run", badge)
	}
	if bar := root.progress(); bar != nil {
		t.Errorf("progress = %+v, want none for the checks", bar)
	}
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "Running the checks") {
		t.Errorf("Home does not say the checks run:\n%s", view)
	}
	root.Update(press("q"))
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "The checks are running") {
		t.Errorf("the quit dialog does not say the checks run:\n%s", view)
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	root.Update(press("u"))
	if root.overlay.title != "Stop the checks?" {
		t.Errorf("u asks %q, want it to ask to stop the checks", root.overlay.title)
	}
}

// TestReadFromBeforeAStop drops the reads that started before D's stop
// ended, which still find the cluster D deleted, and reads again at once.
// Until a read made after the stop comes back, the screens say they read
// the sandbox.
func TestReadFromBeforeAStop(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	before := sandbox.Status{Deployed: true, ClusterExists: true}
	before.Tunnel.Running = true
	root.state.Reads.Set(before)
	mark := root.state.Reads.Mark()
	root.state.Run.StartJob(state.Job{Kind: state.JobStop, Name: "Stopping everything"}, nil)
	root.polling = pollReads
	root.Update(actionDoneMsg{})
	if phase, _ := root.state.Phase(); phase != state.PhaseReading {
		t.Errorf("phase after the stop = %v, want reading", phase)
	}
	root.Update(statusMsg{status: before, mark: mark})
	if _, cmd := root.Update(podsMsg{mark: mark}); cmd == nil || root.polling != pollReads {
		t.Errorf("polling = %d after the old reads; want a new poll at once", root.polling)
	}
	if root.state.Reads.Done || root.state.Reads.Status.ClusterExists {
		t.Errorf("status = %+v, read %v; want the old read dropped", root.state.Reads.Status, root.state.Reads.Done)
	}
	root.Update(statusMsg{mark: root.state.Reads.Mark()})
	if phase, _ := root.state.Phase(); phase != state.PhaseDown {
		t.Errorf("phase after a new read = %v, want down", phase)
	}
}

// TestStopEndsTheTraffic stops following the proxy's log once everything
// stopped, since the pod it follows is gone with the cluster.
func TestStopEndsTheTraffic(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	stopped := false
	root.trafficStop = func() { stopped = true }
	root.state.Agents.Traffic.Reset(samplePod)
	root.state.Run.StartJob(state.Job{Kind: state.JobStop}, nil)
	root.Update(actionDoneMsg{})
	if _, pod := root.state.Agents.Traffic.Snapshot(); !stopped || root.trafficStop != nil || pod != "" {
		t.Errorf("after the stop: follower stopped %v, store of %q; want it stopped and the store empty", stopped, pod)
	}
}

// TestQuitDialogFollowsTheJob draws the quit dialog from what runs now: a
// bring-up that ends while it is open drops out of it.
func TestQuitDialogFollowsTheJob(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.state.Run.Job = state.Job{Kind: state.JobBringUp}
	root.Update(press("q"))
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "A bring-up is running") {
		t.Fatalf("the quit dialog does not say the bring-up runs:\n%s", view)
	}
	root.state.EndJob()
	if view := ansi.Strip(root.View().Content); strings.Contains(view, "A bring-up is running") ||
		!strings.Contains(view, "The sandbox keeps running") {
		t.Errorf("the quit dialog after the bring-up ended; want it to say only what keeps running:\n%s", view)
	}
}

// TestStartsWithoutTellingToBringUp leaves what to do to Home on a sandbox
// without a tunnel step, which may be up already: the first read says.
func TestStartsWithoutTellingToBringUp(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.Init()
	if root.ui.Toast.Text != "" || root.state.Run.Job.Busy() {
		t.Errorf("Init: notice %q, job %+v; want neither", root.ui.Toast.Text, root.state.Run.Job)
	}
}

// TestResumeAtStoppedMirror starts again at the mirror when a later step
// failed and the mirror no longer runs, because the step that failed needs
// it.
func TestResumeAtStoppedMirror(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.state.Reads.Status.Tunnel.Running = true
	for i := range 3 {
		root.Update(stepStartMsg{index: i})
		root.Update(stepDoneMsg{index: i})
	}
	root.Update(stepStartMsg{index: 3})
	root.Update(stepDoneMsg{index: 3, err: sandbox.ErrService})
	if got := root.state.ResumeIndex(); got != 2 {
		t.Errorf("ResumeIndex with the mirror stopped = %d, want 2", got)
	}
	root.state.Reads.Status.Mirror.Running = true
	if got := root.state.ResumeIndex(); got != 3 {
		t.Errorf("ResumeIndex = %d, want 3", got)
	}
}

// TestRecreateAsks offers to recreate a cluster made for another issuer
// URL, or one the kubeconfig has no context for, and says which. Recreating
// deletes the cluster, so only y says yes: enter says no.
func TestRecreateAsks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, why string
		err       error
	}{
		{"other issuer", "another issuer URL", errors.Join(sandbox.ErrIssuer, sandbox.ErrOtherIssuer)},
		{"no kubeconfig context", "no context for the cluster", sandbox.ErrBrokenCluster},
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
	if _, ok := cmd().(tea.QuitMsg); !ok || root.state.Run.Job.Busy() {
		t.Errorf("Enter: quit %v, job %+v; want a plain quit", ok, root.state.Run.Job)
	}
}

// TestTabsAndLogs switches tabs with the number keys, shows a log from the
// list of every log over the screen, and goes back to the tab's own log on
// esc.
func TestTabsAndLogs(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	for _, tc := range []struct {
		key    tea.KeyPressMsg
		tab    tab
		shown  overlayKind
		source string
	}{
		{press("3"), tabTraffic, overlayNone, state.SourceTraffic},
		{press("d"), tabTraffic, overlayNone, state.SourceTrafficAll},
		{press("t"), tabTraffic, overlayLog, state.SourceActivity},
		{tea.KeyPressMsg{Code: tea.KeyEscape}, tabTraffic, overlayNone, state.SourceTrafficAll},
		{press("l"), tabTraffic, overlayLogs, state.SourceTrafficAll},
		{tea.KeyPressMsg{Code: tea.KeyEnter}, tabTraffic, overlayLog, state.ContainerSource("", state.AgentContainer)},
		{tea.KeyPressMsg{Code: tea.KeyEscape}, tabTraffic, overlayNone, state.SourceTrafficAll},
		{press("1"), tabHome, overlayNone, state.SourceTrafficAll},
	} {
		root.Update(tc.key)
		if source := root.ui.Logs.Source(); root.tab != tc.tab || root.overlay.kind != tc.shown || source != tc.source {
			t.Errorf("%s: tab %v, overlay %v, source %q; want %v, %v, %q", tc.key, root.tab, root.overlay.kind, source,
				tc.tab, tc.shown, tc.source)
		}
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

// TestChecksScore counts the passed checks from the checks step's log, and
// Home says they need a look.
func TestChecksScore(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	last := len(root.state.Run.Steps) - 1
	row := root.state.Run.Steps[last]
	row.Log.Add("  PASS  allowed", "  FAIL  credentialed", "  PASS  denied", "  MANUAL  event log")
	root.Update(stepStartMsg{index: last})
	root.Update(stepDoneMsg{index: last, err: sandbox.ErrCheck})
	if view := ansi.Strip(root.View().Content); row.Passed != 2 || row.Total != 3 ||
		!strings.Contains(view, "NEEDS A LOOK") || !strings.Contains(view, "Checks: 2 of 3 passed.") {
		t.Errorf("checks = %d/%d, want Home to say 2 of 3 passed:\n%s", row.Passed, row.Total, view)
	}
}

// TestFirstRunWelcome greets a checkout with no settings: the mark, what
// the sandbox does, and the machine checks as they run.
func TestFirstRunWelcome(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.screen = screenWelcome
	root.preflight([]sandbox.PreflightItem{{Name: sampleCheck}, {Name: sampleTools}})
	root.Update(
		checkDoneMsg{
			run:   root.state.Setup.Preflight.Run,
			index: 0,
			check: sandbox.Check{Name: sampleCheck, Detail: sampleDocker},
		},
	)
	mark := style.Logo(root.ui.Styles, root.ui.Background, style.Glint{})
	view := ansi.Strip(root.View().Content)
	for _, want := range []string{
		"Run an AI agent that never holds a secret.", "egress proxy", "Keycard zone", "This machine",
		sampleDocker, "checking…", "start setup", ansi.Strip(strings.SplitN(mark, "\n", 2)[0]),
	} {
		if !strings.Contains(view, want) {
			t.Errorf("welcome lacks %q:\n%s", want, view)
		}
	}
	activity, _ := root.ui.Logs.Log(state.SourceActivity).Snapshot()
	if len(activity) != 1 || !strings.Contains(activity[0], "✓ Docker: OrbStack") {
		t.Errorf("activity = %q, want the Docker check", activity)
	}
}

// TestSetupDone goes back to the screen the wizard started from when the
// setup stops before it saves, without the answer it did not keep, and says
// there why a setup failed. When saving the settings failed, it shows the
// review, and the activity says why, whatever the result says.
func TestSetupDone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, answer, says, activity string
		back, want                   screen
		msg                          setupDoneMsg
	}{
		{name: "stopped", back: screenWelcome, want: screenWelcome, msg: setupDoneMsg{err: sandbox.ErrInterrupted}},
		{
			name: "refused", back: screenWelcome, want: screenWelcome, says: "✕ Setup stopped: settings",
			msg: setupDoneMsg{err: fmt.Errorf("%w: signing in: refused", sandbox.ErrSettings)},
		},
		{
			name: "stopped from the review", back: screenReview, want: screenReview,
			answer: "Your existing cluster prod-ctx", msg: setupDoneMsg{err: sandbox.ErrInterrupted},
		},
		{
			name: "not saved", back: screenWelcome, want: screenReview,
			activity: "✕ the settings are not saved: the keychain is locked", msg: setupDoneMsg{
				result: sandbox.InitResult{Saved: true}, err: fmt.Errorf("%w: the keychain is locked", sandbox.ErrNotSaved),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := testModel(t)
			root.screen, root.wizardBack = screenWizard, tc.back
			if tc.answer != "" {
				root.setup.Begin(sandbox.SectionCluster)
				root.setup.OnSection(page.SectionMsg{ID: sandbox.SectionCluster})
				cluster := slices.IndexFunc(root.setup.Rail, func(step *page.RailStep) bool {
					return step.ID == sandbox.SectionCluster
				})
				root.setup.Rail[cluster].Answers = []string{tc.answer}
			}
			root.Update(tc.msg)
			view := ansi.Strip(root.View().Content)
			if root.screen != tc.want || strings.Contains(view, "✕ Setup stopped") != (tc.says != "") ||
				!strings.Contains(view, tc.says) || strings.Contains(view, "prod-ctx") ||
				strings.Contains(view, "Your existing cluster") {
				t.Errorf("screen %v; want %v, with %q, and without the answer the setup did not keep:\n%s",
					root.screen, tc.want, tc.says, view)
			}
			activity, _ := root.ui.Logs.Log(state.SourceActivity).Snapshot()
			if !strings.HasSuffix(strings.Join(activity, "\n"), tc.activity) {
				t.Errorf("activity %q, want its last line to end with %q", activity, tc.activity)
			}
		})
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
	pump(t, root, func() bool { return !root.state.Run.Job.Is(state.JobSetup) })
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
	root.screen, root.state.Run.Job = screenWizard, state.Job{Kind: state.JobSetup}
	asker := root.startQuestions(nil, nil)
	defer root.stopSetup()
	root.Update(press("q"))
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "Setup stops, and keeps none") {
		t.Errorf("the quit dialog does not say the setup stops:\n%s", view)
	}
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
	asker := addingAgent(root)
	defer root.stopSetup()
	root.Update(press("l"))
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
	addingAgent(root)
	if root.Update(press("u")); !strings.Contains(root.ui.Toast.Text, "Adding an agent") {
		t.Errorf("u while adding an agent: notice %q", root.ui.Toast.Text)
	}
	if root.Update(tea.KeyPressMsg{Code: tea.KeyEscape}); root.asking() {
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
	answerYes(root)
	root.Update(podsMsg{rows: root.state.Agents.Pods, agents: root.state.Agents.List})
	if source := root.ui.Logs.Source(); source != state.SourceAction {
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
		root.state.Reads.Status.Tunnel.Running = sandboxUp
		agents := append(slices.Clone(root.state.Agents.List), sandbox.Agent{Email: newcomer})
		root.Update(press("2"))
		root.Update(agentAddedMsg{email: newcomer, agents: agents})
		view := ansi.Strip(root.View().Content)
		switch {
		case sandboxUp && (root.state.Run.Job.Agent != newcomer || !strings.Contains(view, "coming up")):
			t.Errorf("sandbox up: job %+v; want the agent coming up:\n%s", root.state.Run.Job, view)
		case !sandboxUp && (root.state.Run.Job.Busy() || !strings.Contains(root.ui.Toast.Text, "comes up with the sandbox")):
			t.Errorf("sandbox down: job %+v, notice %q; want only a notice", root.state.Run.Job, root.ui.Toast.Text)
		}
		root.state.Run.CancelJob()
	}
}

// TestAgentKeptWhenSavingFails selects an agent that was added when saving
// the settings failed, and says it is for this session only.
func TestAgentKeptWhenSavingFails(t *testing.T) {
	t.Parallel()
	const newcomer = "grace@example.com"
	root := upModel(t)
	root.state.Reads.Status.Tunnel.Running = false
	agents := append(slices.Clone(root.state.Agents.List), sandbox.Agent{Email: newcomer})
	root.Update(agentAddedMsg{email: newcomer, agents: agents, err: fmt.Errorf("%w: disk full", sandbox.ErrNotSaved)})
	activity, _ := root.ui.Logs.Log(state.SourceActivity).Snapshot()
	if selected := root.session.Selected(); selected != newcomer ||
		!strings.Contains(strings.Join(activity, "\n"), newcomer+" has an agent for this session only") {
		t.Errorf("selected %q, activity %q; want %s selected, for this session only", selected, activity, newcomer)
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
		if got := root.session.ReplaceActive; got != tc.want {
			t.Errorf("after %s: replace the active set %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestBringUpWaitsForTheSettings refuses u while the settings have a
// problem, and says to fix them.
func TestBringUpWaitsForTheSettings(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.state.Setup.Problems = []error{sandbox.ErrSettings}
	if root.Update(press("u")); root.state.Run.Job.Busy() || !strings.Contains(root.ui.Toast.Text, "Fix the settings") {
		t.Errorf("u with a problem: running %v, notice %q; want a notice to fix the settings",
			root.state.Run.Job.Busy(), root.ui.Toast.Text)
	}
}

// TestChecksThatDidNotRun says that the checks did not run when they stop
// before any check, and does not say the sandbox is up after a run of the
// checks alone. On a sandbox that is down, c does not run them.
func TestChecksThatDidNotRun(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	if root.Update(press("c")); root.state.Run.Job.Busy() || !strings.Contains(root.ui.Toast.Text, "sandbox is down") {
		t.Errorf("c on a sandbox that is down: job %+v, notice %q; want a notice that it is down",
			root.state.Run.Job, root.ui.Toast.Text)
	}
	last := len(root.state.Run.Steps) - 1
	root.state.Run.Job = state.Job{Kind: state.JobChecks, From: last}
	root.Update(stepStartMsg{index: last})
	root.Update(stepDoneMsg{index: last, err: sandbox.ErrPod})
	root.Update(bringUpDoneMsg{})
	activity, _ := root.ui.Logs.Log(state.SourceActivity).Snapshot()
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
	err := readRecords(strings.NewReader(strings.Join([]string{
		lookup(sampleDenied, verdictDeny),
		lookup(sampleDenied+".sandbox.svc.cluster.local", verdictDeny),
		lookup("pypi.org", verdictAllow),
	}, "\n")), "", feeds, &proxylog.Store{})
	if err != nil {
		t.Fatal(err)
	}
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
			answerYes(root)
		}, busyKeys()},
		{
			"the setup", "Setup is open", func(root *model) { root.state.Run.Job = state.Job{Kind: state.JobSetup} },
			busyKeys(),
		},
		{
			"a bring-up", "bring-up is running", func(root *model) { root.state.Run.Job = state.Job{Kind: state.JobBringUp} },
			slices.DeleteFunc(busyKeys(), func(key string) bool { return key == "u" || key == "D" }),
		},
	} {
		for _, key := range tc.keys {
			root := quietModel(t)
			root.Update(press("2"))
			tc.start(root)
			running := root.state.Run.Job
			_, cmd := root.Update(press(key))
			if root.overlay.kind != overlayNone || root.screen != screenDashboard ||
				root.state.Run.Job.Kind != running.Kind || cmd != nil || !strings.Contains(root.ui.Toast.Text, tc.notice) {
				t.Errorf("%s while %s runs: overlay %v, screen %v, job %+v, command %v, notice %q; want only "+
					"a notice that says %q", key, tc.name, root.overlay.kind, root.screen, root.state.Run.Job, cmd != nil,
					root.ui.Toast.Text, tc.notice)
			}
			root.state.Run.CancelJob()
		}
	}
}

// localQuietModel is a sandbox that is up, on a local k3d cluster that
// does not exist, where every tool fails: a stop it starts deletes nothing.
func localQuietModel(t *testing.T) *model {
	t.Helper()
	root := quietModel(t)
	settings := root.session.Local().Settings
	delete(settings, sandbox.KeyKubeContext)
	settings[sandbox.KeyCluster] = "keycard-test-none"
	root.session.Local().Tool = func(ctx context.Context, _ string, _ ...string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "false"), nil
	}
	root.state.ResetSteps()
	return root
}

// TestDownWaitsForTheBringUp asks before D in the quit dialog stops
// everything, stops a running bring-up first, and stops everything once it
// has.
func TestDownWaitsForTheBringUp(t *testing.T) {
	t.Parallel()
	root := localQuietModel(t)
	root.state.Run.Job = state.Job{Kind: state.JobBringUp}
	root.Update(press("q"))
	root.Update(press("D"))
	if job := root.state.Run.Job; job.StopAfter || root.overlay.title != "Stop everything and quit?" {
		t.Errorf("D asked %q and set job = %+v, want it to ask before it stops anything", root.overlay.title, job)
	}
	answerYes(root)
	if job := root.state.Run.Job; !job.Is(state.JobBringUp) || !job.StopAfter || !job.QuitAfter {
		t.Errorf("job = %+v while the bring-up runs, want the bring-up, then a stop and a quit", job)
	}
	root.Update(bringUpDoneMsg{err: context.Canceled})
	if job := root.state.Run.Job; !job.Is(state.JobStop) || !job.QuitAfter {
		t.Errorf("after the bring-up stopped, job = %+v, want everything to stop, then a quit", job)
	}
}

// TestStopWhileAToolRuns reports a bring-up stopped while a tool runs as
// stopped, though the tool that the stop killed fails with its own error.
func TestStopWhileAToolRuns(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.session.Local().Tool = func(ctx context.Context, _ string, _ ...string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sleep", "30"), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	run := bringUp{session: root.session, out: newBus(t.Context()), steps: root.state.Run.Steps}
	stop := time.AfterFunc(200*time.Millisecond, cancel)
	defer stop.Stop()
	if err := runSteps(ctx, run, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("the bring-up ended with %v, want context.Canceled", err)
	}
	for range 2 {
		if msg, ok := (<-run.out.ch).(stepDoneMsg); ok && !errors.Is(msg.err, context.Canceled) {
			t.Errorf("the step ended with %v, want context.Canceled", msg.err)
		}
	}
}

// TestStoppedBringUp says where the operator stopped the bring-up, apart
// from a failure: no error, no ✕, and no failed progress.
func TestStoppedBringUp(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.state.Reads.Status.Tunnel.Running = true
	root.Update(stepDoneMsg{index: 1, err: context.Canceled})
	root.Update(bringUpDoneMsg{err: context.Canceled})
	view := ansi.Strip(root.View().Content)
	for _, want := range []string{
		"○ You stopped the bring-up at: Create or check the cluster", "○ stopped at Create or check the cluster",
		"Start again from: Create or check the cluster",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("Home lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "✕") || strings.Contains(view, "context canceled") {
		t.Errorf("Home shows the stop as a failure:\n%s", view)
	}
	activity, _ := root.ui.Logs.Log(state.SourceActivity).Snapshot()
	stopped := slices.ContainsFunc(activity, func(line string) bool {
		return strings.HasSuffix(line, "• Stopped the bring-up at: Create or check the cluster")
	})
	if !stopped || slices.ContainsFunc(activity, func(line string) bool { return strings.Contains(line, "✕") }) {
		t.Errorf("activity = %q, want only the stop", activity)
	}
	if progress := root.progress(); progress != nil {
		t.Errorf("progress = %+v, want none once the bring-up stopped", progress)
	}
}

// TestDownForgetsTheFailedStep says that everything stops while it does,
// and then forgets the bring-up that failed, so Home no longer says where
// it stopped.
func TestDownForgetsTheFailedStep(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.Update(stepDoneMsg{index: 0, err: sandbox.ErrDocker})
	root.stopEverything(false)
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "Stopping everything…") ||
		!strings.Contains(view, "stopping") {
		t.Errorf("while it stops, Home and the band do not say so:\n%s", view)
	}
	root.Update(<-root.bus.ch)
	for _, row := range root.state.Run.Steps {
		if row.State != state.StepPending {
			t.Errorf("after the stop, %q is %v, want it pending", row.Step.Title, row.State)
		}
	}
}

// TestRecreateWaitsForTheQuitDialog keeps the quit dialog open when the
// bring-up asks to recreate the cluster, and asks once the dialog closes.
func TestRecreateWaitsForTheQuitDialog(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.Update(press("q"))
	root.Update(stepDoneMsg{index: 0, err: sandbox.ErrOtherIssuer})
	if root.overlay.kind != overlayQuit {
		t.Fatalf("overlay = %v while the quit dialog is open, want the quit dialog", root.overlay.kind)
	}
	root.Update(press("esc"))
	if root.overlay.kind != overlayConfirm || root.overlay.title != "Recreate the cluster?" {
		t.Errorf("after esc: overlay %v %q, want the question to recreate the cluster", root.overlay.kind,
			root.overlay.title)
	}
}

// TestPollSkipsPodsWhenDown does not run kubectl for the pods while the
// last read found no cluster or no zone step.
func TestPollSkipsPodsWhenDown(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.state.Reads.Done, root.state.Reads.Status = true, sandbox.Status{ClusterExists: true}
	root.session.Local().Tool = func(ctx context.Context, name string, _ ...string) (*exec.Cmd, error) {
		t.Errorf("the poll ran %s while the sandbox is down", name)
		return exec.CommandContext(ctx, "false"), nil
	}
	batch, ok := root.poll()().(tea.BatchMsg)
	if !ok || len(batch) != pollReads {
		t.Fatalf("poll = %v, want %d reads", batch, pollReads)
	}
	if msg, ok := batch[1]().(podsMsg); !ok || msg.rows != nil || msg.err != nil {
		t.Errorf("pods read = %+v, want no pods and no error", msg)
	}
}

// TestTrafficSaysWhyItCannotRead keeps why the proxy's log cannot be
// read, and Home says it instead of showing no traffic.
func TestTrafficSaysWhyItCannotRead(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	store := &proxylog.Store{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go followTraffic(ctx, root.session, samplePod, [2]*component.Log{{}, {}}, store)
	for deadline := time.Now().Add(5 * time.Second); store.Err() == nil && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if store.Err() == nil {
		t.Fatal("no error from following a pod with no zone step, want one")
	}
	root.state.Agents.Traffic.SetErr(store.Err())
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "Cannot read the proxy's log") ||
		strings.Contains(view, "not used yet") {
		t.Errorf("Home says nothing of the error:\n%s", view)
	}
}

// TestAgentKeysWhileDown says the sandbox is down for the keys that work
// with the agent, instead of trying them.
func TestAgentKeysWhileDown(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"x", "R", "c"} {
		root := quietModel(t)
		root.state.Reads.Done, root.state.Reads.Status = true, sandbox.Status{}
		_, cmd := root.Update(press(key))
		if cmd != nil || root.overlay.kind != overlayNone || root.state.Run.Job.Busy() ||
			!strings.Contains(root.ui.Toast.Text, "The sandbox is down") {
			t.Errorf("%s while down: command %v, overlay %v, notice %q; want only a notice that it is down",
				key, cmd != nil, root.overlay.kind, root.ui.Toast.Text)
		}
	}
}

// TestBusEndsWithTheTUI stops waiting on the bus, and drops what comes,
// once the TUI's context ends.
func TestBusEndsWithTheTUI(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	queue := newBus(ctx)
	cancel()
	for range busSize + 1 {
		queue.Send(pollMsg{})
	}
	done := make(chan struct{})
	go func() {
		for range busSize + 1 {
			queue.next()()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the bus still waits after the TUI stopped")
	}
}

// TestNothingToStopOnAnExistingCluster says that D has nothing to stop on
// an existing cluster, and does not ask to delete a k3d cluster.
func TestNothingToStopOnAnExistingCluster(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.state.Reads.Status = sandbox.Status{Deployed: true, ClusterExists: true}
	root.Update(press("D"))
	if root.overlay.kind != overlayNone || !strings.Contains(root.ui.Toast.Text, "nothing for D to stop") {
		t.Errorf("D: overlay %v, notice %q; want only a notice", root.overlay.kind, root.ui.Toast.Text)
	}
	if root.Update(press("q")); root.anythingRunning() {
		t.Error("the quit dialog offers to stop the existing cluster")
	}
	// While a bring-up runs, quitting stops it, and D still has nothing to
	// stop.
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	root.state.Run.Job = state.Job{Kind: state.JobBringUp}
	root.Update(press("q"))
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "A bring-up is running") ||
		strings.Contains(view, "stop everything") {
		t.Errorf("the quit dialog during a bring-up; want it to say the bring-up stops, without D:\n%s", view)
	}
	if root.Update(press("D")); root.overlay.kind != overlayNone {
		t.Errorf("D in the quit dialog opened %v, want only a notice", root.overlay.kind)
	}
}

// TestQuitAfterAFailedStop stays when stopping everything before a quit
// fails, so that the failure shows.
func TestQuitAfterAFailedStop(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.state.Run.Job = state.Job{Kind: state.JobStop, Name: "Stopping everything", QuitAfter: true}
	if _, cmd := root.Update(actionDoneMsg{err: sandbox.ErrDocker}); cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Error("the TUI quit after stopping everything failed")
		}
	}
	if !strings.Contains(root.ui.Toast.Text, "Stopping everything failed") {
		t.Errorf("notice = %q, want the failure", root.ui.Toast.Text)
	}
}

// TestWizardWaitsForSetup leaves the dashboard as it is when the settings
// and the wizard are asked for while the setup for a new agent runs, and
// its question shows over the dashboard.
func TestWizardWaitsForSetup(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	addingAgent(root)
	for _, key := range []string{"e", "a"} {
		root.Update(press(key))
	}
	root.Update(
		askMsg{question: &page.Question{Kind: page.AskInput, Title: sampleAPITitle}, replies: make(chan reply, 1)},
	)
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
	root.Update(press("R"))
	answerYes(root)
	root.Update(press("q"))
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
	root.Update(statusMsg{status: root.state.Reads.Status})
	if _, next := root.Update(podsMsg{rows: root.state.Agents.Pods, agents: root.state.Agents.List}); next == nil {
		t.Error("the last read came back, and no next read is planned")
	}
	// An action that ends reads the sandbox at once, and the poll planned
	// above stays the only one.
	root.state.Run.Job = state.Job{Kind: state.JobAction, Name: "Waiting"}
	if _, read := root.Update(actionDoneMsg{}); read == nil {
		t.Fatal("an action ended, and the sandbox was not read again")
	}
	mark := root.state.Reads.Mark()
	root.Update(statusMsg{status: root.state.Reads.Status, mark: mark})
	if _, next := root.Update(
		podsMsg{rows: root.state.Agents.Pods, agents: root.state.Agents.List, mark: mark},
	); next != nil {
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
	activity, _ := root.ui.Logs.Log(state.SourceActivity).Snapshot()
	if root.ui.Toast.Err || !strings.Contains(strings.Join(activity, "\n"), "exited with status 7") {
		t.Errorf("notice %q (error %v), activity %q; want the status noted, and no error",
			root.ui.Toast.Text, root.ui.Toast.Err, activity)
	}
}

// TestTrafficFollowerError shows in the feed why the proxy's log cannot be
// followed, and keeps trying.
func TestTrafficFollowerError(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	feeds := [2]*component.Log{{}, {}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		followTraffic(ctx, root.session, samplePod, feeds, &proxylog.Store{})
		close(done)
	}()
	var lines []string
	for deadline := time.Now().Add(5 * time.Second); len(lines) == 0 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		lines, _ = feeds[0].Snapshot()
	}
	cancel()
	<-done
	if len(lines) == 0 || !strings.Contains(lines[0], "cluster name") {
		t.Errorf("feed = %q, want why the proxy's log cannot be read", lines)
	}
}

// TestAttemptLogKeepsLines keeps the lines of the run of kubectl logs
// before until the next run writes, so the pane does not empty while
// kubectl starts again.
func TestAttemptLogKeepsLines(t *testing.T) {
	t.Parallel()
	log := &component.Log{}
	log.Add("from the run before")
	next := &attemptLog{log: log, again: true}
	if lines, _ := log.Snapshot(); len(lines) != 1 {
		t.Fatalf("lines = %q before the next run wrote, want the earlier run's", lines)
	}
	if _, err := next.Write([]byte("Error from server: container is waiting to start\n")); err != nil {
		t.Fatal(err)
	}
	next.Add("a line")
	if lines, _ := log.Snapshot(); !slices.Equal(lines, []string{
		"Error from server: container is waiting to start", "a line",
	}) {
		t.Errorf("lines = %q, want only the next run's", lines)
	}
}

// TestAgentsResetKeepsTheTabsLayout leaves the log pane as the tab that
// shows laid it out, when the Agents tab forgets its request.
func TestAgentsResetKeepsTheTabsLayout(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	root.Update(press("3"))
	width, height := root.ui.Logs.Size()
	root.agents.Reset()
	if gotWidth, gotHeight := root.ui.Logs.Size(); gotWidth != width || gotHeight != height {
		t.Errorf("after Reset on the Traffic tab, the log pane is %dx%d, want %dx%d", gotWidth, gotHeight, width,
			height)
	}
}

// TestShutdownStopsTheWork cancels the work in the background, and waits
// for it to end.
func TestShutdownStopsTheWork(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	root := newModel(ctx, testSession(t))
	var ended atomic.Bool
	root.runAction(state.Job{Kind: state.JobAction, Name: "Waiting"}, func(ctx context.Context, _ io.Writer) error {
		<-ctx.Done()
		ended.Store(true)
		return ctx.Err()
	})
	root.shutdown(cancel)
	if !ended.Load() {
		t.Error("shutdown returned before the action ended")
	}
}

// TestRecreateQuestionGoesWithD drops the question to recreate the cluster
// that came while the D dialog was open, once D is confirmed. An answer that
// comes after everything stopped does not recreate the cluster.
func TestRecreateQuestionGoesWithD(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	cluster := slices.IndexFunc(root.state.Run.Steps, func(row *state.StepRow) bool {
		return row.Step.ID == sandbox.StepCluster
	})
	root.state.Run.Job = state.Job{Kind: state.JobBringUp}
	root.Update(press("D"))
	root.Update(stepDoneMsg{index: cluster, err: sandbox.ErrOtherIssuer})
	answerYes(root)
	if root.overlay.kind != overlayNone || root.waiting != nil || !root.state.Run.Job.StopAfter {
		t.Errorf("after D: overlay %v, waiting %v, job %+v; want the question gone and everything to stop",
			root.overlay.kind, root.waiting != nil, root.state.Run.Job)
	}
	root.state.Run.Job = state.Job{}
	root.state.ResetSteps()
	root.Update(recreateClusterMsg{from: cluster})
	if root.state.Run.Job.Busy() || !strings.Contains(root.ui.Toast.Text, "does not need to be recreated") {
		t.Errorf("a late answer: job %+v, notice %q; want no bring-up", root.state.Run.Job, root.ui.Toast.Text)
	}
}

// TestStopEverythingAfterTheSetup stops the setup first, from D in the
// quit dialog once answered yes, and stops everything once the setup
// reports back.
func TestStopEverythingAfterTheSetup(t *testing.T) {
	t.Parallel()
	root := localQuietModel(t)
	root.screen, root.state.Run.Job = screenWizard, state.Job{Kind: state.JobSetup}
	root.startQuestions(nil, nil)
	root.Update(press("q"))
	root.Update(press("D"))
	answerYes(root)
	if job := root.state.Run.Job; root.asking() || !job.Is(state.JobSetup) || !job.StopAfter || !job.QuitAfter {
		t.Errorf("after D: setup asks %v, job %+v; want the setup stopped, then a stop and a quit",
			root.asking(), job)
	}
	root.Update(setupDoneMsg{err: sandbox.ErrInterrupted})
	if job := root.state.Run.Job; !job.Is(state.JobStop) || !job.QuitAfter {
		t.Errorf("once the setup ended, job = %+v, want everything to stop, then a quit", job)
	}
}
