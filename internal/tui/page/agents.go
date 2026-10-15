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
	// agentBoxRows is the height of the Agents tab's agent pane, with its
	// border.
	agentBoxRows = 5
	// The Agents tab's left column takes a leftShare of the width, within
	// minLeftWidth and maxLeftWidth.
	minLeftWidth, maxLeftWidth, leftShare = 40, 56, 3
	// responseCaptionRows is what the response's caption takes over the
	// command the agent ran: the request and its status, what Keycard did
	// in up to verdictRows lines, and a blank line.
	responseCaptionRows, verdictRows = 4, 2
	// commandIndent indents each line of a command after its first, and
	// commandMore ends each line the command goes on from.
	commandIndent, commandMore = "    ", ` \`
	// maxBodyLines bounds how much of a response's body the tab keeps.
	maxBodyLines = 2000
	// recordSlack is how much earlier than the request a proxy record may
	// be stamped and still be the request's, for clocks that differ.
	recordSlack = time.Second
	// agentEmailWidth is the width of an agent's email in the list.
	agentEmailWidth = 30
)

// AgentPane is the pane of the Agents tab with focus: the agent it shows,
// the requests it can send, or the pane on the right.
type AgentPane int

// The Agents tab's panes.
const (
	AgentPaneAgent AgentPane = iota
	AgentPaneRequests
	AgentPaneRight
	agentPaneCount
)

// SendRequestMsg asks the root model to send a request from agent's
// container, and to answer with a RequestDoneMsg. Seq numbers the request.
type SendRequestMsg struct {
	Agent             string
	Seq               int
	Method, URL, Body string
}

// RequestDoneMsg is what came back from a request the Agents tab sent
// from the agent's container.
type RequestDoneMsg struct {
	Agent       string
	Seq         int
	Method, URL string
	Response    sandbox.AgentResponse
	Err         error
}

// DeployTitle names the action that brings up the agent of email once the
// sandbox is up.
func DeployTitle(email string) string { return "Bringing up " + email + "'s agent" }

// trial is a request the Agents tab offers to send from the agent.
type trial struct {
	method, url, body string
	// name names the resource whose credential the proxy adds, or is ""
	// for an endpoint that needs none. blocked is set for a host the demo
	// does not allow.
	name    string
	blocked bool
}

// requestState is the request the Agents tab sent last, and the agent it
// sent it from. seq numbers it, so that a response to an earlier request
// does not take its place.
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

// Agents is the dashboard's tab for working with the agents. It lists an
// agent for each person the settings give one, and the agent selected
// there is the one every tab shows: its pod, its log, its traffic and its
// health. On the left are the agent it shows and the requests it can send.
// On the right is the agent's log, or with the requests in focus, what the
// last one got back.
type Agents struct {
	view

	host Host
	// focus is the pane with focus, and showResponse is set when the pane
	// on the right shows the last response, and not the agent's log.
	focus        AgentPane
	showResponse bool
	// trial is the request selected, request what the last one sent got
	// back, and sent how many requests the tab sent.
	trial   int
	request requestState
	sent    int
	// picker is the list of agents to switch to.
	picker component.Picker
}

// NewAgents returns the Agents tab.
func NewAgents(ui *UI, sandbox *state.Sandbox, host Host) *Agents {
	return &Agents{ui: ui, state: sandbox, host: host}
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

// Keys moves between the panes, and gives a key to the pane with focus.
// Tab and the arrows left and right go round the panes.
func (a *Agents) Keys(msg tea.KeyPressMsg) tea.Cmd {
	keys := a.ui.Keys
	switch {
	case key.Matches(msg, keys.NextPane), msg.String() == "right":
		a.FocusPane((a.focus + 1) % agentPaneCount)
	case key.Matches(msg, keys.PrevPane), msg.String() == "left":
		a.FocusPane((a.focus + agentPaneCount - 1) % agentPaneCount)
	case a.focus == AgentPaneAgent:
		return a.agentPaneKeys(msg)
	case a.focus == AgentPaneRequests:
		return a.requestPaneKeys(msg)
	default:
		return a.rightPaneKeys(msg)
	}
	return nil
}

// Layout sizes the log pane on the right, under the response's caption
// when it shows the last response.
func (a *Agents) Layout() {
	height := a.ui.BodyRows()
	if a.responseShows() {
		height -= len(a.responseCaption(a.rightWidth() - component.BorderCols))
	}
	a.ui.Logs.SetSize(max(a.rightWidth()-component.BorderCols, 1), max(height-component.BorderRows-1, 1))
}

// FocusPane gives pane the focus. The pane on the right shows the agent's
// log for the agent pane, and the last response for the requests. With
// focus itself, it keeps what it showed.
func (a *Agents) FocusPane(pane AgentPane) {
	a.focus = pane
	switch pane {
	case AgentPaneAgent:
		a.showResponse = false
	case AgentPaneRequests:
		a.showResponse = true
	case AgentPaneRight, agentPaneCount:
	}
	a.ShowRight()
}

// FocusLog gives the pane on the right the focus, with the agent's log.
func (a *Agents) FocusLog() {
	a.showResponse = false
	a.FocusPane(AgentPaneRight)
}

// ShowRight points the pane on the right at what it shows.
func (a *Agents) ShowRight() {
	if a.showResponse {
		a.ui.Logs.Show(state.SourceResponse)
	} else if row, ok := a.state.Container(state.AgentContainer); ok {
		a.host.Follow(row)
	}
	a.Layout()
}

// Reset forgets the last request, as for an agent, a pod or a demo that is
// new, and keeps the selected request within the demo's requests. It does
// not size the log pane, which the tab that shows lays out.
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
	a.FocusPane(AgentPaneRequests)
	a.state.Record("▸", agent+"'s agent sent "+item.method+" "+item.url)
	request := SendRequestMsg{Agent: agent, Seq: a.sent, Method: item.method, URL: item.url, Body: item.body}
	return func() tea.Msg { return request }
}

// OnRequestDone records what came back. It shows it while it answers the
// request the tab shows.
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
	sent := msg.Agent + "'s agent: " + msg.Method + " " + msg.URL
	switch {
	case msg.Err != nil:
		a.state.Record("✕", sent+": "+component.FirstLine(msg.Err.Error()))
	case msg.Response.Failure != "":
		a.state.Record("✕", sent+": "+msg.Response.Failure)
	default:
		mark := "✓"
		if msg.Response.Status >= http.StatusBadRequest {
			mark = "✕"
		}
		a.state.Record(mark, sent+": "+strconv.Itoa(msg.Response.Status)+" "+http.StatusText(msg.Response.Status))
	}
}

// OpenPicker puts the list of agents' cursor on the selected agent.
func (a *Agents) OpenPicker() {
	_, index := a.state.Selected()
	a.picker.Cursor = max(index, 0)
}

// PickerKeys moves through the list of agents, and returns the email of
// the one chosen.
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

// trials offers a request for each of the demo's endpoints that says
// something on its own.
func (a *Agents) trials() []trial { return trialsFor(a.endpoints()) }

// agentReady reports whether the agent's container runs and is ready.
func (a *Agents) agentReady() bool {
	row, ok := a.state.Container(state.AgentContainer)
	return ok && row.Container.Ready
}

// leftWidth is the width of the tab's left column, and rightWidth that of
// the pane right of it.
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

// rightPane draws the log pane on the right: the agent's log, or the last
// response under what it was.
func (a *Agents) rightPane(width, height int) string {
	source := a.ui.Logs.Source()
	focused := a.focus == AgentPaneRight
	if a.responseShows() {
		return a.ui.logPane("Response", a.responseCaption(width-component.BorderCols), a.responseWaiting(),
			width, height, focused)
	}
	return a.ui.logPane(sourceTitle(source), nil, waitingText(source), width, height, focused)
}

// agentPaneLines show the agent the tabs show: its person, how its pod is,
// and how to switch to another.
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
	theme := a.ui.Styles
	where := theme.Dim.Render(" · pod " + agent.Pod)
	switch pod, row := a.state.PodState(agent); pod {
	case state.PodNone:
		if a.state.Run.Job.Agent == agent.Email {
			return theme.Running.Render(a.ui.spinner()), theme.Running.Render("coming up")
		}
		return theme.Dim.Render("○"), theme.Dim.Render("not brought up yet. u brings it up")
	case state.PodUnread, state.PodMissing:
		return theme.Dim.Render("○"), theme.Dim.Render("not running") + where
	case state.PodFirewallFailed:
		return theme.Failed.Render("✕"), theme.Failed.Render("firewall failed: "+containerDetail(row.Container)) + where
	case state.PodRunning:
		return theme.OK.Render("●"), theme.OK.Render("running") + where
	case state.PodAgentFailed:
		return theme.Failed.Render("✕"), theme.Failed.Render(containerDetail(row.Container)) + where
	case state.PodStarting:
	}
	return theme.Running.Render(a.ui.spinner()), theme.Running.Render("starting") + where
}

// requestPaneLines list the requests the agent can send, the selected one
// marked, then what it needs from Keycard and how the agent sends it.
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

// trialAbout says what the selected request needs from Keycard, in the
// style to say it in.
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

// noPodLines say why the selected agent has no pod to show, from how the
// sandbox is: it comes up, is read, or the bring-up stopped. Else the pods
// cannot be read, or the pod does not exist yet.
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
			"The bring-up stopped at: "+row.Step.Title+". Press 1 to see why, or u to start again from that step.")
	case a.state.Agents.PodsErr != nil && a.state.Reads.Status.Deployed:
		return lines(theme.Failed.Render("✕ Cannot read the pods"), component.FirstLine(a.state.Agents.PodsErr.Error()))
	}
	return lines(theme.Title.Render("No pod for "+a.agentName()+" yet"),
		"Press u to bring the sandbox up. It creates the pod of every agent.")
}

// responseCaption says what the last request was, what came back, what the
// proxy and Keycard did with it, and the command the agent ran to send it,
// over its body.
func (a *Agents) responseCaption(width int) []string {
	theme := a.ui.Styles
	request := a.request
	// The status goes first, so that a narrow pane cuts the URL, not what
	// came back.
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

// commandCaption shows the whole command the agent's container ran to send
// the last request, with a blank line under it, or nothing before the
// first request.
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

// commandLines lays a command's arguments out in width columns, quoted, so
// that a shell takes the lines pasted as the command. A flag keeps its
// value on its line where they fit, and each line but the last ends in a
// backslash.
func commandLines(args []string, width int) []string {
	var lines []commandLine
	for _, unit := range flagUnits(args) {
		lines = placeUnit(lines, unit, width)
	}
	return joinLines(lines)
}

// placeUnit puts a flag and its value after lines: on the last line when it
// fits there, else on a line of its own, else each argument on lines of
// its own.
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

// wordLines puts one argument on lines of its own, indented unless it
// starts the command. An argument too wide for a line goes in pieces, each
// quoted, and the pieces after the first start their lines: a backslash at
// the end of a line joins them back into one word.
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

// flagUnits joins each flag in args with the value after it, as one unit
// that stays on one line where it fits.
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

// keycardVerdict says what the proxy and Keycard did with the last request,
// from the proxy's record of it.
func (a *Agents) keycardVerdict() string {
	theme := a.ui.Styles
	request := a.request
	label := theme.Dim.Render("Keycard  ")
	records, _ := a.state.Agents.Traffic.Snapshot()
	record, found := proxylog.RecordFor(records, request.trial.method, request.trial.host(), request.trial.path(),
		request.started.Add(-recordSlack))
	switch {
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

// rightPaneKeys scrolls and filters what the pane on the right shows. Esc
// goes back to the pane it shows for.
func (a *Agents) rightPaneKeys(msg tea.KeyPressMsg) tea.Cmd {
	if !key.Matches(msg, a.ui.Keys.Back) || a.ui.Logs.Filter() != "" {
		return a.ui.Logs.Keys(msg, a.ui.Keys)
	}
	if a.showResponse {
		a.FocusPane(AgentPaneRequests)
	} else {
		a.FocusPane(AgentPaneAgent)
	}
	return nil
}

// agentPaneKeys opens the list of agents to switch to, and goes down to
// the requests.
func (a *Agents) agentPaneKeys(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case msg.String() == component.KeyEnter:
		if len(a.state.Agents.List) <= 1 {
			a.ui.Notify("This is the only agent in this demo. Press n to give someone else one.", false)
			return nil
		}
		a.OpenPicker()
		a.host.ShowAgentList()
	case key.Matches(msg, a.ui.Keys.Down):
		a.FocusPane(AgentPaneRequests)
	default:
		return a.ui.Logs.Keys(msg, a.ui.Keys)
	}
	return nil
}

// requestPaneKeys chooses a request and sends it. Up from the first goes
// to the agent.
func (a *Agents) requestPaneKeys(msg tea.KeyPressMsg) tea.Cmd {
	keys := a.ui.Keys
	switch {
	case key.Matches(msg, keys.Up) && a.trial == 0:
		a.FocusPane(AgentPaneAgent)
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

// host is the host the trial goes to.
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

// trialsFor offers one request for each endpoint that says something on its
// own, then one to a host no demo allows. A request to an endpoint with a
// credential asks who the credential belongs to.
func trialsFor(endpoints []sandbox.ScenarioEndpoint) []trial {
	known := map[string]trial{
		"api.github.com:443/":      {method: http.MethodGet, url: "https://api.github.com/user"},
		"openrouter.ai:443/api/v1": {method: http.MethodGet, url: "https://openrouter.ai/api/v1/key"},
		"slack.com:443/api":        {method: http.MethodGet, url: "https://slack.com/api/auth.test"},
		"api.linear.app:443/": {
			method: http.MethodPost, url: "https://api.linear.app/graphql", body: `{"query":"{ viewer { name email } }"}`,
		},
		"pypi.org:443/": {method: http.MethodGet, url: "https://pypi.org/pypi/requests/json"},
	}
	// Git over HTTPS and file downloads say little in one request.
	skipped := map[string]bool{"github.com:443/": true, "files.pythonhosted.org:443/": true}
	trials := make([]trial, 0, len(endpoints)+1)
	for _, endpoint := range endpoints {
		item, ok := known[endpoint.ID()]
		switch {
		case ok:
		case skipped[endpoint.ID()] || !endpoint.Credential:
			continue
		default:
			item = trial{method: http.MethodGet, url: endpoint.URL}
		}
		if endpoint.Credential {
			item.name = endpoint.Name
		}
		trials = append(trials, item)
	}
	return append(trials, trial{method: http.MethodGet, url: "https://example.com/", blocked: true})
}

// excerpt returns the first rows lines of a body, indented when it is
// JSON.
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
