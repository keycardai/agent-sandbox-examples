package tui

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

const (
	// requestTimeout bounds a request the Agents tab sends, with kubectl.
	requestTimeout = 40 * time.Second
	// readTimeout bounds one read of the pods or the services.
	readTimeout = 10 * time.Second
	// pollReads is how many reads a poll makes: the services and the pods.
	pollReads = 2
	// streamRetry is the pause before a log is followed again.
	streamRetry = 2 * time.Second
	// trafficTail is how many of the proxy's records the TUI starts with.
	trafficTail = 2000
	// newAgentTitle titles the questions for a new agent.
	newAgentTitle = "New agent"
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
	statusMsg      struct {
		status sandbox.Status
		mark   int
	}
	podsMsg struct {
		rows   []state.ContainerRow
		agents []sandbox.Agent
		err    error
		mark   int
	}
	actionDoneMsg struct{ err error }
	execDoneMsg   struct {
		plain bool
		err   error
	}
	setupDoneMsg struct {
		result   sandbox.InitResult
		err      error
		describe map[string]string
		problems []error
	}
	// agentAddedMsg reports the end of adding an agent.
	agentAddedMsg struct {
		email    string
		err      error
		agents   []sandbox.Agent
		describe map[string]string
		problems []error
	}
	// settingsMsg is the settings as an action left them.
	settingsMsg struct {
		describe map[string]string
		problems []error
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
	forgetSettingsMsg  struct{}
	removeAgentMsg     struct{ email string }
)

// errPanic reports a panic in background work. The TUI keeps running.
var errPanic = errors.New("internal error")

// inBackground runs work on a goroutine Run waits for, and sends its
// message. A panic sends failed's message with errPanic instead.
func (m *model) inBackground(work func() tea.Msg, failed func(err error) tea.Msg) {
	out := m.bus
	m.jobs.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				out.Send(failed(fmt.Errorf("%w: %v", errPanic, r)))
			}
		}()
		out.Send(work())
	})
}

func frameTick() tea.Cmd {
	return tea.Tick(frameEvery, func(time.Time) tea.Msg { return frameMsg{} })
}

func pollTick() tea.Cmd {
	return tea.Tick(pollEvery, func(time.Time) tea.Msg { return pollMsg{} })
}

// recoverInto writes a panic of the goroutine it is deferred in to log.
func recoverInto(log *component.Log) {
	if r := recover(); r != nil {
		log.Add(fmt.Sprintf("✕ %v: %v", errPanic, r))
	}
}

// startBringUp runs the steps from index from on in the background. With
// recreate, the cluster step deletes the cluster and creates it again.
func (m *model) startBringUp(from int, recreate bool) {
	if from >= len(m.state.Run.Steps) {
		return
	}
	kind := state.JobBringUp
	if m.state.Run.Steps[from].Step.ID == sandbox.StepChecks {
		kind = state.JobChecks
	}
	ctx, cancel := context.WithCancel(m.ctx)
	if !m.state.Run.StartJob(state.Job{Kind: kind, From: from}, cancel) {
		cancel()
		return
	}
	switch {
	case kind == state.JobChecks:
		m.state.Record("▸", "Running the checks")
	case from == 0:
		m.state.Record("▸", "Bring-up started")
	default:
		m.state.Record("▸", "Bring-up started again at: "+m.state.Run.Steps[from].Step.Title)
	}
	m.state.Run.StartSteps(from)
	run := bringUp{session: m.session, out: m.bus, steps: slices.Clone(m.state.Run.Steps), recreate: recreate}
	m.inBackground(func() tea.Msg {
		defer cancel()
		return bringUpDoneMsg{err: runSteps(ctx, run, from)}
	}, func(err error) tea.Msg { return bringUpDoneMsg{err: err} })
}

// bringUp is what a bring-up's goroutine needs.
type bringUp struct {
	session  *sandbox.Session
	out      *bus
	steps    []*state.StepRow
	recreate bool
}

// runSteps runs the steps from index from on, and reports each start and
// end. It stops at a failed step other than the checks.
func runSteps(ctx context.Context, run bringUp, from int) error {
	for i := from; i < len(run.steps); i++ {
		row := run.steps[i]
		run.out.Send(stepStartMsg{index: i})
		var err error
		if run.recreate && row.Step.ID == sandbox.StepCluster {
			err, run.recreate = run.session.RecreateCluster(ctx, row.Log), false
		} else {
			err = run.session.Run(ctx, row.Step.ID, row.Log)
		}
		// A tool ctx killed fails with its own error, such as signal: killed.
		if err != nil && ctx.Err() != nil {
			err = context.Canceled
		}
		run.out.Send(stepDoneMsg{index: i, err: err})
		if err != nil && row.Step.ID != sandbox.StepChecks {
			return err
		}
	}
	return nil
}

