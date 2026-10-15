package page

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
)

const (
	// healthNameWidth is the width of a health check's name.
	healthNameWidth = 34
	// maxCaptionRows bounds the words over a log.
	maxCaptionRows = 3
	// logsChrome is the rows the logs list takes besides its logs.
	logsChrome = 10
)

// Pane is what the Health tab has focus on: the list of its checks, or the
// log of the one selected.
type Pane int

// The Health tab's panes.
const (
	PaneChecks Pane = iota
	PaneLog
)

// healthState is the state of one thing the sandbox needs.
type healthState int

const (
	// healthOff is not running yet, or not run in this session.
	healthOff healthState = iota
	// healthWait is starting.
	healthWait
	healthOK
	// healthWarn works, with something to look at.
	healthWarn
	healthBad
)

// healthCheck is one thing the sandbox needs to work: whether it does, a
// line on it, what to do when it does not, and the log behind it.
type healthCheck struct {
	name   string
	state  healthState
	detail string
	fix    string
	// caption says what the log shows. source is the log, and container
	// the pod's container to follow for it, if any.
	caption, source, container string
}

// LogEntry is one log the logs list offers: its name, the log, the pod's
// container to follow for it, and what it shows.
type LogEntry struct {
	name, source, container, caption string
}

// Health is the dashboard's tab for how the sandbox is provisioned: a
// headline on whether everything works, the checks, and the log of the one
// selected, or of one chosen from the list of every log.
type Health struct {
	shared *Shared
	host   Host
	// Focus is the pane with focus.
	Focus Pane
	// cursor is the check selected, and pick the log chosen from the list
	// of every log in its place, if any.
	cursor int
	pick   string
	logs   component.Picker
}

// NewHealth returns the Health tab.
func NewHealth(shared *Shared, host Host) *Health { return &Health{shared: shared, host: host} }

// View draws the headline and the checks over the log.
func (h *Health) View() string {
	shared := h.shared
	lines := h.top()
	width := shared.Width - component.PageIndent
	pane := shared.logPane(h.logTitle(), h.caption(width-component.BorderCols), waitingText(shared.Logs.Source()),
		width, shared.BodyRows()-len(lines), h.Focus == PaneLog)
	return strings.Join(append(lines, pane), "\n")
}

// Hints lists the tab's keys: for its checks, or for the log when it has
// focus.
func (h *Health) Hints() []component.Hint {
	shared := h.shared
	if h.Focus == PaneLog {
		return logHints()
	}
	start := "bring up"
	if shared.Running {
		start = "stop"
	} else if from := shared.ResumeIndex(); from > 0 {
		start = "retry " + string(shared.Steps[from].Step.ID)
	}
	return []component.Hint{
		{Key: "↑↓", Desc: "select"},
		{Key: component.KeyEnter, Desc: "log"},
		{Key: "l", Desc: "every log"},
		{Key: "u", Desc: start},
		{Key: "c", Desc: "checks"},
		{Key: "?", Desc: wordKeys},
		{Key: "q", Desc: wordQuit},
	}
}

// Keys moves through the checks, and scrolls the log.
func (h *Health) Keys(msg tea.KeyPressMsg) tea.Cmd {
	keys := h.shared.Keys
	switch {
	case h.Focus == PaneLog && key.Matches(msg, keys.Back) && h.shared.Logs.Filter() == "":
		h.Focus = PaneChecks
		h.ShowCheck()
	case h.Focus == PaneLog:
		return h.shared.Logs.Keys(msg, keys)
	case key.Matches(msg, keys.Up), key.Matches(msg, keys.Down):
		delta := 1
		if key.Matches(msg, keys.Up) {
			delta = -1
		}
		h.cursor = min(max(h.cursor+delta, 0), len(h.checks())-1)
		h.ShowCheck()
	case key.Matches(msg, keys.Output), key.Matches(msg, keys.NextPane):
		h.Focus = PaneLog
	case key.Matches(msg, keys.DNS):
		h.shared.ShowDNS = !h.shared.ShowDNS
		h.ShowCheck()
	}
	return nil
}

