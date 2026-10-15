package tui

import (
	"bufio"
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
	statusMsg      struct{ status sandbox.Status }
	podsMsg        struct {
		rows   []page.ContainerRow
		agents []sandbox.Agent
		err    error
	}
	actionDoneMsg struct {
		what string
		err  error
	}
	execDoneMsg struct {
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

// errPanic is a background job that panicked. The TUI reports it and keeps
// running.
var errPanic = errors.New("internal error")

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

// startBringUp runs the steps from index from on, one after another, in the
// background. With recreate, the cluster step deletes the cluster and
// creates it again. A failed step stops the run, except the checks: the
// sandbox is up even when a check fails.
func (m *model) startBringUp(from int, recreate bool) {
	shared := m.shared
	if shared.Running || m.acting != "" || m.asking || from >= len(shared.Steps) {
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	shared.Running, m.cancel, shared.RunFrom = true, cancel, from
	shared.Launched = shared.CurrentDemo()
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
		run.out.Send(stepDoneMsg{index: i, err: err})
		if err != nil && row.Step.ID != sandbox.StepChecks {
			run.out.Send(bringUpDoneMsg{err: err})
			return
		}
	}
	run.out.Send(bringUpDoneMsg{})
}

// trackStep follows step i: on Home, and in the Health tab's log.
func (m *model) trackStep(i int) {
	m.shared.StepCursor = i
	if m.tab == tabHealth {
		m.health.Track(i)
	}
}

func (m *model) onStepStart(msg stepStartMsg) {
	row := m.shared.Steps[msg.index]
	row.State, row.Started, row.Ended = page.StepRunning, time.Now(), time.Time{}
	m.trackStep(msg.index)
}

// onStepDone marks the step, and offers to recreate a cluster that was made
// for another tunnel URL.
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
	if row.Step.ID != sandbox.StepChecks {
		shared.Record("✕", row.Step.Title+": "+component.FirstLine(msg.err.Error()))
	}
	row.State = page.StepFailed
	row.Log.Add("", "✕ "+msg.err.Error())
	if hint := page.StepHint(row, msg.err); hint != "" {
		row.Log.Add("→ " + hint)
	}
	if errors.Is(msg.err, sandbox.ErrOtherIssuer) || errors.Is(msg.err, sandbox.ErrBrokenCluster) {
		index := msg.index
		m.confirm("Recreate the cluster?", recreateReason(msg.err), func() tea.Cmd {
			m.startBringUp(index, true)
			return nil
		})
	}
}

// recordChecks counts the checks that passed, and records how they went,
// or that they did not run.
func (m *model) recordChecks(row *page.StepRow, err error) {
	row.Passed, row.Total = countChecks(row.Log)
	switch {
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
	why := "The cluster was made for an older tunnel URL, and a cluster keeps its issuer for life.\n" +
		"Delete it and create it again for the new URL?"
	if errors.Is(err, sandbox.ErrBrokenCluster) {
		why = "Creating the cluster stopped partway, so kubectl cannot reach it.\n" +
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
	shared.Running, m.cancel = false, nil
	switch {
	case msg.err != nil:
		if !errors.Is(msg.err, sandbox.ErrOtherIssuer) && !errors.Is(msg.err, sandbox.ErrBrokenCluster) &&
			!errors.Is(msg.err, context.Canceled) {
			shared.Notify("The bring-up stopped: "+component.FirstLine(msg.err.Error()), true)
		}
		shared.Record("✕", "Bring-up stopped")
	case shared.Steps[shared.RunFrom].Step.ID == sandbox.StepChecks:
	default:
		shared.Record("✓", "The sandbox is up")
		m.notifyUp()
	}
	if m.downAfterBringUp {
		m.downAfterBringUp = false
		m.down()
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
		shared.Notify("The sandbox is up, and the agent runs in the background. Its log is on the Agents tab.", false)
	case scenario.Run.Mode == sandbox.ModeShell && len(scenario.Run.Command) > 0:
		shared.Notify("The sandbox is up. Press a to open "+scenario.Run.Command[0]+", or s to send a request.", false)
	default:
		shared.Notify("The sandbox is up. Press s to send a request from the agent.", false)
	}
}

// stop cancels a running bring-up.
func (m *model) stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

// watchBringUp shows a bring-up that starts: on Home, unless the Health
// tab shows, which follows each step's log.
func (m *model) watchBringUp() {
	m.health.Focus = page.PaneChecks
	if m.tab != tabHealth {
		m.openTab(tabHome)
	}
}

func (m *model) toggleBringUp() {
	if !m.shared.Running {
		if m.busy() {
			return
		}
		if len(m.shared.Problems) > 0 {
			m.shared.Notify("Fix the settings first. Press e to open them.", true)
			return
		}
		from := m.shared.ResumeIndex()
		if from > 0 {
			m.shared.Notify("Starting again at: "+m.shared.Steps[from].Step.Title, false)
		}
		m.watchBringUp()
		m.startBringUp(from, false)
		return
	}
	m.confirm("Stop the bring-up?", "The step that runs now is canceled.\n"+
		"Press u again later to start again from that step.", func() tea.Cmd {
		m.stop()
		return nil
	})
}

// runChecks runs the checks step again, against the running sandbox.
func (m *model) runChecks() {
	if m.busy() {
		return
	}
	if m.shared.Read && !m.shared.SandboxUp() {
		m.shared.Notify("The sandbox is down. Press u to bring it up.", true)
		return
	}
	for i, row := range m.shared.Steps {
		if row.Step.ID == sandbox.StepChecks {
			m.watchBringUp()
			m.startBringUp(i, false)
		}
	}
}

// agent opens the scenario's command in the agent, such as pi, or shows the
// agent's log for an agent that runs in the background.
func (m *model) agent() tea.Cmd {
	scenario := m.shared.Scenario()
	if scenario == nil {
		_, err := m.shared.Session.Scenario()
		m.shared.Notify(component.FirstLine(err.Error()), true)
		return nil
	}
	if scenario.Run.Mode == sandbox.ModeBackground {
		m.openTab(tabAgent)
		m.agents.FocusLog()
		return nil
	}
	return m.openAgent(false)
}

func (m *model) restartAgent() {
	if m.busy() {
		return
	}
	name, session := m.shared.AgentName(), m.shared.Session
	m.confirm("Restart "+name+"'s pod?", "This deletes the pod, and the controller creates it again\n"+
		"with the current images and settings. The agent loses what it kept in the pod.",
		func() tea.Cmd {
			m.stopStream()
			m.agents.Reset()
			m.runAction("Restarting "+name+"'s pod", session.RestartAgent)
			return nil
		})
}

// stopAll asks, then stops everything. A running bring-up stops first.
// On an existing cluster, the TUI did not start anything that D stops.
func (m *model) stopAll() {
	if !m.shared.Running && m.busy() {
		return
	}
	if m.shared.Session.Local().Existing() {
		m.shared.Notify("On an existing cluster, there is nothing for D to stop.", false)
		return
	}
	m.confirm("Stop everything?", "This stops the tunnel and the mirror and deletes the k3d cluster.\n"+
		"What the sandbox created in your Keycard zone stays.", func() tea.Cmd {
		m.down()
		return nil
	})
}

// down stops the tunnel and the mirror and deletes the cluster. A running
// bring-up stops first, and onBringUpDone calls down again once it has.
func (m *model) down() {
	m.stopStream()
	if m.shared.Running {
		m.stop()
		m.downAfterBringUp = true
		m.shared.Notify("Stopping the bring-up first…", false)
		return
	}
	m.runAction("Stopping everything", m.shared.Session.Down)
}

// runAction runs work in the background with its output in the action log,
// which the log pane shows, and reports when it is done. It is the one
// action that runs until then.
func (m *model) runAction(what string, work func(ctx context.Context, out io.Writer) error) {
	shared := m.shared
	log := shared.Logs.Log(page.SourceAction)
	log.Reset()
	log.Add("$ " + what)
	shared.Record("▸", what)
	shared.Logs.Show(page.SourceAction)
	shared.Notify(what+"…", false)
	m.acting = what
	ctx, out := m.ctx, m.bus
	m.jobs.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				out.Send(actionDoneMsg{what: what, err: fmt.Errorf("%w: %v", errPanic, r)})
			}
		}()
		out.Send(actionDoneMsg{what: what, err: work(ctx, log)})
	})
}