// onStepDone marks the step, and offers to recreate a cluster that only a
// new one fixes.
func (m *model) onStepDone(msg stepDoneMsg) {
	row := m.state.StepEnded(msg.index, msg.err)
	if row.Step.ID == sandbox.StepChecks {
		m.recordChecks(row, msg.err)
	} else if msg.err == nil {
		m.state.Record("✓", row.Step.Title+" ("+component.Duration(row.Ended.Sub(row.Started))+")")
	}
	if msg.err == nil {
		return
	}
	if row.Stopped() {
		m.recordStop(row)
		return
	}
	if row.Step.ID != sandbox.StepChecks {
		m.state.Record("✕", row.Step.Title+": "+component.FirstLine(msg.err.Error()))
	}
	row.Log.Add("", "✕ "+msg.err.Error())
	if hint := page.StepHint(row, msg.err); hint != "" {
		row.Log.Add("→ " + hint)
	}
	if recreatable(msg.err) {
		m.confirmLater("Recreate the cluster?", recreateReason(msg.err), recreateClusterMsg{from: msg.index})
	}
}

// recreatable reports whether err is a cluster that only a new one fixes.
func recreatable(err error) bool {
	return errors.Is(err, sandbox.ErrOtherIssuer) || errors.Is(err, sandbox.ErrBrokenCluster)
}

// recordStop notes the stop at row. recordChecks notes it for the checks.
func (m *model) recordStop(row *state.StepRow) {
	row.Log.Add("", "○ You stopped the bring-up here.")
	if row.Step.ID != sandbox.StepChecks {
		m.state.Record("•", "Stopped the bring-up at: "+row.Step.Title)
	}
}

// recordChecks records how the checks went.
func (m *model) recordChecks(row *state.StepRow, err error) {
	switch {
	case errors.Is(err, context.Canceled):
		m.state.Record("•", "Stopped the checks")
	case err != nil && row.Total == 0:
		m.state.Record("✕", "The checks did not run: "+component.FirstLine(err.Error()))
	case err != nil:
		m.state.Record("✕", fmt.Sprintf("Checks: %d of %d passed", row.Passed, row.Total))
	default:
		m.state.Record("✓", fmt.Sprintf("Checks: %d of %d passed", row.Passed, row.Total))
	}
}

// recreateReason says why the cluster must be created again.
func recreateReason(err error) string {
	why := "The cluster was made for another issuer URL, and a cluster keeps its issuer for life.\n" +
		"Delete it and create it again for the new URL?"
	if errors.Is(err, sandbox.ErrBrokenCluster) {
		why = "Your kubeconfig has no context for the cluster, so kubectl cannot reach it.\n" +
			"Delete it and create it again?"
	}
	return why + " It takes about a minute, and the\nsandbox's Keycard setup stays."
}

// onBringUpDone ends the run and points the operator at the agent. A run
// of the checks alone says nothing more than the checks did.
func (m *model) onBringUpDone(msg bringUpDoneMsg) tea.Cmd {
	ended := m.state.EndJob()
	switch {
	case errors.Is(msg.err, context.Canceled):
		// The step that ran says where the operator stopped the bring-up.
	case msg.err != nil:
		if !recreatable(msg.err) {
			m.ui.Notify("The bring-up stopped: "+component.FirstLine(msg.err.Error()), true)
		}
		m.state.Record("✕", "Bring-up stopped")
	case ended.Is(state.JobChecks):
	default:
		m.state.Record("✓", "The sandbox is up")
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
	scenario := m.state.LoadedDemo()
	switch {
	case scenario == nil:
	case scenario.Run.Mode == sandbox.ModeBackground:
		m.ui.Notify("The sandbox is up, and the agent runs in the background. Its log is on the Agents tab.", false)
	case scenario.Run.Mode == sandbox.ModeShell && len(scenario.Run.Command) > 0:
		m.ui.Notify("The sandbox is up. Press a to open "+scenario.Run.Command[0]+", or s to send a request.", false)
	default:
		m.ui.Notify("The sandbox is up. Press s to send a request from the agent.", false)
	}
}

// watchBringUp shows a bring-up that starts on Home.
func (m *model) watchBringUp() { m.openTab(tabHome) }

func (m *model) toggleBringUp() {
	if m.state.Run.Job.Is(state.JobChecks) {
		m.confirm("Stop the checks?", "The check that runs now is canceled.", stopBringUpMsg{})
		return
	}
	if !m.state.Run.Job.Is(state.JobBringUp) {
		if m.busy() {
			return
		}
		if len(m.state.Setup.Problems) > 0 {
			m.ui.Notify("Fix the settings first. Press e to open them.", true)
			return
		}
		from := m.state.ResumeIndex()
		if from > 0 {
			m.ui.Notify("Starting again at: "+m.state.Run.Steps[from].Step.Title, false)
		}
		m.watchBringUp()
		m.startBringUp(from, false)
		return
	}
	m.confirm("Stop the bring-up?", "The step that runs now is canceled.\n"+
		"Press u again later to start again from that step.", stopBringUpMsg{})
}

// updateAnswers acts on the answer a confirmation sent, and reports whether
// msg was one. A question that waited opens after it.
func (m *model) updateAnswers(msg tea.Msg) (tea.Cmd, bool) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case stopBringUpMsg:
		if m.state.Run.Job.RunsSteps() {
			m.state.Run.CancelJob()
		}
	case recreateClusterMsg:
		m.recreateCluster(msg.from)
	case restartPodMsg:
		m.restartPod()
	case stopAllMsg:
		m.stopAllNow(msg.quit)
	case forgetSettingsMsg:
		cmd = m.forget()
	case removeAgentMsg:
		m.removeAgentNow(msg.email)
	default:
		return nil, false
	}
	m.openWaiting()
	return cmd, true
}