// Layout sizes the log pane under the checks and what the log shows.
func (h *Health) Layout() {
	shared := h.shared
	width := shared.Width - component.PageIndent - component.BorderCols
	height := shared.BodyRows() - len(h.top()) - len(h.caption(width))
	shared.Logs.SetSize(max(width, 1), max(height-component.BorderRows-1, 1))
}

// ShowCheck shows the log of the check under the cursor: a container's,
// followed, or another log.
func (h *Health) ShowCheck() {
	checks := h.checks()
	h.cursor = min(max(h.cursor, 0), len(checks)-1)
	h.pick = ""
	check := checks[h.cursor]
	if row, ok := h.shared.Container(check.container); ok && check.container != "" {
		h.host.Follow(row)
	} else {
		h.shared.Logs.Show(check.source)
	}
	h.Layout()
}

// OnPods shows the selected check's log again once the pod is listed,
// unless the log in its place came from the list of every log.
func (h *Health) OnPods() {
	if h.Focus == PaneChecks && h.pick == "" {
		h.ShowCheck()
	}
}

// Track shows the log of step i of a bring-up while the checks have focus.
func (h *Health) Track(i int) {
	if h.Focus != PaneChecks {
		return
	}
	step := h.shared.Steps[i].Step
	h.pick = "Bring-up: " + step.Title
	h.shared.Logs.Show(StepSource(step.ID))
	h.Layout()
}

// ShowLog shows a log from the list of every log, with focus.
func (h *Health) ShowLog(entry LogEntry) {
	h.pick, h.Focus = entry.name, PaneLog
	h.show(entry)
}

// Open points the log at what the tab shows first: the log picked from
// the list, or else the step that runs or stopped the bring-up, or else the
// selected check's log.
func (h *Health) Open() {
	shared := h.shared
	failed := slices.IndexFunc(shared.Steps, func(row *StepRow) bool { return row == shared.FailedStep() })
	switch {
	case h.Focus == PaneLog && h.pick != "":
		h.showPick()
	case h.Focus == PaneLog:
		h.ShowCheck()
	case shared.Running && shared.StepCursor < len(shared.Steps):
		h.Track(shared.StepCursor)
	case failed >= 0:
		h.Track(failed)
	default:
		h.ShowCheck()
	}
}

// LogEntries are every log the sandbox has, for looking into a problem: the
// pod's containers, the programs on this machine, each step of the
// bring-up, and what happened in this session, last.
func (h *Health) LogEntries() []LogEntry {
	entries := []LogEntry{
		{"Agent", "", AgentContainer, h.agentCaption()},
		{"Egress proxy", "", ProxyContainer, "The egress proxy's own log: every decision, with every DNS lookup."},
		{"Firewall", "", netsetupContainer, "The rules the firewall set when the pod started. They send each " +
			"DNS lookup and web request from the agent to the proxy, and drop everything else."},
	}
	if h.hasStep(sandbox.StepMirror) {
		entries = append(entries, LogEntry{
			"Mirror", ServiceSource(sandbox.ServiceMirror), "",
			"The mirror on this machine serves the cluster's public keys.",
		})
	}
	if h.hasStep(sandbox.StepTunnel) {
		entries = append(entries, LogEntry{
			"Tunnel", ServiceSource(sandbox.ServiceTunnel), "",
			"The Cloudflare tunnel puts the mirror on the internet, for Keycard.",
		})
	}
	for _, row := range h.shared.Steps {
		entries = append(entries, LogEntry{
			"Bring-up: " + row.Step.Title, StepSource(row.Step.ID), "",
			"What this step of the bring-up printed.",
		})
	}
	return append(entries,
		LogEntry{"Last action", SourceAction, "", "What the last action printed, such as a restart of the pod."},
		LogEntry{"Activity", SourceActivity, "", "What happened in this session, newest last."})
}

