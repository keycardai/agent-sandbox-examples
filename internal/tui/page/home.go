package page

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	// activityRows is the most activity Home shows.
	activityRows = 6
	// failureRows is the most of an error Home shows.
	failureRows = 4
)

// Home is the dashboard. It says what the sandbox does and
// what to do next: the bring-up while it runs, where it stopped when it
// failed, and once it is up, what Keycard did for the agent and what to
// try.
type Home struct {
	shared *Shared
}

// NewHome returns Home.
func NewHome(shared *Shared) *Home { return &Home{shared: shared} }

// View draws Home for how the sandbox is.
func (h *Home) View() string {
	shared := h.shared
	failed := shared.FailedStep()
	switch {
	case shared.Running:
		return h.bringUp()
	case failed != nil:
		return h.failed(failed)
	case !shared.Read:
		return shared.Styles.Muted.Render(shared.spinner() + " Reading the sandbox…")
	case shared.SandboxUp():
		return h.up()
	}
	return h.down()
}

// Hints lists Home's keys for how the sandbox is.
func (h *Home) Hints() []component.Hint {
	shared := h.shared
	switch {
	case shared.Running:
		return []component.Hint{
			{Key: "u", Desc: "stop"},
			{Key: "?", Desc: wordKeys},
			{Key: "q", Desc: wordQuit},
		}
	case shared.FailedStep() != nil:
		return []component.Hint{
			{Key: "u", Desc: "try again"},
			{Key: "?", Desc: wordKeys},
			{Key: "q", Desc: wordQuit},
		}
	case shared.SandboxUp():
		return []component.Hint{
			{Key: "a", Desc: shared.AgentActionName()},
			{Key: "o", Desc: "zone events"},
			{Key: "x", Desc: "shell"},
			{Key: "?", Desc: wordKeys},
			{Key: "q", Desc: wordQuit},
		}
	}
	return []component.Hint{
		{Key: "u", Desc: "bring up"},
		{Key: "?", Desc: wordKeys},
		{Key: "q", Desc: wordQuit},
	}
}

// bringUp shows the steps as they run, and the end of the running step's
// log. A run of the checks alone shows only their log.
func (h *Home) bringUp() string {
	shared := h.shared
	theme := shared.Styles
	current := shared.Steps[shared.StepCursor]
	title := "Bringing up your sandbox"
	note := "The first time takes a few minutes. Quitting stops the step that runs. What is done stays, " +
		"and u runs the steps again."
	if shared.RunFrom > 0 {
		title += " from: " + shared.Steps[shared.RunFrom].Step.Title
	}
	lines := []string{}
	if current.Step.ID == sandbox.StepChecks && shared.RunFrom == shared.StepCursor {
		title, note = "Running the checks", "Each check sends a request from the agent and reads what came back."
	} else {
		lines = append(lines, "")
		for _, row := range shared.Steps {
			lines = append(lines, shared.stepLine(row, min(shared.bodyWidth(), readWidth)))
		}
	}
	lines = append([]string{theme.Title.Render(title), shared.para(theme.Muted, note)}, lines...)
	rows := shared.BodyRows() - lipgloss.Height(strings.Join(lines, "\n")) - half
	if rows > 0 {
		lines = append(lines, "", theme.SectionTitle.Render(strings.ToUpper(current.Step.Title)))
		for _, line := range tail(current.Log, rows-1) {
			lines = append(lines, theme.Faint.Render("│ ")+theme.Dim.Render(line))
		}
	}
	return strings.Join(lines, "\n")
}

// failed says where the bring-up stopped, why, and what to do.
func (h *Home) failed(row *StepRow) string {
	shared := h.shared
	theme := shared.Styles
	lines := []string{theme.Failed.Bold(true).Render("✕ The bring-up stopped at: " + row.Step.Title)}
	if row.Err != nil {
		message := strings.Split(strings.TrimSpace(row.Err.Error()), "\n")
		lines = append(lines, shared.para(theme.Text, strings.Join(message[:min(len(message), failureRows)], "\n")))
		if hint := StepHint(row, row.Err); hint != "" {
			lines = append(lines, shared.para(theme.Hint, "→ "+hint))
		}
	}
	lines = append(lines, "",
		shared.actionLine("u", "Start again from: "+shared.Steps[shared.ResumeIndex()].Step.Title),
		"")
	for _, step := range shared.Steps {
		lines = append(lines, shared.stepLine(step, min(shared.bodyWidth(), readWidth)))
	}
	return strings.Join(lines, "\n")
}