// stopAllNow stops everything, unless D waits for the job that runs.
func (m *model) stopAllNow(quit bool) {
	if m.stopAllWaits(quit) {
		return
	}
	m.stopEverything(quit)
}

// stopAllWaits reports whether D waits for the job that runs, and says
// which in the footer. D does not wait for a job it stops first.
func (m *model) stopAllWaits(quit bool) bool {
	job := m.state.Run.Job
	stopsFirst := job.RunsSteps() || quit && job.Is(state.JobSetup)
	return !stopsFirst && m.busy()
}

// recreateCluster deletes the cluster and brings the sandbox up again from
// step from, while that step still fails for a cluster only a new one fixes.
func (m *model) recreateCluster(from int) {
	if from >= len(m.state.Run.Steps) || m.state.Run.Steps[from].State != state.StepFailed ||
		!recreatable(m.state.Run.Steps[from].Err) {
		m.ui.Notify("The cluster does not need to be recreated now.", false)
		return
	}
	if m.busy() {
		return
	}
	m.startBringUp(from, true)
}

// runChecks runs the checks step again, against the running sandbox.
func (m *model) runChecks() {
	if m.busy() || m.isDown() {
		return
	}
	for i, row := range m.state.Run.Steps {
		if row.Step.ID == sandbox.StepChecks {
			m.watchBringUp()
			m.startBringUp(i, false)
		}
	}
}

// agent opens the scenario's command in the agent, such as pi, or shows the
// agent's log for an agent that runs in the background.
func (m *model) agent() tea.Cmd {
	scenario, err := m.state.Demo()
	if err != nil {
		m.ui.Notify(component.FirstLine(err.Error()), true)
		return nil
	}
	if scenario.Run.Mode == sandbox.ModeBackground {
		m.openTab(tabAgent)
		m.follow(m.agents.FocusLog())
		return nil
	}
	return m.openAgent(false)
}

func (m *model) restartAgent() {
	if m.busy() || m.isDown() {
		return
	}
	m.confirm("Restart "+page.AgentName(m.state)+"'s pod?", "This deletes the pod, and the controller creates it again\n"+
		"with the current images and settings. The agent loses what it kept in the pod.", restartPodMsg{})
}

// restartPod restarts the selected agent's pod, while nothing else runs and
// the sandbox is up. The agent's log starts over with the new pod.
func (m *model) restartPod() {
	if m.busy() || m.isDown() {
		return
	}
	m.stopStream()
	m.agents.Reset()
	m.layout()
	selected, _ := m.state.Selected()
	job := state.Job{
		Kind: state.JobAction, Name: "Restarting " + page.AgentName(m.state) + "'s pod",
		Agent: selected.Email,
	}
	m.runAction(job, m.session.RestartAgent)
}

// stopAll asks, then stops everything.
func (m *model) stopAll() {
	if m.stopAllWaits(false) {
		return
	}
	m.askStopAll(false)
}

// askStopAll asks whether to stop everything, and with quit, to quit after.
func (m *model) askStopAll(quit bool) {
	if m.session.Local().Existing() {
		m.ui.Notify("On an existing cluster, there is nothing for D to stop.", false)
		return
	}
	title := "Stop everything?"
	if quit {
		title = "Stop everything and quit?"
	}
	m.confirm(title, "This stops the tunnel and the mirror and deletes the k3d cluster.\n"+
		"What the sandbox created in your Keycard zone stays.", stopAllMsg{quit: quit})
}

// stopEverything stops the tunnel and the mirror and deletes the cluster.
// A bring-up, checks run or setup stops first, and the rest stops after it.
func (m *model) stopEverything(quit bool) {
	m.stopStream()
	// A question about the cluster's issuer is gone with the cluster.
	m.waiting = nil
	switch {
	case m.state.Run.Job.RunsSteps():
		what := "the bring-up"
		if m.state.Run.Job.Is(state.JobChecks) {
			what = "the checks"
		}
		m.state.Run.StopAfterJob(quit)
		m.state.Run.CancelJob()
		m.ui.Notify("Stopping "+what+" first…", false)
		return
	case m.state.Run.Job.Is(state.JobSetup):
		m.state.Run.StopAfterJob(quit)
		m.stopSetup()
		return
	}
	m.runAction(state.Job{Kind: state.JobStop, Name: "Stopping everything", QuitAfter: quit}, m.session.Down)
}