// OpenLogs keeps the list of every log's cursor on a log, since the list
// can be shorter than when it last showed.
func (h *Health) OpenLogs() {
	h.logs.Cursor = max(min(h.logs.Cursor, len(h.LogEntries())-1), 0)
}

// LogsKeys moves through the list of every log, and returns the log
// chosen.
func (h *Health) LogsKeys(msg tea.KeyPressMsg) (LogEntry, component.Pick) {
	entries := h.LogEntries()
	if key.Matches(msg, h.shared.Keys.Logs) {
		return LogEntry{}, component.PickClosed
	}
	pick := h.logs.Update(msg, h.shared.Keys, len(entries))
	if pick != component.PickChosen {
		return LogEntry{}, pick
	}
	return entries[h.logs.Cursor], pick
}

// LogsView lists every log.
func (h *Health) LogsView() string {
	shared := h.shared
	theme := shared.Styles
	width := component.ModalInnerWidth(shared.Width)
	entries := h.LogEntries()
	lines := make([]string, 0, len(entries))
	for i, entry := range entries {
		mark := "  "
		if i == h.logs.Cursor {
			mark = theme.Accent.Render("▸ ")
		}
		lines = append(lines, component.Selected(theme, mark+theme.Text.Render(entry.name), i == h.logs.Cursor, true,
			width))
	}
	return component.ListModal(theme, shared.Width, max(shared.Height-component.ChromeRows-logsChrome, 1),
		"Every log", "For looking into a problem. The Health tab's checks show the logs that matter most.",
		lines, h.logs.Cursor, component.Hint{Key: "↑↓", Desc: wordChoose}, component.Hint{
			Key:  component.KeyEnter,
			Desc: "show",
		}, component.Hint{Key: component.KeyEsc, Desc: "close"})
}

// show shows a log from the list: a container's, followed, or another log.
func (h *Health) show(entry LogEntry) {
	if row, ok := h.shared.Container(entry.container); ok && entry.container != "" {
		h.host.Follow(row)
	} else {
		h.shared.Logs.Show(cmp.Or(entry.source, ContainerSource("", entry.container)))
	}
	h.Layout()
}

// showPick shows the log picked from the list again, or the selected
// check's log when the list lacks it.
func (h *Health) showPick() {
	entries := h.LogEntries()
	i := slices.IndexFunc(entries, func(entry LogEntry) bool { return entry.name == h.pick })
	if i < 0 {
		h.ShowCheck()
		return
	}
	h.show(entries[i])
}

// hasStep reports whether the bring-up has the step id.
func (h *Health) hasStep(id sandbox.StepID) bool {
	return slices.ContainsFunc(h.shared.Steps, func(row *StepRow) bool { return row.Step.ID == id })
}

// checks are what the sandbox needs to work, in the order a request needs
// them: the agent runs, its requests go through Keycard, Keycard can
// verify the agent, and the checks pass.
func (h *Health) checks() []healthCheck {
	return []healthCheck{h.agentCheck(), h.keycardCheck(), h.identityCheck(), h.checksCheck()}
}

// containerCheck says how one of the pod's containers is.
func (h *Health) containerCheck(name string) healthCheck {
	check := healthCheck{container: name, source: ContainerSource("", name), detail: "No pod yet"}
	row, ok := h.shared.Container(name)
	if !ok {
		return check
	}
	ctr := row.Container
	check.source, check.detail = ContainerSource(row.Pod, name), containerDetail(ctr)
	switch {
	case ctr.Ready:
		check.state = healthOK
	case ctr.State == StateTerminated, slices.Contains(crashReasons(), ctr.Reason):
		check.state, check.fix = healthBad, "R restarts the pod. Its log is below."
	default:
		check.state = healthWait
	}
	return check
}

