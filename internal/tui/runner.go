package tui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

const (
	// readTimeout bounds one read of the pods or the services.
	readTimeout = 10 * time.Second
	// pollReads is how many reads a poll makes: the services and the pods.
	pollReads = 2
	// streamRetry is the pause before a container's log is followed again,
	// as when the container restarts.
	streamRetry = 2 * time.Second
	// trafficTail is how many of the proxy's records the TUI starts with.
	trafficTail = 2000
	// clearScreen clears the terminal and puts the cursor at the top.
	clearScreen = "\x1b[2J\x1b[H"
)

// The messages background work sends.
type (
	frameMsg     struct{}
	pollMsg      struct{}
	stepStartMsg struct{ index int }
	stepDoneMsg  struct {
		index int
		err   error
	}
	bringUpDoneMsg struct{ err error }
	statusMsg      struct{ status sandbox.Status }
	podsMsg        struct {
		rows   []page.ContainerRow
		agents []sandbox.Agent
		err    error
	}
	actionDoneMsg struct{ err error }
	execDoneMsg   struct {
		plain bool
		err   error
	}
	eventsMsg struct {
		url string
		err error
	}
)

// The answers a confirmation sends. Each handler checks, when it runs, that
// the work still applies.
type (
	stopBringUpMsg     struct{}
	recreateClusterMsg struct{ from int }
	restartPodMsg      struct{}
	stopAllMsg         struct{ quit bool }
)

// errPanic is a background job that panicked. The TUI reports it and keeps
// running.
var errPanic = errors.New("internal error")

func frameTick() tea.Cmd {
	return tea.Tick(frameEvery, func(time.Time) tea.Msg { return frameMsg{} })
}

func pollTick() tea.Cmd {
	return tea.Tick(pollEvery, func(time.Time) tea.Msg { return pollMsg{} })
}

// startBringUp runs the steps from index from on, one after another, in the
// background. With recreate, the cluster step deletes the cluster and
// creates it again. A failed step stops the run, except the checks: the
// sandbox is up even when a check fails.
func (m *model) startBringUp(from int, recreate bool) {
	shared := m.shared
	if from >= len(shared.Steps) {
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	if !shared.StartJob(page.Job{Kind: page.JobBringUp, From: from}, cancel) {
		cancel()
		return
	}
	if from == 0 {
		shared.Record("▸", "Bring-up started")
	} else {
		shared.Record("▸", "Bring-up started again at: "+shared.Steps[from].Step.Title)
	}
	for _, row := range shared.Steps[from:] {
		row.State, row.Err = page.StepPending, nil
		row.Log.Reset()
	}
	m.trackStep(from)
	run := bringUp{session: shared.Session, out: m.bus, steps: slices.Clone(shared.Steps), recreate: recreate}
	m.jobs.Go(func() { runSteps(ctx, cancel, run, from) })
}

// bringUp is what a bring-up's goroutine needs.
type bringUp struct {
	session  *sandbox.Session
	out      *bus
	steps    []*page.StepRow
	recreate bool
}

// runSteps runs the steps from index from on, and reports each start and
// end, then the bring-up's end. It stops at a failed step other than the
// checks.
func runSteps(ctx context.Context, cancel context.CancelFunc, run bringUp, from int) {
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			run.out.Send(bringUpDoneMsg{err: fmt.Errorf("%w: %v", errPanic, r)})
		}
	}()
	for i := from; i < len(run.steps); i++ {
		row := run.steps[i]
		run.out.Send(stepStartMsg{index: i})
		var err error
		if run.recreate && row.Step.ID == sandbox.StepCluster {
			err, run.recreate = run.session.RecreateCluster(ctx, row.Log), false
		} else {
			err = run.session.Run(ctx, row.Step.ID, row.Log)
		}
		// A tool that ctx killed fails with its own error, such as signal:
		// killed. The step reports the stop instead.
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		run.out.Send(stepDoneMsg{index: i, err: err})
		if err != nil && row.Step.ID != sandbox.StepChecks {
			run.out.Send(bringUpDoneMsg{err: err})
			return
		}
	}
	run.out.Send(bringUpDoneMsg{})
}

// trackStep follows step i on Home.
func (m *model) trackStep(i int) {
	m.shared.StepCursor = i
}

func (m *model) onStepStart(msg stepStartMsg) {
	row := m.shared.Steps[msg.index]
	row.State, row.Started, row.Ended = page.StepRunning, time.Now(), time.Time{}
	m.trackStep(msg.index)
}

