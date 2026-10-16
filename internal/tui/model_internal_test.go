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
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
)

// Sample data the tests share.
const (
	sampleMethod  = "GET"
	sampleDenied  = "example.com"
	samplePod     = "u7k2vjw9ab"
	verdictAllow  = "allow"
	verdictDeny   = "deny"
	sampleAllowed = "endpoint_allowed"
	sampleReason  = "no_matching_endpoint"
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
	root.state.Read = true
	return root
}

func press(text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: []rune(text)[0], Text: text}
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
	steps := root.state.Steps
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
	root.state.Status.Tunnel.Running = true
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
	if _, ok := cmd().(tea.QuitMsg); !ok || root.state.Job.Busy() {
		t.Errorf("Enter: quit %v, job %+v; want a plain quit", ok, root.state.Job)
	}
}

// TestViewSizes draws Home at a laptop size and a small size, and says
// when the window is too small.
func TestViewSizes(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	for _, size := range []tea.WindowSizeMsg{{Width: 140, Height: 40}, {Width: 80, Height: 22}} {
		root.Update(size)
		content := ansi.Strip(root.View().Content)
		if lines := strings.Split(content, "\n"); len(lines) > size.Height {
			t.Errorf("%dx%d: %d lines, want at most %d", size.Width, size.Height, len(lines), size.Height)
		}
		for line := range strings.SplitSeq(content, "\n") {
			if ansi.StringWidth(line) > size.Width {
				t.Errorf("%dx%d: line wider than the window: %q", size.Width, size.Height, line)
				break
			}
		}
	}
	root.Update(tea.WindowSizeMsg{Width: 60, Height: 15})
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "too small") {
		t.Errorf("60x15 view = %q, want it to say the window is too small", view)
	}
}

// TestChecksScore counts the passed checks from the checks step's log.
func TestChecksScore(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	last := len(root.state.Steps) - 1
	row := root.state.Steps[last]
	row.Log.Add("  PASS  allowed", "  FAIL  credentialed", "  PASS  denied", "  MANUAL  event log")
	root.Update(stepStartMsg{index: last})
	root.Update(stepDoneMsg{index: last, err: sandbox.ErrCheck})
	if row.Passed != 2 || row.Total != 3 {
		t.Errorf("checks = %d/%d, want 2 of 3 passed", row.Passed, row.Total)
	}
}

// TestChecksThatDidNotRun says that the checks did not run when they stop
// before any check, and does not say the sandbox is up after a run of the
// checks alone. On a sandbox that is down, c does not run them.
func TestChecksThatDidNotRun(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	if root.Update(press("c")); root.state.Job.Busy() || !strings.Contains(root.ui.Toast.Text, "sandbox is down") {
		t.Errorf("c on a sandbox that is down: job %+v, notice %q; want a notice that it is down",
			root.state.Job, root.ui.Toast.Text)
	}
	last := len(root.state.Steps) - 1
	root.state.Job = state.Job{Kind: state.JobBringUp, From: last}
	root.Update(stepStartMsg{index: last})
	root.Update(stepDoneMsg{index: last, err: sandbox.ErrPod})
	root.Update(bringUpDoneMsg{})
	activity, _ := root.ui.Logs.Log(state.SourceActivity).Snapshot()
	text := strings.Join(activity, "\n")
	if !strings.Contains(text, "✕ The checks did not run") || strings.Contains(text, "The sandbox is up") {
		t.Errorf("activity = %q, want the checks not run, and nothing about the sandbox being up", activity)
	}
}

// busyKeys are the keys that change the sandbox, and so wait for the
// bring-up, the action or the setup that runs.
func busyKeys() []string { return []string{"u", "c", "R", "D", "x"} }

// TestOneJobAtATime refuses, with a notice, to start work that changes the
// sandbox while a bring-up or an action runs. A running
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
			"a bring-up", "bring-up is running", func(root *model) { root.state.Job = state.Job{Kind: state.JobBringUp} },
			slices.DeleteFunc(busyKeys(), func(key string) bool { return key == "u" || key == "D" }),
		},
	} {
		for _, key := range tc.keys {
			root := quietModel(t)
			tc.start(root)
			running := root.state.Job
			_, cmd := root.Update(press(key))
			if root.overlay.kind != overlayNone || root.state.Job.Kind != running.Kind || cmd != nil ||
				!strings.Contains(root.ui.Toast.Text, tc.notice) {
				t.Errorf("%s while %s runs: overlay %v, job %+v, command %v, notice %q; want only a notice "+
					"that says %q", key, tc.name, root.overlay.kind, root.state.Job, cmd != nil,
					root.ui.Toast.Text, tc.notice)
			}
			root.state.CancelJob()
		}
	}
}