// agentCheck is whether the agent runs, and what it does. When the pods
// of a sandbox that was set up cannot be read, it says why.
func (h *Health) agentCheck() healthCheck {
	shared := h.shared
	check := h.containerCheck(AgentContainer)
	check.name, check.caption = "The agent runs", h.agentCaption()
	_, found := shared.Container(AgentContainer)
	agent, _ := shared.SelectedAgent()
	if row, failed := shared.failedInit(agent.Pod); failed {
		check.state, check.detail = healthBad, "The firewall did not start: "+containerDetail(row.Container)
		check.fix = "R restarts the pod. The firewall's log is below."
		check.source, check.container = ContainerSource(row.Pod, row.Container.Name), row.Container.Name
		check.caption = "The rules the firewall sets when the pod starts."
		return check
	}
	switch {
	case check.state == healthOK:
		check.detail = h.agentNote()
	case !found && shared.PodsErr != nil && shared.Status.Deployed:
		check.state = healthBad
		check.detail = "Cannot read the pods: " + component.FirstLine(shared.PodsErr.Error())
	}
	return check
}

// agentNote says what the agent does, by how the demo runs.
func (h *Health) agentNote() string {
	scenario := h.shared.Scenario()
	if scenario == nil {
		return "Running"
	}
	title := cmp.Or(scenario.Title, scenario.Name)
	switch scenario.Run.Mode {
	case sandbox.ModeShell:
		return title + ". Press a to open it"
	case sandbox.ModeBackground:
		return title + ". It answers on its own"
	}
	return title + ". The Agents tab sends requests from it"
}

// agentCaption says what the agent's log shows, by how the demo runs.
func (h *Health) agentCaption() string {
	scenario := h.shared.Scenario()
	if scenario == nil {
		return "What the agent printed."
	}
	switch scenario.Run.Mode {
	case sandbox.ModeShell:
		return "The agent's own output. It waits until you press a to open the demo. What the demo does " +
			"then shows on the Traffic tab."
	case sandbox.ModeBackground:
		return "The agent's own output. The demo runs on its own, and writes what it does here."
	}
	return "The agent's own output. It stays idle until you send it a request."
}

// keycardCheck is whether the agent's requests go through the egress
// proxy, and what Keycard did with them.
func (h *Health) keycardCheck() healthCheck {
	shared := h.shared
	check := h.containerCheck(ProxyContainer)
	check.name = "Its requests go through Keycard"
	// A proxy that fails shows its own log, which says why.
	if check.state == healthBad {
		check.caption = "The egress proxy's own log."
		return check
	}
	check.source, check.container = shared.TrafficSource(), ""
	check.caption = "Each request the agent made, and what Keycard did with it. d adds every DNS lookup."
	if check.state != healthOK {
		return check
	}
	records, _ := shared.Traffic.Snapshot()
	uses, blocked := proxylog.Summarize(records, shared.endpoints())
	requests, credentials, refused := 0, 0, 0
	for _, record := range records {
		if record.Request != nil {
			requests++
		}
	}
	for _, use := range uses {
		refused += use.Refused
		if use.Endpoint.Credential {
			credentials += use.Allowed
		}
	}
	if requests == 0 {
		check.detail = "No requests yet. s on the Agents tab sends one"
		return check
	}
	check.detail = component.Plural(requests, "request") + ", " + component.Plural(credentials, "credential") +
		" added, " + component.Plural(len(blocked), "host") + " blocked"
	if refused > 0 {
		check.state = healthWarn
		times := component.Plural(refused, "time")
		if refused == 1 {
			times = "once"
		}
		check.fix = "Keycard refused a credential " + times + ". The Traffic tab says why."
	}
	return check
}