// runAction runs job's work in the background with its output in the
// action log, which the log pane shows, and reports when it is done. While
// another job runs, it refuses and says so.
func (m *model) runAction(job state.Job, work func(ctx context.Context, out io.Writer) error) {
	if !m.state.Run.StartJob(job, nil) {
		return
	}
	log := m.ui.Logs.Log(state.SourceAction)
	log.Reset()
	log.Add("$ " + job.Name)
	m.state.Record("▸", job.Name)
	m.ui.Logs.Show(state.SourceAction)
	m.ui.Notify(job.Name+"…", false)
	ctx := m.ctx
	m.inBackground(func() tea.Msg { return actionDoneMsg{err: work(ctx, log)} },
		func(err error) tea.Msg { return actionDoneMsg{err: err} })
}

// onActionDone ends the job, and says how it went. Once everything
// stopped, the steps start over and the screens wait for a new read.
func (m *model) onActionDone(msg actionDoneMsg) tea.Cmd {
	ended := m.state.EndJob()
	log := m.ui.Logs.Log(state.SourceAction)
	if msg.err != nil {
		log.Add("", "✕ "+msg.err.Error())
		if hint := page.HintFor(msg.err); hint != "" {
			log.Add("→ " + hint)
		}
		m.ui.Notify(ended.Name+" failed: "+component.FirstLine(msg.err.Error()), true)
		m.state.Record("✕", ended.Name+" failed: "+component.FirstLine(msg.err.Error()))
	} else {
		log.Add("", "✓ done")
		m.ui.Notify(ended.Name+": done", false)
		m.state.Record("✓", ended.Name+": done")
		if ended.Is(state.JobStop) {
			m.state.StoppedAll()
			m.stopTraffic()
		}
	}
	// A quit that waited for the job does not hide its failure.
	if ended.QuitAfter && msg.err == nil {
		return tea.Quit
	}
	return m.poll()
}

// poll reads the services and the pods in the background, unless a read
// is on its way. Once both reads come back, polled plans the next poll.
func (m *model) poll() tea.Cmd {
	if m.polling > 0 {
		return nil
	}
	m.polling = pollReads
	base, session, mark := m.ctx, m.session, m.state.Reads.Mark()
	// Without the cluster and the zone step, there are no pods to list.
	last := m.state.Reads.Status
	noPods := m.state.Reads.Done && (!last.ClusterExists || !last.Deployed)
	status := func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, readTimeout)
		defer cancel()
		return statusMsg{status: session.Status(ctx), mark: mark}
	}
	pods := func() tea.Msg {
		if noPods {
			return podsMsg{agents: session.Agents(), mark: mark}
		}
		ctx, cancel := context.WithTimeout(base, readTimeout)
		defer cancel()
		list, err := session.Pods(ctx)
		var rows []state.ContainerRow
		for _, pod := range list {
			for _, container := range pod.Containers {
				rows = append(rows, state.ContainerRow{Pod: pod.Name, Container: container})
			}
		}
		return podsMsg{rows: rows, agents: session.Agents(), err: err, mark: mark}
	}
	return tea.Batch(status, pods)
}

// polled counts a read that came back, and plans the next poll. A poll
// with a read from before a job ended reads again at once.
func (m *model) polled() tea.Cmd {
	m.polling = max(m.polling-1, 0)
	if m.polling > 0 {
		return nil
	}
	if m.stale {
		m.stale = false
		return m.poll()
	}
	if m.tickPlanned {
		return nil
	}
	m.tickPlanned = true
	return pollTick()
}

// onStatus keeps what a read of the services and the cluster found,
// unless a job ended since it started.
func (m *model) onStatus(msg statusMsg) {
	if !m.state.Reads.Current(msg.mark) {
		m.stale = true
		return
	}
	m.state.Reads.Set(msg.status)
}

// onPods keeps a read of the pods, unless a job ended since it began.
func (m *model) onPods(msg podsMsg) {
	if !m.state.Reads.Current(msg.mark) {
		m.stale = true
		return
	}
	m.state.SetPods(msg.rows, msg.agents, msg.err)
	m.syncAgents()
	if len(m.state.SelectedPods()) > 0 {
		m.startTraffic()
	}
	// The log pane keeps an action's log until the operator moves away
	// from it, so that its outcome stays on screen.
	if m.ui.Logs.Source() != state.SourceAction {
		m.followFront()
	}
}

// syncAgents follows the selected agent's pod. When that pod changed, every
// tab moves to it.
func (m *model) syncAgents() {
	agent, _ := m.state.Selected()
	if agent.Pod != m.shownPod {
		m.shownPod = agent.Pod
		m.agentSwitched()
	}
}