// down says that the sandbox does not run, and how to bring it up.
func (h *Home) down() string {
	shared := h.shared
	theme := shared.Styles
	why := "There is no sandbox yet."
	switch {
	case shared.Status.ClusterErr != nil:
		why = "The cluster might exist. Checking it failed: " + component.FirstLine(shared.Status.ClusterErr.Error())
	case shared.Status.ClusterExists && !shared.Status.Tunnel.Running:
		why = "The cluster exists, but the tunnel is stopped, so Keycard cannot reach the cluster's issuer. " +
			"u starts a new tunnel. A quick tunnel gets a new URL, so the bring-up then asks to recreate the cluster."
	case shared.Status.ClusterExists:
		why = "The cluster exists, but the sandbox was not set up in it."
	}
	return strings.Join([]string{
		theme.Title.Render("○ The sandbox is down"),
		shared.para(theme.Muted, why),
		"",
		shared.actionLine("u", "Bring it up. The first time takes a few minutes."),
		"",
		h.contextLine(),
	}, "\n")
}

// up shows the agent, what Keycard did for it, what to try, and what
// happened last.
func (h *Home) up() string {
	shared := h.shared
	theme := shared.Styles
	lines := h.agentHeadline()
	lines = append(lines, "", theme.SectionTitle.Render("WHAT KEYCARD DID"))
	for _, line := range shared.keycardLines() {
		lines = append(lines, "  "+line)
	}
	lines = append(lines, "", theme.SectionTitle.Render("TRY IT"))
	for _, action := range h.actions() {
		lines = append(lines, "  "+action)
	}
	rows := min(shared.BodyRows()-len(lines)-half, activityRows)
	activity, _ := shared.Logs.Log(SourceActivity).Snapshot()
	if rows > 0 && len(activity) > 0 {
		lines = append(lines, "", theme.SectionTitle.Render("RECENT ACTIVITY"))
		for _, line := range shared.colorActivity(activity[max(len(activity)-rows, 0):]) {
			lines = append(lines, "  "+line)
		}
	}
	return strings.Join(lines, "\n")
}

// colorActivity colors each activity row by its mark.
func (s *Shared) colorActivity(lines []string) []string {
	theme := s.Styles
	marks := map[string]func(...string) string{
		"✓": theme.OK.Render, "✕": theme.Failed.Render, "▸": theme.Running.Render, "•": theme.Accent.Render,
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		when, rest, _ := strings.Cut(line, "  ")
		mark, text, _ := strings.Cut(rest, " ")
		render, ok := marks[mark]
		if !ok {
			out[i] = line
			continue
		}
		out[i] = theme.Dim.Render(when) + "  " + render(mark) + " " + theme.Text.Render(text)
	}
	return out
}

// agentHeadline says whether the agent runs, who it acts as, and that it
// does not hold any secrets.
func (h *Home) agentHeadline() []string {
	shared := h.shared
	theme := shared.Styles
	settings := shared.Session.Local().Settings
	person, _ := shared.SelectedAgent()
	user := cmp.Or(person.Email, "the zone's user")
	about := "It acts as " + user + " in zone " + shortHost(settings[sandbox.KeyZoneURL]) + ", and does not " +
		"hold any secrets. The egress proxy asks Keycard for " + user + "'s credential on each request, and adds it on " +
		"the way out."
	name := shared.AgentName()
	if person.Email == "" {
		name = "The agent"
	}
	agent, found := shared.Container(AgentContainer)
	switch {
	case found && agent.Container.Ready:
		return []string{theme.OK.Bold(true).Render("● " + name + " is running"), shared.para(theme.Muted, about)}
	case found && (agent.Container.State == StateTerminated || slices.Contains(crashReasons(), agent.Container.Reason)):
		return []string{
			theme.Failed.Bold(true).Render("✕ " + name + " is not running: " + containerDetail(agent.Container)),
			shared.para(theme.Muted, "Press R to restart the pod."),
		}
	case !found && shared.PodsErr != nil:
		return []string{
			theme.Failed.Bold(true).Render("✕ Cannot read the sandbox's pods"),
			shared.para(theme.Muted, component.FirstLine(shared.PodsErr.Error())),
		}
	case person.Pod == "":
		return []string{
			theme.Dim.Bold(true).Render("○ " + name + " is not up yet"),
			shared.para(theme.Muted, "Press u to bring the sandbox up again. It creates every agent's pod."),
		}
	}
	return []string{
		theme.Running.Bold(true).Render(shared.spinner() + " " + name + "'s pod is starting"),
		shared.para(theme.Muted, about),
	}
}

// actions lists what to try, for how the demo runs its agent.
func (h *Home) actions() []string {
	shared := h.shared
	actions := []string{}
	scenario := shared.Scenario()
	switch {
	case scenario == nil:
	case scenario.Run.Mode == sandbox.ModeShell && len(scenario.Run.Command) > 0:
		actions = append(actions, shared.actionLine("a", "Open "+scenario.Run.Command[0]+
			" in the agent, and ask for something that needs your accounts"))
	default:
		actions = append(actions, shared.actionLine("c", "Run the checks again"+h.checksScore()))
	}
	actions = append(actions,
		shared.actionLine("o", "Open the zone's events in the Keycard console"),
		shared.actionLine("x", "Open a shell in the agent"),
	)
	return actions
}