// onStepDone marks the step, and offers to recreate a cluster that was made
// for another tunnel URL. A step the operator stopped says so, and not why
// it ended.
func (m *model) onStepDone(msg stepDoneMsg) {
	shared := m.shared
	row := shared.Steps[msg.index]
	row.Ended, row.Err = time.Now(), msg.err
	row.State = page.StepDone
	if row.Step.ID == sandbox.StepChecks {
		m.recordChecks(row, msg.err)
	} else if msg.err == nil {
		shared.Record("✓", row.Step.Title+" ("+component.Duration(row.Ended.Sub(row.Started))+")")
	}
	if msg.err == nil {
		return
	}
	row.State = page.StepFailed
	if row.Stopped() {
		m.recordStop(row)
		return
	}
	if row.Step.ID != sandbox.StepChecks {
		shared.Record("✕", row.Step.Title+": "+component.FirstLine(msg.err.Error()))
	}
	row.Log.Add("", "✕ "+msg.err.Error())
	if hint := page.StepHint(row, msg.err); hint != "" {
		row.Log.Add("→ " + hint)
	}
	if recreatable(msg.err) {
		m.confirmLater("Recreate the cluster?", recreateReason(msg.err), recreateClusterMsg{from: msg.index})
	}
}

// recreatable reports whether err is a cluster that only a new one fixes:
// one made for another issuer, or one kubectl cannot reach.
func recreatable(err error) bool {
	return errors.Is(err, sandbox.ErrOtherIssuer) || errors.Is(err, sandbox.ErrBrokenCluster)
}

// recordStop says in the activity log and in the step's log that the
// operator stopped the bring-up at row. recordChecks says it for the checks.
func (m *model) recordStop(row *page.StepRow) {
	row.Log.Add("", "○ You stopped the bring-up here.")
	if row.Step.ID != sandbox.StepChecks {
		m.shared.Record("•", "Stopped the bring-up at: "+row.Step.Title)
	}
}

// recordChecks counts the checks that passed, and records how they went,
// that they were stopped, or that they did not run.
func (m *model) recordChecks(row *page.StepRow, err error) {
	row.Passed, row.Total = countChecks(row.Log)
	switch {
	case errors.Is(err, context.Canceled):
		m.shared.Record("•", "Stopped the checks")
	case err != nil && row.Total == 0:
		m.shared.Record("✕", "The checks did not run: "+component.FirstLine(err.Error()))
	case err != nil:
		m.shared.Record("✕", fmt.Sprintf("Checks: %d of %d passed", row.Passed, row.Total))
	default:
		m.shared.Record("✓", fmt.Sprintf("Checks: %d of %d passed", row.Passed, row.Total))
	}
}

// recreateReason says why the cluster must be created again, and that the
// sandbox's Keycard setup stays.
func recreateReason(err error) string {
	why := "The cluster was made for another issuer URL, and a cluster keeps its issuer for life.\n" +
		"Delete it and create it again for the new URL?"
	if errors.Is(err, sandbox.ErrBrokenCluster) {
		why = "Your kubeconfig has no context for the cluster, so kubectl cannot reach it.\n" +
			"Delete it and create it again?"
	}
	return why + " It takes about a minute, and the\nsandbox's Keycard setup stays."
}

// countChecks counts the PASS and FAIL lines the checks print.
func countChecks(log *component.Log) (int, int) {
	lines, _ := log.Snapshot()
	passed, total := 0, 0
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "  PASS"):
			passed++
			total++
		case strings.HasPrefix(line, "  FAIL"):
			total++
		}
	}
	return passed, total
}

// onBringUpDone ends the run and points the operator at the agent. When
// everything is to stop, it stops now. A run of the checks alone brings
// nothing up, so it says nothing more than the checks did.
func (m *model) onBringUpDone(msg bringUpDoneMsg) tea.Cmd {
	shared := m.shared
	ended := shared.EndJob()
	switch {
	case errors.Is(msg.err, context.Canceled):
		// The step that ran says where the operator stopped the bring-up.
	case msg.err != nil:
		if !recreatable(msg.err) {
			shared.Notify("The bring-up stopped: "+component.FirstLine(msg.err.Error()), true)
		}
		shared.Record("✕", "Bring-up stopped")
	case shared.Steps[ended.From].Step.ID == sandbox.StepChecks:
	default:
		shared.Record("✓", "The sandbox is up")
		m.notifyUp()
	}
	if ended.StopAfter {
		m.stopEverything(ended.QuitAfter)
		return nil
	}
	return m.poll()
}