// agentSwitched points the screens at the agent selected now: its log,
// its traffic, and no request yet.
func (m *model) agentSwitched() {
	m.agents.Reset()
	m.stopStream()
	m.startTraffic()
	m.followFront()
	m.layout()
}

func (m *model) stopStream() {
	if m.streamStop != nil {
		m.streamStop()
	}
	m.stream, m.streamStop, m.streamFinished = "", nil, false
}

// followContainer follows a container's log until ctx ends. It follows it
// again when kubectl stops, as it does when the container restarts, and
// keeps the lines it has until kubectl writes new ones. A container that
// finished, such as the init container, is read once.
func followContainer(ctx context.Context, session *sandbox.Session, pod, container string, finished bool,
	log *component.Log,
) {
	defer recoverInto(log)
	log.Reset()
	for again := false; ctx.Err() == nil; again = true {
		cmd, err := session.LogsCommand(ctx, pod, container, logTail, !finished)
		if err != nil {
			log.Add("✕ " + err.Error())
			return
		}
		out := &attemptLog{log: log, again: again}
		cmd.Stderr = out
		stdout, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err == nil {
			_ = component.ReadLines(stdout, func(line string) { out.Add(proxylog.ReadableLine(line)) })
			err = cmd.Wait()
		}
		if ctx.Err() != nil {
			return
		}
		if finished {
			log.Add("", "── the container has finished ──")
			return
		}
		if err != nil {
			log.Add(fmt.Sprintf("… kubectl logs stopped (%v). Following again in %s.", err, streamRetry))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(streamRetry):
		}
	}
}

// attemptLog is what one run of kubectl logs writes to log. When the run
// follows another, log drops the earlier run's lines once this run writes,
// since kubectl starts again from the tail.
type attemptLog struct {
	log   *component.Log
	again bool
	reset sync.Once
}

func (a *attemptLog) Write(data []byte) (int, error) {
	a.start()
	return a.log.Write(data) //nolint:wrapcheck // Log.Write does not fail.
}

// Add adds whole lines to the log.
func (a *attemptLog) Add(lines ...string) {
	a.start()
	a.log.Add(lines...)
}

// start drops the earlier run's lines, the first time this run writes.
func (a *attemptLog) start() {
	a.reset.Do(func() {
		if a.again {
			a.log.Reset()
		}
	})
}