// checksScore says how the last checks went, or "".
func (h *Home) checksScore() string {
	for _, row := range h.shared.Steps {
		if row.Step.ID == sandbox.StepChecks && row.Total > 0 {
			return fmt.Sprintf(" (last time %d of %d passed)", row.Passed, row.Total)
		}
	}
	return ""
}

// contextLine names the demo, the zone, the user and the cluster.
func (h *Home) contextLine() string {
	shared := h.shared
	theme := shared.Styles
	settings := shared.Session.Local().Settings
	parts := make([]string, 0, half*half)
	for _, item := range [][2]string{
		{"demo", cmp.Or(settings[sandbox.KeyScenario], sandbox.DefaultScenario)},
		{"zone", shortHost(settings[sandbox.KeyZoneURL])},
		{wordAgent, h.agentContext()},
		{"cluster", cmp.Or(settings[sandbox.KeyKubeContext], settings[sandbox.KeyCluster])},
	} {
		if item[1] != "" {
			parts = append(parts, theme.Dim.Render(item[0]+" ")+theme.Muted.Render(item[1]))
		}
	}
	return strings.Join(parts, theme.Faint.Render("  ·  "))
}

// agentContext names the selected agent's person, and how many agents
// there are when there is more than one.
func (h *Home) agentContext() string {
	agent, _ := h.shared.SelectedAgent()
	if len(h.shared.Agents) > 1 {
		return agent.Email + " (" + component.Plural(len(h.shared.Agents), wordAgent) + ")"
	}
	return agent.Email
}

// tail returns a log's last rows lines, without the blank ones at its end.
func tail(log *component.Log, rows int) []string {
	lines, _ := log.Snapshot()
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines[max(len(lines)-rows, 0):]
}

// StepHint says what to do about a failed step. Checks that failed because
// the agent could not resolve a name get their own hint: the cluster's DNS
// was not ready.
func StepHint(row *StepRow, err error) string {
	if row.Step.ID == sandbox.StepIssuer && strings.Contains(err.Error(), "trycloudflare.com") &&
		strings.Contains(err.Error(), "no such host") {
		return "The quick tunnel's URL no longer exists. Cloudflare deletes a quick tunnel that loses its " +
			"connection for a while, as when the laptop sleeps. Press u: the bring-up starts a new tunnel, " +
			"and asks to recreate the cluster for its URL."
	}
	lines, _ := row.Log.Snapshot()
	if row.Step.ID == sandbox.StepChecks && slices.ContainsFunc(lines, func(line string) bool {
		return strings.Contains(line, "Could not resolve host")
	}) {
		return "The agent could not resolve the hosts' names, so the cluster's DNS was not ready. " +
			"Check kube-system/coredns, then press c to run the checks again."
	}
	return HintFor(err)
}

// HintFor says what to do about err, in one line, or returns "".
func HintFor(err error) string {
	hints := []struct {
		target error
		hint   string
	}{
		{sandbox.ErrDocker, "Start Docker (OrbStack or Docker Desktop), then press u."},
		{sandbox.ErrImages, "Do what the error says, then press u to start again from this step."},
		{sandbox.ErrBuild, "A build that cannot download anything usually means Docker's network is down: " +
			"restart OrbStack or Docker Desktop, then press u."},
		{sandbox.ErrOtherIssuer, "The cluster belongs to an older tunnel URL. Press u and recreate it."},
		{sandbox.ErrBrokenCluster, "Your kubeconfig has no context for the cluster. Press u and recreate it."},
		{zonesetup.ErrPolicySetActive, "Run sandbox init, and allow replacing the active policy set."},
		{zonesetup.ErrAuth, "Keycard refused the service account. Check KEYCARD_CLIENT_ID and KEYCARD_CLIENT_SECRET."},
		{zonesetup.ErrMissingSecret, "A credential has no value. Run sandbox init to enter it."},
		{zonesetup.ErrValues, "A setting is missing. Run sandbox init to give each credential a value."},
		{zonesetup.ErrUserNotFound, "The user has not signed in to the zone yet. Invite them in the Keycard console."},
		{
			zonesetup.ErrProviderNotFound,
			"The zone lacks a provider the demo names. Run sandbox init and choose the demo again.",
		},
		{sandbox.ErrSettings, "Run sandbox init to set the sandbox up."},
		{sandbox.ErrScenario, "Run sandbox init and choose a demo."},
		{sandbox.ErrService, "Read the service's log: local/out/tunnel.log or local/out/mirror.log."},
		{
			sandbox.ErrIssuer,
			"Keycard cannot reach the cluster's issuer. Read local/out/tunnel.log and local/out/mirror.log.",
		},
		{sandbox.ErrPod, "Run sandbox logs to follow the agent's log."},
		{sandbox.ErrCheck, "The sandbox is up. Read the failed check, then press c to run the checks again."},
		{sandbox.ErrAgent, "Run sandbox logs to follow the agent's log."},
	}
	for _, h := range hints {
		if errors.Is(err, h.target) {
			return h.hint
		}
	}
	return ""
}
