package page

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
)

const (
	// agentBoxRows is the agent pane's height, with its border.
	agentBoxRows = 5
	// The left column takes 1/leftShare of the width, within the bounds.
	minLeftWidth, maxLeftWidth, leftShare = 40, 56, 3
	// responseCaptionRows fits the status line, up to verdictRows lines of
	// Keycard's verdict and a blank line.
	responseCaptionRows, verdictRows = 4, 2
	// commandIndent indents each line of a command after its first, and
	// commandMore ends each line the command goes on from.
	commandIndent, commandMore = "    ", ` \`
	// maxBodyLines bounds how much of a response's body the tab keeps.
	maxBodyLines = 2000
	// recordSlack is how early a proxy record may be stamped, for clock skew.
	recordSlack = time.Second
	// agentEmailWidth is the width of an agent's email in the list.
	agentEmailWidth = 30
)

// AgentPane is the Agents tab's pane with focus.
type AgentPane int

// The Agents tab's panes.
const (
	AgentPaneAgent AgentPane = iota
	AgentPaneRequests
	AgentPaneRight
	agentPaneCount
)

// FollowMsg asks the root model to follow a container's log.
type FollowMsg struct{ Row state.ContainerRow }

// ShowAgentsMsg asks the root model to open the list of agents.
type ShowAgentsMsg struct{}

// SendRequestMsg asks the root model to send a request from agent's
// container, and to answer with a RequestDoneMsg. Seq numbers the request.
type SendRequestMsg struct {
	Agent             string
	Seq               int
	Method, URL, Body string
}

// RequestDoneMsg is what came back from a SendRequestMsg.
type RequestDoneMsg struct {
	Agent       string
	Seq         int
	Method, URL string
	Response    sandbox.AgentResponse
	Err         error
}

// DeployTitle names the action that brings up email's agent.
func DeployTitle(email string) string { return "Bringing up " + email + "'s agent" }

// trial is a request the Agents tab offers to send from the agent.
type trial struct {
	method, url, body string
	// name is the resource whose credential the proxy adds, if any.
	// blocked marks a host the demo does not allow.
	name    string
	blocked bool
}

// requestState is the last request the tab sent. A response that carries
// an earlier seq is dropped.
type requestState struct {
	agent   string
	seq     int
	trial   trial
	started time.Time
	// command is the command the agent's container runs to send it.
	command  []string
	running  bool
	response sandbox.AgentResponse
	err      error
}

// Agents is the dashboard's tab for the agents. The agent selected here is
// the one every tab shows. On the left are the agent and the requests it
// can send. On the right is the agent's log or the last response.
type Agents struct {
	view

	// The pane with focus, and whether the right one shows the response.
	focus        AgentPane
	showResponse bool
	// The selected request, the last one sent, and how many the tab sent.
	trial   int
	request requestState
	sent    int
	// picker is the list of agents to switch to.
	picker component.Picker
}

// NewAgents returns the Agents tab.
func NewAgents(ui *UI, sandbox *state.Sandbox) *Agents {
	return &Agents{ui: ui, state: sandbox}
}

// View draws the panes.
func (a *Agents) View() string {
	left, height := a.leftWidth(), a.ui.BodyRows()
	inner := left - component.BorderCols
	column := lipgloss.JoinVertical(lipgloss.Left,
		component.Box(a.ui.Styles, "Agent", a.agentPaneLines(inner), left, agentBoxRows, a.focus == AgentPaneAgent),
		component.Box(a.ui.Styles, "Try a request", a.requestPaneLines(inner), left,
			max(height-agentBoxRows, component.BorderRows), a.focus == AgentPaneRequests))
	right := a.rightWidth()
	pane := a.rightPane(right, height)
	if len(a.state.SelectedPods()) == 0 && !a.state.Run.Job.Acting() {
		pane = component.Box(a.ui.Styles, "Agent log", a.noPodLines(right-component.BorderCols), right, height,
			a.focus == AgentPaneRight)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, column, " ", pane)
}

// Hints lists the keys of the pane with focus.
func (a *Agents) Hints() []component.Hint {
	var hints []component.Hint
	switch a.focus {
	case AgentPaneAgent:
		if len(a.state.Agents.List) > 1 {
			hints = append(hints, component.Hint{Key: component.KeyEnter, Desc: "switch agent"})
		}
		hints = append(hints, component.Hint{Key: "↓", Desc: "requests"})
	case AgentPaneRequests:
		hints = append(hints, component.Hint{Key: "↑↓", Desc: "request"},
			component.Hint{Key: component.KeyEnter, Desc: "send"})
	case AgentPaneRight, agentPaneCount:
		return logHints()
	}
	hints = append(hints, component.Hint{Key: component.KeyTab, Desc: "next pane"},
		component.Hint{Key: "a", Desc: a.AgentActionName()}, component.Hint{Key: "n", Desc: "add agent"})
	if agent, i := a.state.Selected(); i >= 0 && !agent.Default {
		hints = append(hints, component.Hint{Key: "X", Desc: "remove"})
	}
	return append(hints, component.Hint{Key: "?", Desc: wordKeys})
}

// Keys moves between the panes, or gives a key to the pane with focus.
func (a *Agents) Keys(msg tea.KeyPressMsg) tea.Cmd {
	keys := a.ui.Keys
	switch {
	case key.Matches(msg, keys.NextPane), msg.String() == "right":
		return follow(a.FocusPane((a.focus + 1) % agentPaneCount))
	case key.Matches(msg, keys.PrevPane), msg.String() == "left":
		return follow(a.FocusPane((a.focus + agentPaneCount - 1) % agentPaneCount))
	case a.focus == AgentPaneAgent:
		return a.agentPaneKeys(msg)
	case a.focus == AgentPaneRequests:
		return a.requestPaneKeys(msg)
	}
	return a.rightPaneKeys(msg)
}

// Layout sizes the log pane on the right, below any response caption.
func (a *Agents) Layout() {
	height := a.ui.BodyRows()
	if a.responseShows() {
		height -= len(a.responseCaption(a.rightWidth() - component.BorderCols))
	}
	a.ui.Logs.SetSize(max(a.rightWidth()-component.BorderCols, 1), max(height-component.BorderRows-1, 1))
}

// FocusPane gives pane the focus, and returns the container whose log the
// root model follows, if any.
func (a *Agents) FocusPane(pane AgentPane) (state.ContainerRow, bool) {
	a.focus = pane
	switch pane {
	case AgentPaneAgent:
		a.showResponse = false
	case AgentPaneRequests:
		a.showResponse = true
	case AgentPaneRight, agentPaneCount:
	}
	return a.ShowRight()
}

// FocusLog gives the right pane the focus and shows the agent's log.
func (a *Agents) FocusLog() (state.ContainerRow, bool) {
	a.showResponse = false
	return a.FocusPane(AgentPaneRight)
}

// ShowRight points the right pane at the last response or the agent's log,
// and returns the agent's container when it shows the log.
func (a *Agents) ShowRight() (state.ContainerRow, bool) {
	defer a.Layout()
	if a.showResponse {
		a.ui.Logs.Show(state.SourceResponse)
		return state.ContainerRow{}, false
	}
	return a.state.Container(state.AgentContainer)
}

// follow asks the root model to follow row's log, when ok.
func follow(row state.ContainerRow, ok bool) tea.Cmd {
	if !ok {
		return nil
	}
	return func() tea.Msg { return FollowMsg{Row: row} }
}

// Reset forgets the last request, for a new agent, pod or demo. It does not
// size the log pane, which the tab that shows lays out.
func (a *Agents) Reset() {
	a.request = requestState{}
	a.trial = min(a.trial, max(len(a.trials())-1, 0))
	a.ui.Logs.Log(state.SourceResponse).Reset()
}

// Send has the agent send the selected request, in the background.
func (a *Agents) Send() tea.Cmd {
	trials := a.trials()
	switch {
	case a.request.running:
		a.ui.Notify("A request is on its way. Wait for it to come back.", true)
		return nil
	case !a.agentReady():
		a.ui.Notify("The agent is not running yet.", true)
		return nil
	}
	item := trials[min(a.trial, len(trials)-1)]
	selected, _ := a.state.Selected()
	agent := selected.Email
	a.sent++
	a.request = requestState{
		agent: agent, seq: a.sent, trial: item, started: time.Now(), running: true,
		command: sandbox.AgentCurl(item.method, item.url, item.body),
	}
	a.ui.Logs.Log(state.SourceResponse).Reset()
	focus := follow(a.FocusPane(AgentPaneRequests))
	request := SendRequestMsg{Agent: agent, Seq: a.sent, Method: item.method, URL: item.url, Body: item.body}
	return tea.Batch(focus, func() tea.Msg { return request })
}

// OnRequestDone shows the response, and drops one for an earlier request.
func (a *Agents) OnRequestDone(msg RequestDoneMsg) {
	if a.request.running && a.request.seq == msg.Seq {
		request := &a.request
		request.running, request.response, request.err = false, msg.Response, msg.Err
		body := a.ui.Logs.Log(state.SourceResponse)
		body.Reset()
		body.Add(excerpt(msg.Response.Body, maxBodyLines)...)
		if msg.Response.Failure != "" {
			body.Add(msg.Response.Failure)
		}
		a.ui.Logs.Refresh(true)
	}
}

// OpenPicker puts the list of agents' cursor on the selected agent.
func (a *Agents) OpenPicker() {
	_, index := a.state.Selected()
	a.picker.Cursor = max(index, 0)
}

// PickerKeys moves through the list of agents and returns the one chosen.
func (a *Agents) PickerKeys(msg tea.KeyPressMsg) (string, component.Pick) {
	agents := a.state.Agents.List
	pick := a.picker.Update(msg, a.ui.Keys, len(agents))
	if pick == component.PickChosen && a.picker.Cursor < len(agents) {
		return agents[a.picker.Cursor].Email, pick
	}
	return "", pick
}

// PickerView lists every agent, each with how its pod is, to switch to.
func (a *Agents) PickerView() string {
	theme := a.ui.Styles
	width := component.ModalInnerWidth(a.ui.Width)
	selected, _ := a.state.Selected()
	shown := selected.Email
	lines := make([]string, 0, len(a.state.Agents.List))
	for i, agent := range a.state.Agents.List {
		mark, status := a.agentState(agent)
		cursor := "  "
		if i == a.picker.Cursor {
			cursor = theme.Accent.Render("▸ ")
		}
		email := theme.Text.Render(component.Pad(agent.Email, agentEmailWidth))
		if agent.Email == shown {
			email = theme.Text.Bold(true).Render(component.Pad(agent.Email, agentEmailWidth))
		}
		line := cursor + mark + " " + email + " " + status
		lines = append(lines, component.Selected(theme, ansi.Truncate(line, width, "…"), i == a.picker.Cursor, true,
			width))
	}
	return component.ListModal(theme, a.ui.Width, max(a.ui.Height-component.ChromeRows-logsChrome, 1),
		"Switch agent", "Each person's agent acts as them. Every tab shows the agent you choose.", lines,
		a.picker.Cursor, component.Hint{Key: "↑↓", Desc: wordChoose},
		component.Hint{Key: component.KeyEnter, Desc: "switch"}, component.Hint{Key: component.KeyEsc, Desc: "close"})
}

func (a *Agents) trials() []trial { return trialsFor(a.state.LoadedDemo()) }

// agentReady reports whether the agent's container runs and is ready.
func (a *Agents) agentReady() bool {
	row, ok := a.state.Container(state.AgentContainer)
	return ok && row.Container.Ready
}

func (a *Agents) leftWidth() int {
	return min(max((a.ui.Width-component.PageIndent)/leftShare, minLeftWidth), maxLeftWidth)
}

func (a *Agents) rightWidth() int {
	return max(a.ui.Width-component.PageIndent-a.leftWidth()-1, component.CornerCols+1)
}

// responseShows reports whether the pane on the right shows the last
// response. An action's log takes its place while it shows.
func (a *Agents) responseShows() bool {
	return a.showResponse && a.ui.Logs.Source() == state.SourceResponse
}

func (a *Agents) rightPane(width, height int) string {
	source := a.ui.Logs.Source()
	focused := a.focus == AgentPaneRight
	if a.responseShows() {
		return a.ui.logPane("Response", a.responseCaption(width-component.BorderCols), a.responseWaiting(),
			width, height, focused)
	}
	return a.ui.logPane(sourceTitle(source), nil, waitingText(source), width, height, focused)
}

// agentPaneLines show the selected agent, its pod and how to switch.
func (a *Agents) agentPaneLines(width int) []string {
	theme := a.ui.Styles
	agent, index := a.state.Selected()
	if agent.Email == "" {
		return []string{theme.Dim.Render("No one has an agent yet."), theme.Dim.Render("Press n to give someone one.")}
	}
	mark, status := a.agentState(agent)
	switching := theme.Dim.Render("No other agent in this demo. n adds one.")
	if len(a.state.Agents.List) > 1 {
		switching = theme.Dim.Render(fmt.Sprintf("%d of %d. ", index+1, len(a.state.Agents.List))) +
			theme.Accent.Render("enter") + theme.Dim.Render(" switches to another.")
	}
	lines := []string{mark + " " + theme.Text.Bold(true).Render(agent.Email), "  " + status, switching}
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "…")
	}
	return lines
}

// agentState marks how an agent's pod is, and says it in a few words.
func (a *Agents) agentState(agent sandbox.Agent) (string, string) {
	pod, row := a.state.PodState(agent)
	mark, look := a.ui.podMark(pod)
	said := look.Render(map[state.PodState]string{
		state.PodNone:           "not brought up yet. u brings it up",
		state.PodComingUp:       "coming up",
		state.PodUnread:         "cannot read the pods",
		state.PodMissing:        "no pod. u brings it up",
		state.PodFirewallFailed: "firewall failed: " + containerDetail(row.Container),
		state.PodRunning:        "running",
		state.PodAgentFailed:    containerDetail(row.Container),
		state.PodStarting:       "starting",
	}[pod])
	if pod != state.PodNone && pod != state.PodComingUp && pod != state.PodUnread {
		said += a.ui.Styles.Dim.Render(" · pod " + agent.Pod)
	}
	return look.Render(mark), said
}

// requestPaneLines list the requests, then what the selected one needs.
func (a *Agents) requestPaneLines(width int) []string {
	theme := a.ui.Styles
	trials := a.trials()
	lines := make([]string, 0, len(trials)+verdictRows)
	for i, item := range trials {
		cursor := "  "
		if i == a.trial {
			cursor = theme.Accent.Render("▸ ")
		}
		line := cursor + theme.Text.Render(item.method) + " " + theme.Muted.Render(strings.TrimPrefix(item.url, "https://"))
		lines = append(lines, component.Selected(theme, ansi.Truncate(line, width, "…"), i == a.trial,
			a.focus == AgentPaneRequests, width))
	}
	aboutStyle, about := a.trialAbout()
	lines = append(lines, "")
	lines = append(lines, strings.Split(aboutStyle.Width(width).Render(about), "\n")...)
	lines = append(lines, "")
	return append(lines, strings.Split(theme.Muted.Width(width).Render(a.requestIntro()), "\n")...)
}

// trialAbout says what the selected request needs from Keycard.
func (a *Agents) trialAbout() (lipgloss.Style, string) {
	theme := a.ui.Styles
	trials := a.trials()
	item := trials[min(a.trial, len(trials)-1)]
	agent, _ := a.state.Selected()
	switch {
	case item.blocked:
		return theme.Warn, "Not on the allow list, so the proxy refuses it."
	case item.name != "":
		return theme.Accent, "Needs " + agent.Email + "'s " + item.name + " credential, which the proxy gets from Keycard."
	}
	return theme.Muted, "Needs no credential."
}

// requestIntro says how the agent sends a request, and what else it does.
func (a *Agents) requestIntro() string {
	intro := "Enter sends it from " + a.agentName() + ", with no credential. The egress proxy asks Keycard " +
		"for one and adds it."
	if scenario := a.state.LoadedDemo(); scenario != nil && scenario.Run.Mode == sandbox.ModeShell &&
		len(scenario.Run.Command) > 0 {
		intro += "\n\nPress a to open " + scenario.Run.Command[0] + " in the agent."
	}
	return intro
}

// noPodLines say why the selected agent has no pod to show.
func (a *Agents) noPodLines(width int) []string {
	theme := a.ui.Styles
	lines := func(title, text string) []string {
		return append([]string{title}, strings.Split(theme.Muted.Width(width).Render(text), "\n")...)
	}
	phase, row := a.state.Phase()
	switch {
	case phase == state.PhaseBringingUp:
		return lines(theme.Title.Render(a.ui.spinner()+" The sandbox is coming up"),
			"The agent shows here once its pod runs. Press 1 to watch the bring-up.")
	case phase == state.PhaseReading:
		return lines(theme.Title.Render(a.ui.spinner()+" Reading the sandbox"),
			"The agent shows here once the first read comes back.")
	case phase == state.PhaseStopped || phase == state.PhaseFailed:
		return lines(theme.Title.Render("No pod for "+a.agentName()+" yet"),
			"The bring-up stopped at: "+row.Step.Title+". Press 1 to see why, or u to start again from: "+
				a.state.Run.Steps[a.state.ResumeIndex()].Step.Title+".")
	case a.state.Agents.PodsErr != nil && a.state.Reads.Status.Deployed:
		return lines(theme.Failed.Render("✕ Cannot read the pods"), component.FirstLine(a.state.Agents.PodsErr.Error()))
	}
	return lines(theme.Title.Render("No pod for "+a.agentName()+" yet"),
		"Press u to bring the sandbox up. It creates the pod of every agent.")
}

// responseCaption is the header over the response body: the request, its
// status, Keycard's verdict and the command the agent ran.
func (a *Agents) responseCaption(width int) []string {
	theme := a.ui.Styles
	request := a.request
	// The status goes first so a narrow pane cuts the URL, not the status.
	sent := theme.Dim.Render(" · ") + theme.Text.Render(request.trial.method+" "+request.trial.url)
	var head, more string
	switch {
	case request.started.IsZero():
		head = theme.Dim.Render("Nothing sent yet.")
		more = theme.Dim.Render("Choose a request and press enter. What comes back shows here, with what Keycard did.")
	case request.running:
		head = theme.Running.Render(a.ui.spinner()+" "+component.Duration(time.Since(request.started))) + sent
		more = a.keycardVerdict()
	case request.err != nil:
		head, more = theme.Failed.Render("no response")+sent, theme.Failed.Render("✕ "+
			component.FirstLine(request.err.Error()))
	case request.response.Failure != "":
		head, more = theme.Failed.Render("no response")+sent, a.keycardVerdict()
	default:
		head = a.statusText(request.response.Status) + theme.Dim.Render(" in "+
			request.response.Took.Round(time.Millisecond).String()) + sent
		more = a.keycardVerdict()
	}
	wrapped := strings.Split(lipgloss.NewStyle().Width(width).Render(more), "\n")
	if len(wrapped) > verdictRows {
		wrapped = wrapped[:verdictRows]
		last := strings.TrimRight(ansi.Strip(wrapped[verdictRows-1]), " ")
		wrapped[verdictRows-1] = a.ui.Styles.Dim.Render(ansi.Truncate(last, width-1, "") + "…")
	}
	lines := append([]string{head}, wrapped...)
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "…")
	}
	return append(component.Fit(lines, responseCaptionRows), a.commandCaption(width)...)
}

// commandCaption shows the command the agent ran for the last request.
func (a *Agents) commandCaption(width int) []string {
	if len(a.request.command) == 0 {
		return nil
	}
	theme := a.ui.Styles
	lines := []string{theme.Dim.Render("In the agent's container")}
	for _, line := range commandLines(a.request.command, width) {
		lines = append(lines, theme.Text.Render(line))
	}
	return append(lines, "")
}

// commandLine is one line of a command laid out for a shell. inWord is
// set when the next line goes on with the same word.
type commandLine struct {
	text   string
	inWord bool
}

// commandLines lays a command out in width columns, quoted so that the
// lines paste into a shell as one command.
func commandLines(args []string, width int) []string {
	var lines []commandLine
	for _, unit := range flagUnits(args) {
		lines = placeUnit(lines, unit, width)
	}
	return joinLines(lines)
}

// placeUnit adds a flag and its value to lines: on the last line, or on a
// line of its own, or one argument at a time.
func placeUnit(lines []commandLine, unit []string, width int) []commandLine {
	fits := func(text string) bool { return ansi.StringWidth(text)+len(commandMore) <= width }
	text := strings.Join(sandbox.ShellWords(unit), " ")
	switch last := len(lines) - 1; {
	case last < 0 && fits(text):
		return append(lines, commandLine{text: text})
	case last >= 0 && fits(lines[last].text+" "+text):
		lines[last].text += " " + text
		return lines
	case last >= 0 && fits(commandIndent+text):
		return append(lines, commandLine{text: commandIndent + text})
	}
	for _, arg := range unit {
		lines = append(lines, wordLines(arg, len(lines) > 0, width)...)
	}
	return lines
}

// joinLines ends each line but the last in a backslash, after a space
// unless the next line goes on with the same word.
func joinLines(lines []commandLine) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		switch {
		case i == len(lines)-1:
			out[i] = line.text
		case line.inWord:
			out[i] = line.text + `\`
		default:
			out[i] = line.text + commandMore
		}
	}
	return out
}

// wordLines puts one argument on lines of its own. A too-wide argument goes
// in quoted pieces that a trailing backslash joins back into one word.
func wordLines(arg string, indent bool, width int) []commandLine {
	prefix := ""
	if indent {
		prefix = commandIndent
	}
	quote := func(text string) string { return sandbox.ShellWords([]string{text})[0] }
	var lines []commandLine
	for rest := []rune(arg); len(rest) > 0; prefix = "" {
		n := len(rest)
		for n > 1 && ansi.StringWidth(prefix+quote(string(rest[:n])))+len(commandMore) > width {
			n--
		}
		lines = append(lines, commandLine{text: prefix + quote(string(rest[:n])), inWord: true})
		rest = rest[n:]
	}
	lines[len(lines)-1].inWord = false
	return lines
}

// flagUnits pairs each flag in args with the value after it.
func flagUnits(args []string) [][]string {
	units := make([][]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		unit := []string{args[i]}
		if strings.HasPrefix(args[i], "-") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			unit = append(unit, args[i])
		}
		units = append(units, unit)
	}
	return units
}

// responseWaiting is what the response's pane shows while it has no body.
func (a *Agents) responseWaiting() string {
	switch {
	case a.request.started.IsZero():
		return ""
	case a.request.running:
		return "Waiting for the response…"
	}
	return "The response has no body."
}

// statusText colors an HTTP status by its class.
func (a *Agents) statusText(status int) string {
	theme := a.ui.Styles
	text := strconv.Itoa(status) + " " + http.StatusText(status)
	switch {
	case status >= http.StatusBadRequest:
		return theme.Failed.Bold(true).Render(text)
	case status >= http.StatusMultipleChoices:
		return theme.Warn.Bold(true).Render(text)
	}
	return theme.OK.Bold(true).Render(text)
}

// keycardVerdict says what the proxy and Keycard did with the last request.
func (a *Agents) keycardVerdict() string {
	theme := a.ui.Styles
	request := a.request
	label := theme.Dim.Render("Keycard  ")
	records, _ := a.state.Agents.Traffic.Snapshot()
	record, found := proxylog.RecordFor(records, request.trial.method, request.trial.host(), request.trial.path(),
		request.started.Add(-recordSlack))
	switch {
	case !found && a.state.Agents.Traffic.Err() != nil:
		return label + theme.Failed.Render("✕ Cannot read the proxy's log: "+
			component.FirstLine(a.state.Agents.Traffic.Err().Error()))
	case !found:
		return label + theme.Dim.Render("waiting for the proxy's record of the request…")
	case record.RefusedCredential():
		return label + theme.Failed.Render("✕ No credential: "+proxylog.ReasonText(record.Decision.Reason)+
			errorSuffix(record.Decision.Error))
	case record.Denied():
		return label + theme.Failed.Render("✕ The proxy refused it: "+proxylog.ReasonText(record.Decision.Reason))
	case request.trial.name != "":
		return label + theme.OK.Render("✓ The proxy got "+request.agent+"'s "+request.trial.name+
			" credential from Keycard and added it")
	}
	return label + theme.OK.Render("✓ Allowed with no credential, as the endpoint needs none")
}

// rightPaneKeys scrolls and filters the right pane, and Esc leaves it.
func (a *Agents) rightPaneKeys(msg tea.KeyPressMsg) tea.Cmd {
	if !key.Matches(msg, a.ui.Keys.Back) || a.ui.Logs.Filter() != "" {
		return a.ui.Logs.Keys(msg, a.ui.Keys)
	}
	if a.showResponse {
		return follow(a.FocusPane(AgentPaneRequests))
	}
	return follow(a.FocusPane(AgentPaneAgent))
}

// agentPaneKeys opens the list of agents or goes down to the requests.
func (a *Agents) agentPaneKeys(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case msg.String() == component.KeyEnter:
		if len(a.state.Agents.List) <= 1 {
			a.ui.Notify("This is the only agent in this demo. Press n to give someone else one.", false)
			return nil
		}
		a.OpenPicker()
		return func() tea.Msg { return ShowAgentsMsg{} }
	case key.Matches(msg, a.ui.Keys.Down):
		return follow(a.FocusPane(AgentPaneRequests))
	}
	return a.ui.Logs.Keys(msg, a.ui.Keys)
}

// requestPaneKeys chooses a request and sends it. Up from the first goes
// to the agent.
func (a *Agents) requestPaneKeys(msg tea.KeyPressMsg) tea.Cmd {
	keys := a.ui.Keys
	switch {
	case key.Matches(msg, keys.Up) && a.trial == 0:
		return follow(a.FocusPane(AgentPaneAgent))
	case key.Matches(msg, keys.Up):
		a.trial--
	case key.Matches(msg, keys.Down):
		a.trial = min(a.trial+1, max(len(a.trials())-1, 0))
	case msg.String() == component.KeyEnter:
		return a.Send()
	default:
		return a.ui.Logs.Keys(msg, keys)
	}
	return nil
}

func (t trial) host() string {
	parsed, err := url.Parse(t.url)
	if err != nil {
		return t.url
	}
	return parsed.Hostname()
}

// path is the path the trial requests, as the proxy records it.
func (t trial) path() string {
	parsed, err := url.Parse(t.url)
	if err != nil || parsed.Path == "" {
		return "/"
	}
	return parsed.Path
}

// trialsFor lists the demo's requests, then one to a host no demo allows.
func trialsFor(scenario *sandbox.Scenario) []trial {
	blocked := trial{method: http.MethodGet, url: "https://example.com/", blocked: true}
	if scenario == nil {
		return []trial{blocked}
	}
	endpoints := scenario.EndpointList()
	trials := make([]trial, 0, len(scenario.Try)+1)
	for _, try := range scenario.Try {
		item := trial{method: try.Method, url: try.URL, body: try.Body}
		if endpoint, ok := sandbox.EndpointFor(endpoints, try.URL); ok && endpoint.Credential {
			item.name = endpoint.Name
		}
		trials = append(trials, item)
	}
	return append(trials, blocked)
}

// excerpt returns the first rows lines of body, indented if it is JSON.
func excerpt(body string, rows int) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(body), "", "  ") == nil {
		body = pretty.String()
	}
	lines := strings.SplitN(body, "\n", rows+1)
	return lines[:min(len(lines), rows)]
}

func errorSuffix(text string) string {
	if text == "" {
		return ""
	}
	return " (" + component.FirstLine(text) + ")"
}