// startTraffic follows the proxy of the selected agent's pod, if it has one.
func (m *model) startTraffic() {
	target, err := m.session.Target()
	if err != nil || target.Pod == "" {
		m.stopTraffic()
		return
	}
	if _, pod := m.state.Agents.Traffic.Snapshot(); pod == target.Pod && m.trafficStop != nil {
		return
	}
	if m.trafficStop != nil {
		m.trafficStop()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.trafficStop = cancel
	m.state.Agents.Traffic.Reset(target.Pod)
	feeds := [2]*component.Log{m.ui.Logs.Log(state.SourceTraffic), m.ui.Logs.Log(state.SourceTrafficAll)}
	session, store, pod := m.session, &m.state.Agents.Traffic, target.Pod
	m.jobs.Go(func() { followTraffic(ctx, session, pod, feeds, store) })
}

// stopTraffic stops following the proxy's log, and forgets its records and
// rows, as for an agent without a pod, or once everything stopped.
func (m *model) stopTraffic() {
	if m.trafficStop != nil {
		m.trafficStop()
		m.trafficStop = nil
	}
	m.state.Agents.Traffic.Reset("")
	m.ui.Logs.Log(state.SourceTraffic).Reset()
	m.ui.Logs.Log(state.SourceTrafficAll).Reset()
}

// followTraffic follows the proxy's log in pod until ctx ends. Each audit
// record goes to store, and as a row to the feeds: requests to the first,
// and every record to the second. Other log lines are left out. What
// kubectl says on its error output goes to both feeds. When kubectl stops,
// or cannot start, store keeps why, the feeds say it, and followTraffic
// follows the log again streamRetry later.
func followTraffic(ctx context.Context, session *sandbox.Session, pod string, feeds [2]*component.Log,
	store *proxylog.Store,
) {
	both := io.MultiWriter(feeds[0], feeds[1])
	for ctx.Err() == nil {
		feeds[0].Reset()
		feeds[1].Reset()
		store.Reset(pod)
		err := streamRecords(ctx, session, pod, feeds, store)
		if ctx.Err() != nil {
			return
		}
		store.SetErr(err)
		if err != nil {
			_, _ = fmt.Fprintf(both, "… %v. Following again in %s.\n", err, streamRetry)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(streamRetry):
		}
	}
}

// streamRecords runs kubectl logs once, and reads the proxy's audit
// records into store and the feeds. What kubectl writes to standard error
// goes to both feeds. It returns why kubectl stopped, with the first line
// it wrote to standard error.
func streamRecords(ctx context.Context, session *sandbox.Session, pod string, feeds [2]*component.Log,
	store *proxylog.Store,
) error {
	cmd, err := session.LogsCommand(ctx, pod, state.ProxyContainer, trafficTail, true)
	if err != nil {
		return fmt.Errorf("following the proxy's log: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(feeds[0], feeds[1], &stderr)
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return fmt.Errorf("following the proxy's log: %w", err)
	}
	readErr := readRecords(stdout, pod, feeds, store)
	if err = cmd.Wait(); err != nil {
		if text := strings.TrimSpace(stderr.String()); text != "" {
			return fmt.Errorf("kubectl logs: %w: %s", err, component.FirstLine(text))
		}
		return fmt.Errorf("kubectl logs: %w", err)
	}
	return readErr
}

// readRecords reads the audit records of pod's proxy from stdout into
// store and the feeds. The first line it reads clears the store's error.
func readRecords(stdout io.Reader, pod string, feeds [2]*component.Log, store *proxylog.Store) error {
	reading := false
	err := component.ReadLines(stdout, func(line string) {
		if !reading {
			reading = true
			store.SetErr(nil)
		}
		record, ok := proxylog.Parse([]byte(line))
		if !ok {
			return
		}
		// The feeds take only what the store keeps: a follower that stopped
		// can still read a line after the store moved to another pod.
		if !store.Add(pod, record) {
			return
		}
		row := record.Row()
		// The feed of requests leaves out the routine lookups, and keeps a
		// refused lookup of a host on the internet: the proxy blocks a host
		// it does not allow there, before any connection.
		if !record.RoutineLookup() {
			feeds[0].Add(row)
		}
		feeds[1].Add(row)
	})
	if err != nil {
		return fmt.Errorf("reading the proxy's log: %w", err)
	}
	return nil
}

// startSetup runs the wizard for the sections in the background. Then its
// goroutine reads the settings again, since that reads the keychain.
func (m *model) startSetup(preset map[string]string, ids ...string) {
	ctx, cancel := context.WithCancel(m.ctx)
	if !m.state.Run.StartJob(state.Job{Kind: state.JobSetup}, cancel) {
		cancel()
		return
	}
	m.state.Setup.Started()
	log := m.ui.Logs.Log(state.SourceSetup)
	log.Reset()
	ask := m.startQuestions(preset, log)
	session := m.session
	m.state.Record("▸", "Setup started")
	done := func(result sandbox.InitResult, err error) tea.Msg {
		return setupDoneMsg{result: result, err: err, describe: session.Describe(), problems: session.Problems()}
	}
	m.inBackground(func() tea.Msg { return done(session.Setup(ctx, ask, log, ids...)) },
		func(err error) tea.Msg { return done(sandbox.InitResult{}, err) })
}

// onSetupDone takes the new settings and shows the review, also when saving
// failed. The policy-set answer holds until a setup looks at the zone again.
func (m *model) onSetupDone(msg setupDoneMsg) {
	m.state.Run.CancelJob()
	ended := m.state.EndJob()
	if m.overlay.kind == overlayQuestion {
		m.overlay = overlay{}
	}
	m.endQuestions()
	m.state.Setup.SetSettings(msg.describe, msg.problems)
	if ended.StopAfter {
		m.screen = screenDashboard
		m.stopEverything(ended.QuitAfter)
		return
	}
	if msg.err != nil && !errors.Is(msg.err, sandbox.ErrNotSaved) {
		m.setup.Abandon()
		m.screen = m.wizardBack
		if errors.Is(msg.err, sandbox.ErrInterrupted) || errors.Is(msg.err, context.Canceled) {
			m.state.Record("•", "Setup stopped without saving")
		} else {
			m.state.Setup.Failed(msg.err)
			m.state.Record("✕", "Setup failed: "+component.FirstLine(msg.err.Error()))
			m.ui.Notify("Setup stopped: "+component.FirstLine(msg.err.Error()), true)
		}
		return
	}
	if m.setup.Asked(sandbox.SectionZone) || m.setup.Asked(sandbox.SectionPolicy) {
		m.session.ReplaceActive = msg.result.ReplaceActive
	}
	m.setup.Finish()
	m.state.ResetSteps()
	m.recordSaved(msg)
	m.screen = screenReview
}

// recordSaved says whether setup saved the settings. When saving failed,
// setup kept them for the session, and the activity says why.
func (m *model) recordSaved(msg setupDoneMsg) {
	switch {
	case msg.err != nil:
		m.state.Record("✕", component.FirstLine(msg.err.Error()))
		m.ui.Notify("The settings are not saved. They last until you quit.", true)
	case msg.result.Saved:
		m.state.Record("✓", "Saved the settings")
	default:
		m.state.Record("✓", "Kept the settings for this session")
	}
}

// stopSetup ends the wizard without writing the settings. The setup's job
// ends once its work reports back.
func (m *model) stopSetup() {
	if !m.state.Run.Job.Is(state.JobSetup) || !m.asking() {
		return
	}
	m.stopQuestions()
	m.state.Run.CancelJob()
	if m.overlay.kind == overlayQuestion {
		m.overlay = overlay{}
	}
}

// addAgent asks who gets a new agent, and the demo's questions for them, as
// setup does. Once the settings are saved, the new agent comes up when the
// sandbox is up.
func (m *model) addAgent() {
	if m.busy() {
		return
	}
	if !m.session.Local().Initialized() {
		m.ui.Notify("Set the sandbox up first: press e.", true)
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	if !m.state.Run.StartJob(state.Job{Kind: state.JobSetup, Name: "Adding an agent"}, cancel) {
		cancel()
		return
	}
	m.openTab(tabAgent)
	m.askTitle = newAgentTitle
	log := m.ui.Logs.Log(state.SourceSetup)
	log.Reset()
	m.setup.ForgetNotes()
	ask := m.startQuestions(nil, log)
	session := m.session
	m.state.Record("▸", "Adding an agent")
	m.ui.Notify("Signing in to Keycard to list the zone's users…", false)
	// The settings are read again after the questions, also after a panic.
	done := func(email string, err error) tea.Msg {
		return agentAddedMsg{
			email: email, err: err, agents: session.Agents(), describe: session.Describe(), problems: session.Problems(),
		}
	}
	m.inBackground(func() tea.Msg { return done(session.AddAgent(ctx, ask, log)) },
		func(err error) tea.Msg { return done("", err) })
}

// onAgentAdded selects the new agent, and brings it up when the sandbox is
// up. Otherwise it comes up with the next bring-up. When saving the
// settings failed, it says the agent is for this session only. When D
// stopped the questions, everything stops now.
func (m *model) onAgentAdded(msg agentAddedMsg) {
	m.state.Run.CancelJob()
	ended := m.state.EndJob()
	m.askTitle = ""
	if m.overlay.kind == overlayQuestion {
		m.overlay = overlay{}
	}
	m.endQuestions()
	m.state.Setup.SetSettings(msg.describe, msg.problems)
	if ended.StopAfter {
		m.stopEverything(ended.QuitAfter)
		return
	}
	switch {
	case errors.Is(msg.err, sandbox.ErrInterrupted), errors.Is(msg.err, context.Canceled):
		m.state.Record("•", "Stopped adding an agent")
		return
	case msg.err != nil && !errors.Is(msg.err, sandbox.ErrNotSaved):
		m.ui.Notify("Adding an agent failed: "+component.FirstLine(msg.err.Error()), true)
		m.state.Record("✕", "Adding an agent failed: "+component.FirstLine(msg.err.Error()))
		return
	}
	m.state.SetAgents(msg.agents)
	m.session.SelectAgent(msg.email)
	m.syncAgents()
	m.state.Record("✓", "Gave "+msg.email+" an agent in this demo")
	if msg.err != nil {
		notSaved := msg.email + " has an agent for this session only: " + component.FirstLine(msg.err.Error())
		m.state.Record("!", notSaved)
		m.ui.Notify(notSaved, true)
	}
	if !m.state.Up() {
		m.ui.Notify(msg.email+" has an agent in this demo now. It comes up with the sandbox: press u.", false)
		return
	}
	session, email := m.session, msg.email
	job := state.Job{Kind: state.JobAction, Name: page.DeployTitle(email), Agent: email}
	m.runAction(job, func(ctx context.Context, out io.Writer) error {
		return session.DeployAgent(ctx, email, out)
	})
}

// removeAgent asks, then takes the selected agent away: out of the
// settings, and out of the cluster and the zone's rules when it runs.
func (m *model) removeAgent() {
	agent, index := m.state.Selected()
	switch {
	case index < 0:
		return
	case m.busy():
		return
	case agent.Default:
		m.ui.Notify("Every demo has an agent for "+agent.Email+". To remove it, press e and pick someone else "+
			"in Zone and user.", true)
		return
	}
	body := "This deletes their sandbox and its pod, and turns off their access rules in the\n" +
		"zone. The other agents keep running."
	switch {
	case agent.Pod == "":
		body = "They come off the settings. Their agent never came up, so nothing else changes."
	case !m.state.Reads.Status.ClusterExists:
		body = "They come off the settings. Nothing of theirs runs, and the next bring-up turns off\n" +
			"their access rules in the zone."
	}
	m.confirm("Remove "+agent.Email+"'s agent?", body, removeAgentMsg{email: agent.Email})
}

// removeAgentNow takes email's agent away, while nothing else runs.
func (m *model) removeAgentNow(email string) {
	if m.busy() {
		return
	}
	session, bus := m.session, m.bus
	m.stopStream()
	job := state.Job{Kind: state.JobAction, Name: "Removing " + email + "'s agent"}
	m.runAction(job, func(ctx context.Context, out io.Writer) error {
		err := session.RemoveAgent(ctx, email, out)
		bus.Send(settingsMsg{describe: session.Describe(), problems: session.Problems()})
		gone := !slices.ContainsFunc(session.Agents(), func(a sandbox.Agent) bool { return a.Email == email })
		if err != nil && gone {
			_, _ = fmt.Fprintln(out, "Their access rules stay on in the zone until the next bring-up turns "+
				"them off: press u.")
		}
		return err //nolint:wrapcheck // RemoveAgent's errors say what failed, as the steps' do.
	})
}

// sendRequest sends the request the Agents tab asked for from the agent's
// container, and reports what came back.
func (m *model) sendRequest(msg page.SendRequestMsg) tea.Cmd {
	m.state.Record("▸", msg.Agent+"'s agent sent "+msg.Method+" "+msg.URL)
	session, base := m.session, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, requestTimeout)
		defer cancel()
		response, err := session.AgentRequest(ctx, msg.Method, msg.URL, msg.Body)
		return page.RequestDoneMsg{
			Agent: msg.Agent, Seq: msg.Seq, Method: msg.Method, URL: msg.URL, Response: response, Err: err,
		}
	}
}

// onRequestDone records what came back from a request the Agents tab
// asked for, and shows it there.
func (m *model) onRequestDone(msg page.RequestDoneMsg) {
	sent := msg.Agent + "'s agent: " + msg.Method + " " + msg.URL
	switch {
	case msg.Err != nil:
		m.state.Record("✕", sent+": "+component.FirstLine(msg.Err.Error()))
	case msg.Response.Failure != "":
		m.state.Record("✕", sent+": "+msg.Response.Failure)
	default:
		mark := "✓"
		if msg.Response.Status >= http.StatusBadRequest {
			mark = "✕"
		}
		m.state.Record(mark, sent+": "+strconv.Itoa(msg.Response.Status)+" "+http.StatusText(msg.Response.Status))
	}
	m.agents.OnRequestDone(msg)
}

// openEvents opens the zone's event log in the console, where the zone
// records each credential it issues to the proxy.
func (m *model) openEvents() tea.Cmd {
	m.ui.Notify("Finding the zone in the Keycard console…", false)
	base, session := m.ctx, m.session
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
	switch {
	case msg.url == "":
		m.ui.Notify("Cannot find the zone's events: "+component.FirstLine(msg.err.Error()), true)
		m.state.Record("✕", "Could not find the zone's events: "+component.FirstLine(msg.err.Error()))
	case msg.err != nil:
		m.ui.Notify("Open "+msg.url, false)
		m.state.Record("•", "The zone's events are at "+msg.url)
	default:
		m.ui.Notify("Opened the zone's events in the browser.", false)
		m.state.Record("✓", "Opened the zone's events: "+msg.url)
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
// scenario's command, such as pi, or sh when plain is set.
func (m *model) openAgent(plain bool) tea.Cmd {
	if m.busy() || m.isDown() {
		return nil
	}
	cmd, err := m.session.ShellCommand(m.ctx, plain)
	if err != nil {
		m.ui.Notify("Cannot open the agent: "+component.FirstLine(err.Error()), true)
		return nil
	}
	title, leave := "sh in "+page.AgentName(m.state), "Type exit to come back to "+style.AppName+"."
	if scenario := m.state.LoadedDemo(); scenario != nil && !plain && len(scenario.Run.Command) > 0 {
		title = scenario.Run.Command[0] + " in " + page.AgentName(m.state)
		leave = "Quit " + scenario.Run.Command[0] + " to come back to " + style.AppName + "."
	}
	m.state.Record("▸", "Opened "+title)
	return tea.Exec(&agentExec{cmd: cmd, title: title, leave: leave}, func(err error) tea.Msg {
		return execDoneMsg{plain: plain, err: err}
	})
}

// onExecDone records how the agent's command ended, and polls again.
func (m *model) onExecDone(msg execDoneMsg) tea.Cmd {
	var exit *exec.ExitError
	switch {
	case msg.err == nil:
		m.state.Record("✓", "Came back from the agent")
	case msg.plain && errors.As(msg.err, &exit):
		// sh exits with its last command's status, such as a refused curl.
		m.state.Record("•", "Came back from the shell, which exited with status "+strconv.Itoa(exit.ExitCode()))
	default:
		m.state.Record("✕", "The agent's command ended: "+component.FirstLine(msg.err.Error()))
		m.ui.Notify("The agent's command ended: "+component.FirstLine(msg.err.Error()), true)
	}
	return tea.Batch(m.poll(), tea.RequestWindowSize)
}

// agentExec runs a command in the agent container, as a tea.ExecCommand.
// When the command fails, it waits for Enter, so the error stays readable.
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