// identityCheck is whether Keycard can verify the agent: the issuer check
// passed, the cluster runs and, for a local cluster, the mirror runs, and
// the tunnel too when the bring-up starts one.
func (h *Health) identityCheck() healthCheck {
	shared := h.shared
	check := h.identityBase()
	again := "u brings the sandbox up again."
	issuer := slices.IndexFunc(shared.Steps, func(row *StepRow) bool { return row.Step.ID == sandbox.StepIssuer })
	switch {
	case !shared.Status.ClusterExists:
		check.state, check.detail = healthOff, "No cluster yet"
		check.source = StepSource(sandbox.StepCluster)
	case issuer >= 0 && shared.Steps[issuer].State == StepFailed && shared.Steps[issuer].Err != nil:
		check.state, check.source = healthBad, StepSource(sandbox.StepIssuer)
		check.detail = component.FirstLine(shared.Steps[issuer].Err.Error())
		check.fix = "The step's log is below. u starts the bring-up again."
	case shared.Session.Local().Existing():
		check.detail, check.source = "Your cluster publishes its own keys", StepSource(sandbox.StepIssuer)
		check.caption = "Keycard checks the agent's identity with your cluster's public keys."
	case h.hasStep(sandbox.StepTunnel) && !shared.Status.Tunnel.Running:
		check.state, check.detail, check.fix = healthBad, "The tunnel is down", again
	case !shared.Status.Mirror.Running:
		check.state, check.detail, check.fix = healthBad, "The mirror is down", again
		check.source = ServiceSource(sandbox.ServiceMirror)
	}
	return check
}

// identityBase is the identity check of a local cluster that works: the
// mirror's keys reach the internet through the Cloudflare tunnel, or at
// the issuer URL the operator publishes.
func (h *Health) identityBase() healthCheck {
	check := healthCheck{
		name: "Keycard can verify the agent", state: healthOK, source: ServiceSource(sandbox.ServiceTunnel),
		detail: "The cluster's keys are on the internet, through a Cloudflare tunnel",
		caption: "Keycard checks the agent's identity with the cluster's public keys. A mirror on this " +
			"machine serves them, and this Cloudflare tunnel puts them on the internet. l lists the mirror's " +
			"log too.",
	}
	if !h.hasStep(sandbox.StepTunnel) {
		check.source = ServiceSource(sandbox.ServiceMirror)
		check.detail = "The cluster's keys are on the internet, at your issuer's URL"
		check.caption = "Keycard checks the agent's identity with the cluster's public keys. The mirror on " +
			"this machine serves them, at the issuer URL you publish."
	}
	return check
}

// checksCheck is whether the checks the bring-up ends with passed.
func (h *Health) checksCheck() healthCheck {
	shared := h.shared
	check := healthCheck{
		name: "The checks pass", source: StepSource(sandbox.StepChecks), detail: "Not run in this session",
		caption: "Requests from the agent through the proxy: with a credential, without one, and to a host " +
			"the sandbox does not allow.",
	}
	index := slices.IndexFunc(shared.Steps, func(row *StepRow) bool { return row.Step.ID == sandbox.StepChecks })
	if index < 0 {
		return check
	}
	row := shared.Steps[index]
	score := ansi.Strip(shared.stepElapsed(row))
	switch row.State {
	case StepDone:
		check.state, check.detail, check.fix = healthOK, score+". c runs them again", ""
	case StepFailed:
		check.state, check.detail, check.fix = healthBad, score, "Their log is below. c runs them again."
	case StepRunning:
		check.state, check.detail, check.fix = healthWait, "Running", ""
	case StepPending:
	}
	return check
}

// top is the headline and the checks over the log.
func (h *Health) top() []string {
	shared := h.shared
	theme := shared.Styles
	headline, about := h.headline()
	lines := append(shared.agentBar(), headline, theme.Muted.Render(ansi.Truncate(about, shared.bodyWidth(), "…")), "")
	for i, check := range h.checks() {
		chosen := i == h.cursor && h.pick == ""
		row := h.mark(check.state) + " " + theme.Text.Render(component.Pad(check.name, healthNameWidth)) +
			h.lineStyle(check.state).Render(check.detail)
		lines = append(lines, component.Selected(theme, ansi.Truncate(row, shared.bodyWidth(), "…"), chosen,
			h.Focus == PaneChecks, shared.bodyWidth()))
		if check.fix != "" && check.state != healthOff {
			lines = append(lines, "  "+strings.Repeat(" ", healthNameWidth)+theme.Hint.Render("→ "+check.fix))
		}
	}
	return append(lines, "")
}