// TestDownWaitsForTheBringUp asks before D in the quit dialog stops
// everything, stops a running bring-up first, and stops everything once it
// has.
func TestDownWaitsForTheBringUp(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.state.Job = state.Job{Kind: state.JobBringUp}
	root.Update(press("q"))
	root.Update(press("D"))
	if job := root.state.Job; job.StopAfter || root.overlay.title != "Stop everything and quit?" {
		t.Errorf("D asked %q and set job = %+v, want it to ask before it stops anything", root.overlay.title, job)
	}
	answerYes(root)
	if job := root.state.Job; !job.Is(state.JobBringUp) || !job.StopAfter || !job.QuitAfter {
		t.Errorf("job = %+v while the bring-up runs, want the bring-up, then a stop and a quit", job)
	}
	root.Update(bringUpDoneMsg{err: context.Canceled})
	if job := root.state.Job; !job.Is(state.JobStop) || !job.QuitAfter {
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
	run := bringUp{session: root.session, out: newBus(t.Context()), steps: root.state.Steps}
	stop := time.AfterFunc(200*time.Millisecond, cancel)
	defer stop.Stop()
	runSteps(ctx, cancel, run, 0)
	for range 3 {
		switch msg := (<-run.out.ch).(type) {
		case stepDoneMsg:
			if !errors.Is(msg.err, context.Canceled) {
				t.Errorf("the step ended with %v, want context.Canceled", msg.err)
			}
		case bringUpDoneMsg:
			if !errors.Is(msg.err, context.Canceled) {
				t.Errorf("the bring-up ended with %v, want context.Canceled", msg.err)
			}
		}
	}
}

// TestStoppedBringUp says where the operator stopped the bring-up, apart
// from a failure: no error, no ✕, and no failed progress.
func TestStoppedBringUp(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.state.Status.Tunnel.Running = true
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
	for _, row := range root.state.Steps {
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
	root.state.Read, root.state.Status = true, sandbox.Status{ClusterExists: true}
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
	go followTraffic(ctx, root.session, samplePod, store)
	for deadline := time.Now().Add(5 * time.Second); store.Err() == nil && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if store.Err() == nil {
		t.Fatal("no error from following a pod with no zone step, want one")
	}
	root.state.Traffic.SetErr(store.Err())
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
		root.state.Read, root.state.Status = true, sandbox.Status{}
		_, cmd := root.Update(press(key))
		if cmd != nil || root.overlay.kind != overlayNone || root.state.Job.Busy() ||
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
	root.state.Status = sandbox.Status{Deployed: true, ClusterExists: true}
	root.Update(press("D"))
	if root.overlay.kind != overlayNone || !strings.Contains(root.ui.Toast.Text, "nothing for D to stop") {
		t.Errorf("D: overlay %v, notice %q; want only a notice", root.overlay.kind, root.ui.Toast.Text)
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
	root.state.Job = state.Job{Kind: state.JobStop, Name: "Stopping everything", QuitAfter: true}
	if _, cmd := root.Update(actionDoneMsg{err: sandbox.ErrDocker}); cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Error("the TUI quit after stopping everything failed")
		}
	}
	if !strings.Contains(root.ui.Toast.Text, "Stopping everything failed") {
		t.Errorf("notice = %q, want the failure", root.ui.Toast.Text)
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
	root.Update(statusMsg{status: root.state.Status})
	if _, next := root.Update(podsMsg{rows: root.state.Pods, agents: root.state.Agents}); next == nil {
		t.Error("the last read came back, and no next read is planned")
	}
	// An action that ends reads the sandbox at once, and the poll planned
	// above stays the only one.
	root.state.Job = state.Job{Kind: state.JobAction, Name: "Waiting"}
	if _, read := root.Update(actionDoneMsg{}); read == nil {
		t.Fatal("an action ended, and the sandbox was not read again")
	}
	root.Update(statusMsg{status: root.state.Status})
	if _, next := root.Update(podsMsg{rows: root.state.Pods, agents: root.state.Agents}); next != nil {
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
	cluster := slices.IndexFunc(root.state.Steps, func(row *state.StepRow) bool {
		return row.Step.ID == sandbox.StepCluster
	})
	root.state.Job = state.Job{Kind: state.JobBringUp}
	root.Update(press("D"))
	root.Update(stepDoneMsg{index: cluster, err: sandbox.ErrOtherIssuer})
	answerYes(root)
	if root.overlay.kind != overlayNone || root.waiting != nil || !root.state.Job.StopAfter {
		t.Errorf("after D: overlay %v, waiting %v, job %+v; want the question gone and everything to stop",
			root.overlay.kind, root.waiting != nil, root.state.Job)
	}
	root.state.Job = state.Job{}
	root.state.ResetSteps()
	root.Update(recreateClusterMsg{from: cluster})
	if root.state.Job.Busy() || !strings.Contains(root.ui.Toast.Text, "does not need to be recreated") {
		t.Errorf("a late answer: job %+v, notice %q; want no bring-up", root.state.Job, root.ui.Toast.Text)
	}
}