// notifyUp says the sandbox is up, and what to do with the agent.
func (m *model) notifyUp() {
	shared := m.shared
	scenario := shared.Scenario()
	switch {
	case scenario == nil:
	case scenario.Run.Mode == sandbox.ModeBackground:
		shared.Notify("The sandbox is up, and the agent runs in the background. Run sandbox logs to follow it.", false)
	case scenario.Run.Mode == sandbox.ModeShell && len(scenario.Run.Command) > 0:
		shared.Notify("The sandbox is up. Press a to open "+scenario.Run.Command[0]+".", false)
	default:
		shared.Notify("The sandbox is up. Press x to open a shell in the agent.", false)
	}
}

func (m *model) toggleBringUp() {
	if !m.shared.Job.Is(page.JobBringUp) {
		if m.shared.Busy() {
			return
		}
		from := m.shared.ResumeIndex()
		if from > 0 {
			m.shared.Notify("Starting again at: "+m.shared.Steps[from].Step.Title, false)
		}
		m.startBringUp(from, false)
		return
	}
	m.confirm("Stop the bring-up?", "The step that runs now is canceled.\n"+
		"Press u again later to start again from that step.", stopBringUpMsg{})
}

// updateAnswers acts on the answer a confirmation sent, and reports whether
// msg was one. A question that waited opens after it.
func (m *model) updateAnswers(msg tea.Msg) bool {
	switch msg := msg.(type) {
	case stopBringUpMsg:
		if m.shared.Job.Is(page.JobBringUp) {
			m.shared.CancelJob()
		}
	case recreateClusterMsg:
		m.recreateCluster(msg.from)
	case restartPodMsg:
		m.restartPod()
	case stopAllMsg:
		m.stopAllNow(msg.quit)
	default:
		return false
	}
	m.openWaiting()
	return true
}

// stopAllNow stops everything, then quits with quit, unless a job other
// than a bring-up runs.
func (m *model) stopAllNow(quit bool) {
	if !m.shared.Job.Is(page.JobBringUp) && m.shared.Busy() {
		return
	}
	m.stopEverything(quit)
}

// recreateCluster deletes the cluster and brings the sandbox up again from
// step from, while that step still failed for a cluster that only a new one
// fixes. Once D stopped everything, or a later bring-up passed the step, the
// question no longer applies.
func (m *model) recreateCluster(from int) {
	shared := m.shared
	if from >= len(shared.Steps) || shared.Steps[from].State != page.StepFailed ||
		!recreatable(shared.Steps[from].Err) {
		shared.Notify("The cluster does not need to be recreated now.", false)
		return
	}
	if shared.Busy() {
		return
	}
	m.startBringUp(from, true)
}

// runChecks runs the checks step again, against the running sandbox.
func (m *model) runChecks() {
	if m.shared.Busy() || m.isDown() {
		return
	}
	for i, row := range m.shared.Steps {
		if row.Step.ID == sandbox.StepChecks {
			m.startBringUp(i, false)
		}
	}
}

// agent opens the scenario's command in the agent, such as pi, or shows the
// agent's log for an agent that runs in the background.
func (m *model) agent() tea.Cmd {
	scenario, err := m.shared.LoadScenario()
	if err != nil {
		m.shared.Notify(component.FirstLine(err.Error()), true)
		return nil
	}
	if scenario.Run.Mode == sandbox.ModeBackground {
		m.shared.Notify("The agent runs in the background. Run sandbox logs to follow its log.", false)
		return nil
	}
	return m.openAgent(false)
}

func (m *model) restartAgent() {
	if m.shared.Busy() || m.isDown() {
		return
	}
	m.confirm("Restart "+m.shared.AgentName()+"'s pod?", "This deletes the pod, and the controller creates it again\n"+
		"with the current images and settings. The agent loses what it kept in the pod.", restartPodMsg{})
}

// restartPod restarts the selected agent's pod, while nothing else runs and
// the sandbox is up.
func (m *model) restartPod() {
	if m.shared.Busy() || m.isDown() {
		return
	}
	job := page.Job{Kind: page.JobAction, Name: "Restarting " + m.shared.AgentName() + "'s pod"}
	m.runAction(job, m.shared.Session.RestartAgent)
}