// headline says in one line whether everything works, and what to do when
// it does not.
func (h *Health) headline() (string, string) {
	shared := h.shared
	theme := shared.Styles
	checks := h.checks()
	bad := slices.ContainsFunc(checks, func(check healthCheck) bool { return check.state >= healthWarn })
	switch {
	case shared.Running && shared.StepCursor < len(shared.Steps):
		return theme.Running.Render(shared.spinner() + " Bringing the sandbox up"), "Step " +
			strconv.Itoa(shared.StepCursor+1) + " of " + strconv.Itoa(len(shared.Steps)) + ": " +
			shared.Steps[shared.StepCursor].Step.Title + ". The Home tab shows each step."
	case shared.FailedStep() != nil:
		return theme.Failed.Bold(true).Render("✕ The bring-up stopped at: " + shared.FailedStep().Step.Title),
			"The Home tab says what went wrong. u starts again from that step."
	case !shared.SandboxUp():
		return theme.Dim.Bold(true).Render("○ The sandbox is not running"),
			"u brings it up. The first time takes two to four minutes."
	case bad:
		return theme.Attention.Bold(true).Render("! Something needs a look"),
			"The check marked below says what to do."
	}
	return theme.OK.Bold(true).Render("✓ Everything works"),
		"The agent runs, its requests go through Keycard, and Keycard can verify who it is."
}

// mark draws a check's state: ✓, !, ✕, a spinner while it starts, or ○
// while it is off.
func (h *Health) mark(state healthState) string {
	theme := h.shared.Styles
	switch state {
	case healthOK:
		return theme.OK.Render("✓")
	case healthWait:
		return theme.Running.Render(h.shared.spinner())
	case healthWarn:
		return theme.Warn.Render("!")
	case healthBad:
		return theme.Failed.Render("✕")
	case healthOff:
	}
	return theme.Faint.Render("○")
}

// lineStyle colors a check's line by how it is.
func (h *Health) lineStyle(state healthState) lipgloss.Style {
	theme := h.shared.Styles
	switch state {
	case healthWarn:
		return theme.Warn
	case healthBad:
		return theme.Failed
	case healthOff:
		return theme.Dim
	case healthOK, healthWait:
	}
	return theme.Muted
}

// caption says what the log the tab shows is, wrapped to width.
func (h *Health) caption(width int) []string {
	shared := h.shared
	text := ""
	if h.pick == "" {
		checks := h.checks()
		if h.cursor < len(checks) && checks[h.cursor].source == shared.Logs.Source() {
			text = checks[h.cursor].caption
		}
	}
	for _, entry := range h.LogEntries() {
		if text == "" && entry.name == h.pick {
			text = entry.caption
		}
	}
	if text == "" {
		return nil
	}
	lines := strings.Split(lipgloss.NewStyle().Width(max(width-1, 1)).Render(shared.Styles.Muted.Render(text)), "\n")
	if len(lines) > maxCaptionRows {
		lines = lines[:maxCaptionRows]
		lines[maxCaptionRows-1] += shared.Styles.Muted.Render("…")
	}
	return lines
}

// logTitle names the log the tab shows: by its check, or by its name in
// the list of every log.
func (h *Health) logTitle() string {
	if h.pick != "" {
		return h.pick + " · log"
	}
	checks := h.checks()
	if h.cursor < len(checks) && checks[h.cursor].source == h.shared.Logs.Source() {
		return checks[h.cursor].name + " · log"
	}
	return sourceTitle(h.shared.Logs.Source())
}