func (m *model) onActionDone(msg actionDoneMsg) tea.Cmd {
	shared := m.shared
	m.acting = ""
	log := shared.Logs.Log(page.SourceAction)
	if msg.err != nil {
		log.Add("", "✕ "+msg.err.Error())
		if hint := page.HintFor(msg.err); hint != "" {
			log.Add("→ " + hint)
		}
		shared.Notify(msg.what+" failed: "+component.FirstLine(msg.err.Error()), true)
		shared.Record("✕", msg.what+" failed: "+component.FirstLine(msg.err.Error()))
	} else {
		log.Add("", "✓ done")
		shared.Notify(msg.what+": done", false)
		shared.Record("✓", msg.what+": done")
	}
	// A quit that waited for the action does not hide its failure.
	if m.quitAfterAction {
		m.quitAfterAction = false
		if msg.err == nil {
			return tea.Quit
		}
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
	base, session := m.ctx, m.shared.Session
	status := func() tea.Msg {
		ctx, cancel := context.WithTimeout(base, readTimeout)
		defer cancel()
		return statusMsg{status: session.Status(ctx)}
	}
	pods := func() tea.Msg {
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
	switch {
	// The log pane keeps an action's log until the operator moves away
	// from it, so that its outcome stays on screen.
	case m.stream != "" || len(shared.Pods) == 0, shared.Logs.Source() == page.SourceAction:
	case m.tab == tabAgent:
		if row, ok := shared.Container(page.AgentContainer); ok {
			m.Follow(row)
		}
	case m.tab == tabHealth:
		m.health.OnPods()
	}
}

// syncAgents keeps the selection on an agent in the list, and shows the
// selected agent's pod alone. When that pod changed, every tab moves to it.
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

// agentSwitched points the tabs at the agent selected now: its log, its
// traffic, and no request yet.
func (m *model) agentSwitched() {
	m.agents.Reset()
	m.stopStream()
	m.startTraffic()
	switch m.tab {
	case tabAgent:
		m.agents.ShowRight()
	case tabHealth:
		if m.health.Focus == page.PaneChecks {
			m.health.ShowCheck()
		}
	case tabHome, tabTraffic, tabCount:
	}
	m.layout()
}

func (m *model) stopStream() {
	if m.streamStop != nil {
		m.streamStop()
	}
	m.stream, m.streamStop = "", nil
}

// followContainer follows a container's log until ctx ends. It follows it
// again when kubectl stops, as it does when the container restarts. A
// container that finished, such as the init container, is read once.
func followContainer(ctx context.Context, session *sandbox.Session, pod, container string, finished bool,
	log *component.Log,
) {
	defer recoverInto(log)
	log.Reset()
	for ctx.Err() == nil {
		cmd, err := session.LogsCommand(ctx, pod, container, logTail, !finished)
		if err != nil {
			log.Add("✕ " + err.Error())
			return
		}
		cmd.Stderr = log
		stdout, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err == nil {
			_ = component.ReadLines(stdout, func(line string) { log.Add(proxylog.ReadableLine(line)) })
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
		log.Reset()
	}
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
		shared.Logs.Log(page.SourceTraffic).Reset()
		shared.Logs.Log(page.SourceTrafficAll).Reset()
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
	feeds := [2]*component.Log{shared.Logs.Log(page.SourceTraffic), shared.Logs.Log(page.SourceTrafficAll)}
	session, store, pod := shared.Session, &shared.Traffic, target.Pod
	m.jobs.Go(func() { followTraffic(ctx, session, pod, feeds, store) })
}

// followTraffic follows the proxy's log in pod until ctx ends. Each audit
// record goes to store, and as a row to the feeds: requests to the first,
// and every record to the second. Other log lines are left out. What
// kubectl says on its error output, and why it stopped, go to both feeds.
func followTraffic(ctx context.Context, session *sandbox.Session, pod string, feeds [2]*component.Log,
	store *proxylog.Store,
) {
	both := io.MultiWriter(feeds[0], feeds[1])
	for ctx.Err() == nil {
		feeds[0].Reset()
		feeds[1].Reset()
		store.Reset(pod)
		cmd, err := session.LogsCommand(ctx, pod, page.ProxyContainer, trafficTail, true)
		if err != nil {
			_, _ = fmt.Fprintf(both, "✕ %v\n", err)
			return
		}
		cmd.Stderr = both
		stdout, err := cmd.StdoutPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err == nil {
			readRecords(stdout, feeds, store)
			err = cmd.Wait()
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			_, _ = fmt.Fprintf(both, "… kubectl logs stopped (%v). Following again in %s.\n", err, streamRetry)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(streamRetry):
		}
	}
}

// readRecords reads the proxy's audit records from stdout into store and
// the feeds.
func readRecords(stdout io.Reader, feeds [2]*component.Log, store *proxylog.Store) {
	_ = component.ReadLines(stdout, func(line string) {
		record, ok := proxylog.Parse([]byte(line))
		if !ok {
			return
		}
		store.Add(record)
		row := record.Row()
		// The feed of requests leaves out the routine lookups, and keeps a
		// refused lookup of a host on the internet: the proxy blocks a host
		// it does not allow there, before any connection.
		if !record.RoutineLookup() {
			feeds[0].Add(row)
		}
		feeds[1].Add(row)
	})
}

// startSetup runs the wizard for the sections in the background, with
// preset's answers. Its questions come back as page.AskMsg, its steps as
// page.SectionMsg, and its notes go to the setup log. Once it is done, the
// same goroutine reads the settings again, since that reads the keychain.
func (m *model) startSetup(preset map[string]string, ids ...string) {
	shared := m.shared
	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(m.ctx)
	m.asking, m.stopAsking, m.cancelSetup = true, stop, cancel
	shared.SetupErr = nil
	log := shared.Logs.Log(page.SourceSetup)
	log.Reset()
	local := *shared.Session.Local()
	local.Out = log
	local.Ask = page.Asker{Send: m.bus.Send, Done: stop, Preset: preset, Log: log}
	session, out := shared.Session, m.bus
	shared.Record("▸", "Setup started")
	m.jobs.Go(func() {
		var done setupDoneMsg
		defer func() {
			if r := recover(); r != nil {
				done.err = fmt.Errorf("%w: %v", errPanic, r)
			}
			done.describe, done.problems = session.Describe(), session.Problems()
			out.Send(done)
		}()
		done.result, done.err = local.InitSections(ctx, ids...)
		if done.err == nil {
			session.Reload()
		}
	})
}

// onSetupDone takes the new settings and shows the review. A setup that
// stopped goes back where it started. The answer about the zone's policy
// set holds until a setup looks at the zone again.
func (m *model) onSetupDone(msg setupDoneMsg) {
	shared := m.shared
	m.asking, m.stopAsking = false, nil
	if m.cancelSetup != nil {
		m.cancelSetup()
	}
	if m.overlay.kind == overlayQuestion {
		m.overlay = overlay{}
	}
	m.setup.Drop()
	shared.Describe, shared.Problems = msg.describe, msg.problems
	if msg.err != nil {
		m.screen = m.wizardBack
		if errors.Is(msg.err, sandbox.ErrInterrupted) || errors.Is(msg.err, context.Canceled) {
			shared.Record("•", "Setup stopped without saving")
		} else {
			shared.SetupErr = msg.err
			shared.Record("✕", "Setup failed: "+component.FirstLine(msg.err.Error()))
			shared.Notify("Setup stopped: "+component.FirstLine(msg.err.Error()), true)
		}
		return
	}
	if m.setup.Asked(sandbox.SectionZone) || m.setup.Asked(sandbox.SectionPolicy) {
		shared.Session.ReplaceActive = msg.result.ReplaceActive
	}
	m.setup.Finish()
	shared.ResetSteps()
	if msg.result.Saved {
		shared.Record("✓", "Saved the settings")
	} else {
		shared.Record("✓", "Kept the settings for this session")
	}
	m.screen = screenReview
}

// stopSetup ends the wizard without writing the settings: the question
// goes, and what the setup waits on stops.
func (m *model) stopSetup() {
	if !m.asking || m.stopAsking == nil {
		return
	}
	close(m.stopAsking)
	m.stopAsking = nil
	if m.cancelSetup != nil {
		m.cancelSetup()
	}
	m.setup.Drop()
	if m.overlay.kind == overlayQuestion {
		m.overlay = overlay{}
	}
}

// addAgent asks who gets a new agent, and the demo's questions for them, as
// setup does. Once the settings are saved, the new agent comes up when the
// sandbox is up.
func (m *model) addAgent() {
	shared := m.shared
	if m.busy() {
		return
	}
	if !shared.Session.Local().Initialized() {
		shared.Notify("Set the sandbox up first: press e.", true)
		return
	}
	m.openTab(tabAgent)
	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(m.ctx)
	m.asking, m.stopAsking, m.cancelSetup, m.askTitle = true, stop, cancel, newAgentTitle
	log := shared.Logs.Log(page.SourceSetup)
	log.Reset()
	m.setup.ForgetNotes()
	local := *shared.Session.Local()
	local.Out = log
	local.Ask = page.Asker{Send: m.bus.Send, Done: stop, Log: log}
	session, out := shared.Session, m.bus
	shared.Record("▸", "Adding an agent")
	shared.Notify("Signing in to Keycard to list the zone's users…", false)
	m.jobs.Go(func() {
		var done agentAddedMsg
		defer func() {
			if r := recover(); r != nil {
				done.err = fmt.Errorf("%w: %v", errPanic, r)
			}
			done.describe, done.problems = session.Describe(), session.Problems()
			out.Send(done)
		}()
		done.email, done.err = local.AddAgent(ctx)
		if done.err == nil {
			session.Reload()
			done.agents = session.Agents()
		}
	})
}

// onAgentAdded selects the new agent, and brings it up when the sandbox is
// up. Otherwise it comes up with the next bring-up.
func (m *model) onAgentAdded(msg agentAddedMsg) {
	shared := m.shared
	m.asking, m.stopAsking, m.askTitle = false, nil, ""
	if m.cancelSetup != nil {
		m.cancelSetup()
	}
	if m.overlay.kind == overlayQuestion {
		m.overlay = overlay{}
	}
	m.setup.Drop()
	shared.Describe, shared.Problems = msg.describe, msg.problems
	switch {
	case errors.Is(msg.err, sandbox.ErrInterrupted), errors.Is(msg.err, context.Canceled):
		shared.Record("•", "Stopped adding an agent")
		return
	case msg.err != nil:
		shared.Notify("Adding an agent failed: "+component.FirstLine(msg.err.Error()), true)
		shared.Record("✕", "Adding an agent failed: "+component.FirstLine(msg.err.Error()))
		return
	}
	shared.Agents = msg.agents
	shared.Session.SelectAgent(msg.email)
	m.syncAgents()
	shared.Record("✓", "Gave "+msg.email+" an agent in this demo")
	if !shared.SandboxUp() {
		shared.Notify(msg.email+" has an agent in this demo now. It comes up with the sandbox: press u.", false)
		return
	}
	session, email := shared.Session, msg.email
	m.runAction(page.DeployTitle(email), func(ctx context.Context, out io.Writer) error {
		return session.DeployAgent(ctx, email, out)
	})
}

// removeAgent asks, then takes the selected agent away: out of the
// settings, and out of the cluster and the zone's rules when it runs.
func (m *model) removeAgent() {
	shared := m.shared
	agent, index := shared.SelectedAgent()
	switch {
	case index < 0:
		return
	case m.busy():
		return
	case agent.Default:
		shared.Notify("Every demo has an agent for "+agent.Email+". To remove it, press e and pick someone else "+
			"in Zone and user.", true)
		return
	}
	body := "This deletes their sandbox and its pod, and turns off their access rules in the\n" +
		"zone. The other agents keep running."
	switch {
	case agent.Pod == "":
		body = "They come off the settings. Their agent never came up, so nothing else changes."
	case !shared.Status.ClusterExists:
		body = "They come off the settings. Nothing of theirs runs, and the next bring-up turns off\n" +
			"their access rules in the zone."
	}
	email, session, bus := agent.Email, shared.Session, m.bus
	m.confirm("Remove "+email+"'s agent?", body, func() tea.Cmd {
		m.stopStream()
		m.runAction("Removing "+email+"'s agent", func(ctx context.Context, out io.Writer) error {
			err := session.RemoveAgent(ctx, email, out)
			bus.Send(settingsMsg{describe: session.Describe(), problems: session.Problems()})
			gone := !slices.ContainsFunc(session.Agents(), func(a sandbox.Agent) bool { return a.Email == email })
			if err != nil && gone {
				_, _ = fmt.Fprintln(out, "Their access rules stay on in the zone until the next bring-up turns "+
					"them off: press u.")
			}
			return err //nolint:wrapcheck // RemoveAgent's errors say what failed, as the steps' do.
		})
		return nil
	})
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
	if m.busy() {
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