// stopAll asks, then stops everything. A running bring-up stops first.
// On an existing cluster, the TUI did not start anything that D stops.
func (m *model) stopAll() {
	if !m.shared.Job.Is(page.JobBringUp) && m.shared.Busy() {
		return
	}
	if m.shared.Session.Local().Existing() {
		m.shared.Notify("On an existing cluster, there is nothing for D to stop.", false)
		return
	}
	m.askStopAll(false)
}

// askStopAll asks whether to stop everything, and with quit, to quit
// after.
func (m *model) askStopAll(quit bool) {
	title := "Stop everything?"
	if quit {
		title = "Stop everything and quit?"
	}
	m.confirm(title, "This stops the tunnel and the mirror and deletes the k3d cluster.\n"+
		"What the sandbox created in your Keycard zone stays.", stopAllMsg{quit: quit})
}

// stopEverything stops the tunnel and the mirror and deletes the cluster,
// then quits with quit. A running bring-up stops first, and
// onBringUpDone stops everything once it has.
func (m *model) stopEverything(quit bool) {
	shared := m.shared
	// A question about the cluster's issuer is gone with the cluster.
	m.waiting = nil
	if shared.Job.Is(page.JobBringUp) {
		shared.Job.StopAfter, shared.Job.QuitAfter = true, quit
		shared.CancelJob()
		shared.Notify("Stopping the bring-up first…", false)
		return
	}
	m.runAction(page.Job{Kind: page.JobStop, Name: "Stopping everything", QuitAfter: quit}, shared.Session.Down)
}

// runAction runs job's work in the background, and reports when it is
// done. While another job runs, it refuses and says so.
func (m *model) runAction(job page.Job, work func(ctx context.Context, out io.Writer) error) {
	shared := m.shared
	if !shared.StartJob(job, nil) {
		return
	}
	shared.Record("▸", job.Name)
	shared.Notify(job.Name+"…", false)
	ctx, out := m.ctx, m.bus
	m.jobs.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				out.Send(actionDoneMsg{err: fmt.Errorf("%w: %v", errPanic, r)})
			}
		}()
		out.Send(actionDoneMsg{err: work(ctx, io.Discard)})
	})
}

// onActionDone ends the job, and says how it went. Once everything
// stopped, the bring-up that stopped or failed before is gone with the
// sandbox, so its steps start over.
func (m *model) onActionDone(msg actionDoneMsg) tea.Cmd {
	shared := m.shared
	ended := shared.EndJob()
	if msg.err != nil {
		shared.Notify(ended.Name+" failed: "+component.FirstLine(msg.err.Error()), true)
		shared.Record("✕", ended.Name+" failed: "+component.FirstLine(msg.err.Error()))
	} else {
		shared.Notify(ended.Name+": done", false)
		shared.Record("✓", ended.Name+": done")
		if ended.Is(page.JobStop) {
			shared.ResetSteps()
		}
	}
	// A quit that waited for the job does not hide its failure.
	if ended.QuitAfter && msg.err == nil {
		return tea.Quit
	}
	return m.poll()
}

// poll reads the services and the pods in the background, unless a read
// is on its way. It reads the pods only while the last read found the
// sandbox deployed in its cluster. Once both reads come back, polled plans
// the next poll.
func (m *model) poll() tea.Cmd {
	if m.polling > 0 {
		return nil
	}
	m.polling = pollReads
	base, session := m.ctx, m.shared.Session
	// The pods need the cluster and the zone step. Without them, kubectl
	// has nothing to list, so the poll skips it.
	last := m.shared.Status
	noPods := m.shared.Read && (!last.ClusterExists || !last.Deployed)
	status := func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, readTimeout)
		defer cancel()
		return statusMsg{status: session.Status(ctx)}
	}
	pods := func() tea.Msg {
		if noPods {
			return podsMsg{agents: session.Agents()}
		}
		ctx, cancel := context.WithTimeout(base, readTimeout)
		defer cancel()
		list, err := session.Pods(ctx)
		var rows []page.ContainerRow
		for _, pod := range list {
			for _, container := range pod.Containers {
				rows = append(rows, page.ContainerRow{Pod: pod.Name, Container: container})
			}
		}
		return podsMsg{rows: rows, agents: session.Agents(), err: err}
	}
	return tea.Batch(status, pods)
}

// polled counts a read that came back, and plans the next poll pollEvery
// after the last one, unless one is planned.
func (m *model) polled() tea.Cmd {
	m.polling = max(m.polling-1, 0)
	if m.polling > 0 || m.tickPlanned {
		return nil
	}
	m.tickPlanned = true
	return pollTick()
}

func (m *model) onPods(msg podsMsg) {
	shared := m.shared
	shared.AllPods, shared.Agents, shared.PodsErr = msg.rows, msg.agents, msg.err
	m.syncAgents()
	if len(shared.Pods) > 0 {
		m.startTraffic()
	}
}

// syncAgents keeps the selection on an agent in the list, and shows the
// selected agent's pod alone. When that pod changed, Home moves to it.
func (m *model) syncAgents() {
	shared := m.shared
	if _, index := shared.SelectedAgent(); index < 0 && len(shared.Agents) > 0 {
		shared.Session.SelectAgent(shared.Agents[0].Email)
	}
	agent, _ := shared.SelectedAgent()
	var pods []page.ContainerRow
	for _, row := range shared.AllPods {
		if agent.Pod != "" && row.Pod == agent.Pod {
			pods = append(pods, row)
		}
	}
	shared.Pods = pods
	if agent.Pod != m.shownPod {
		m.shownPod = agent.Pod
		m.agentSwitched()
	}
}

// agentSwitched points Home at the agent selected now: its traffic.
func (m *model) agentSwitched() {
	m.startTraffic()
}

// startTraffic follows the proxy of the selected agent's pod, once there
// is one, and again when the pod changes.
func (m *model) startTraffic() {
	shared := m.shared
	target, err := shared.Session.Target()
	if err != nil || target.Pod == "" {
		// The selected agent has no pod: its traffic is empty.
		if m.trafficStop != nil {
			m.trafficStop()
			m.trafficStop = nil
		}
		shared.Traffic.Reset("")
		return
	}
	if _, pod := shared.Traffic.Snapshot(); pod == target.Pod && m.trafficStop != nil {
		return
	}
	if m.trafficStop != nil {
		m.trafficStop()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.trafficStop = cancel
	shared.Traffic.Reset(target.Pod)
	session, store, pod := shared.Session, &shared.Traffic, target.Pod
	m.jobs.Go(func() { followTraffic(ctx, session, pod, store) })
}

// followTraffic follows the proxy's log in pod until ctx ends, and keeps
// each audit record in store. Other log lines are left out. When kubectl
// stops, or cannot start, it keeps why in store and follows the log again
// streamRetry later.
func followTraffic(ctx context.Context, session *sandbox.Session, pod string, store *proxylog.Store) {
	for ctx.Err() == nil {
		store.Reset(pod)
		err := streamRecords(ctx, session, pod, store)
		if ctx.Err() != nil {
			return
		}
		store.SetErr(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(streamRetry):
		}
	}
}

// streamRecords runs kubectl logs once, and reads the proxy's audit
// records into store. It returns why kubectl stopped, with the first line
// it wrote to standard error.
func streamRecords(ctx context.Context, session *sandbox.Session, pod string, store *proxylog.Store) error {
	cmd, err := session.LogsCommand(ctx, pod, page.ProxyContainer, trafficTail, true)
	if err != nil {
		return fmt.Errorf("following the proxy's log: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return fmt.Errorf("following the proxy's log: %w", err)
	}
	readErr := readRecords(stdout, store)
	if err = cmd.Wait(); err != nil {
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return fmt.Errorf("kubectl logs: %w: %s", err, component.FirstLine(text))
		}
		return fmt.Errorf("kubectl logs: %w", err)
	}
	return readErr
}

// readRecords reads the proxy's audit records from stdout into store. The
// first line it reads clears the store's error.
func readRecords(stdout io.Reader, store *proxylog.Store) error {
	reading := false
	err := component.ReadLines(stdout, func(line string) {
		if !reading {
			reading = true
			store.SetErr(nil)
		}
		if record, ok := proxylog.Parse([]byte(line)); ok {
			store.Add(record)
		}
	})
	if err != nil {
		return fmt.Errorf("reading the proxy's log: %w", err)
	}
	return nil
}

// openEvents opens the zone's event log in the console, where the zone
// records each credential it issues to the proxy.
func (m *model) openEvents() tea.Cmd {
	m.shared.Notify("Finding the zone in the Keycard console…", false)
	base, session := m.ctx, m.shared.Session
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, readTimeout)
		defer cancel()
		address, err := session.ZoneEventsURL(ctx)
		if err != nil {
			return eventsMsg{err: err}
		}
		return eventsMsg{url: address, err: openBrowser(ctx, address)}
	}
}

func (m *model) onEvents(msg eventsMsg) {
	shared := m.shared
	switch {
	case msg.url == "":
		shared.Notify("Cannot find the zone's events: "+component.FirstLine(msg.err.Error()), true)
		shared.Record("✕", "Could not find the zone's events: "+component.FirstLine(msg.err.Error()))
	case msg.err != nil:
		shared.Notify("Open "+msg.url, false)
		shared.Record("•", "The zone's events are at "+msg.url)
	default:
		shared.Notify("Opened the zone's events in the browser.", false)
		shared.Record("✓", "Opened the zone's events: "+msg.url)
	}
}

// openBrowser opens address in the default browser.
func openBrowser(ctx context.Context, address string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	//nolint:gosec // G204: the address is the console's, made from the settings.
	if err := exec.CommandContext(ctx, opener, address).Run(); err != nil {
		return fmt.Errorf("opening %s with %s: %w", address, opener, err)
	}
	return nil
}

// openAgent hands the terminal to a command in the agent container: the
// scenario's command, such as pi, or sh when plain is set. The screen comes
// back when it exits.
func (m *model) openAgent(plain bool) tea.Cmd {
	shared := m.shared
	if shared.Busy() || m.isDown() {
		return nil
	}
	cmd, err := shared.Session.ShellCommand(m.ctx, plain)
	if err != nil {
		shared.Notify("Cannot open the agent: "+component.FirstLine(err.Error()), true)
		return nil
	}
	title, leave := "sh in "+shared.AgentName(), "Type exit to come back to "+style.AppName+"."
	if scenario, err := shared.Session.Scenario(); err == nil && !plain && len(scenario.Run.Command) > 0 {
		title = scenario.Run.Command[0] + " in " + shared.AgentName()
		leave = "Quit " + scenario.Run.Command[0] + " to come back to " + style.AppName + "."
	}
	shared.Record("▸", "Opened "+title)
	return tea.Exec(&agentExec{cmd: cmd, title: title, leave: leave}, func(err error) tea.Msg {
		return execDoneMsg{plain: plain, err: err}
	})
}

// onExecDone notes how the agent's command ended, and reads the sandbox
// again.
func (m *model) onExecDone(msg execDoneMsg) tea.Cmd {
	var exit *exec.ExitError
	switch {
	case msg.err == nil:
		m.shared.Record("✓", "Came back from the agent")
	case msg.plain && errors.As(msg.err, &exit):
		// sh exits with the status of the last command it ran, such as a
		// curl the proxy refused.
		m.shared.Record("•", "Came back from the shell, which exited with status "+strconv.Itoa(exit.ExitCode()))
	default:
		m.shared.Record("✕", "The agent's command ended: "+component.FirstLine(msg.err.Error()))
		m.shared.Notify("The agent's command ended: "+component.FirstLine(msg.err.Error()), true)
	}
	return tea.Batch(m.poll(), tea.RequestWindowSize)
}

// agentExec runs a command in the agent container with the terminal. It
// says first what runs and how to come back, and when the command fails it
// waits for Enter, so the error stays readable. It implements
// tea.ExecCommand.
type agentExec struct {
	cmd    *exec.Cmd
	title  string
	leave  string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func (e *agentExec) SetStdin(r io.Reader)  { e.stdin = r }
func (e *agentExec) SetStdout(w io.Writer) { e.stdout = w }
func (e *agentExec) SetStderr(w io.Writer) { e.stderr = w }

func (e *agentExec) Run() error {
	e.cmd.Stdin, e.cmd.Stdout, e.cmd.Stderr = e.stdin, e.stdout, e.stderr
	_, _ = fmt.Fprintf(e.stdout, "%s── Keycard sandbox · %s ──\n%s\n\n", clearScreen, e.title, e.leave)
	err := e.cmd.Run()
	if err != nil {
		_, _ = fmt.Fprintf(e.stdout, "\n%s ended: %v\nPress Enter to go back to %s.", e.title, err, style.AppName)
		_, _ = bufio.NewReader(e.stdin).ReadString('\n')
		return fmt.Errorf("%s: %w", e.title, err)
	}
	return nil
}
