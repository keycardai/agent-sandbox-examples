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
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
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
	view
}

// NewHome returns Home.
func NewHome(ui *UI, sandbox *state.Sandbox) *Home { return &Home{view{ui: ui, state: sandbox}} }

// View draws Home for how the sandbox is.
func (h *Home) View() string {
	switch phase, row := h.state.Phase(); phase {
	case state.PhaseBringingUp, state.PhaseChecking:
		return h.bringUp()
	case state.PhaseStopping:
		return h.ui.Styles.Muted.Render(h.ui.spinner() + " Stopping everything…")
	case state.PhaseStopped, state.PhaseFailed:
		return h.failed(row)
	case state.PhaseReading:
		return h.ui.Styles.Muted.Render(h.ui.spinner() + " Reading the sandbox…")
	case state.PhaseUp:
		return h.up()
	case state.PhaseDown:
	}
	return h.down()
}

// Hints lists Home's keys for how the sandbox is, then the help and quit.
func (h *Home) Hints() []component.Hint {
	return append(h.phaseHints(), component.Hint{Key: "?", Desc: wordKeys}, component.Hint{Key: "q", Desc: wordQuit})
}

// phaseHints lists the keys for how the sandbox is.
func (h *Home) phaseHints() []component.Hint {
	switch phase, _ := h.state.Phase(); phase {
	case state.PhaseBringingUp, state.PhaseChecking:
		return []component.Hint{{Key: "u", Desc: "stop"}}
	case state.PhaseStopping:
		return nil
	case state.PhaseStopped, state.PhaseFailed:
		again := "try again"
		if phase == state.PhaseStopped {
			again = "start again"
		}
		return []component.Hint{{Key: "u", Desc: again}}
	case state.PhaseUp:
		return []component.Hint{
			{Key: "a", Desc: h.AgentActionName()},
			{Key: "o", Desc: "zone events"},
			{Key: "x", Desc: "shell"},
		}
	case state.PhaseReading, state.PhaseDown:
	}
	return []component.Hint{{Key: "u", Desc: "bring up"}}
}

// bringUp shows the steps as they run, and the end of the running step's
// log. A run of the checks alone shows only their log.
func (h *Home) bringUp() string {
	theme := h.ui.Styles
	current := h.state.Run.Steps[h.state.Run.Cursor]
	title := "Bringing up your sandbox"
	note := "The first time takes a few minutes. Quitting stops the step that runs. What is done stays, " +
		"and u runs the steps again."
	if from := h.state.Run.Job.From; from > 0 {
		title += " from: " + h.state.Run.Steps[from].Step.Title
	}
	lines := []string{}
	if h.state.Run.Job.Is(state.JobChecks) {
		title, note = "Running the checks", "Each check sends a request from the agent and reads what came back."
	} else {
		lines = append(lines, "")
		for _, row := range h.state.Run.Steps {
			lines = append(lines, h.stepLine(row, min(h.ui.bodyWidth(), readWidth)))
		}
	}
	lines = append([]string{theme.Title.Render(title), h.ui.para(theme.Muted, note)}, lines...)
	rows := h.ui.BodyRows() - lipgloss.Height(strings.Join(lines, "\n")) - half
	if rows > 0 {
		lines = append(lines, "", theme.SectionTitle.Render(strings.ToUpper(current.Step.Title)))
		for _, line := range tail(current.Log, rows-1) {
			lines = append(lines, theme.Faint.Render("│ ")+theme.Dim.Render(line))
		}
	}
	return strings.Join(lines, "\n")
}

// failed says where the bring-up stopped, why when a step failed, and what
// to do.
func (h *Home) failed(row *state.StepRow) string {
	theme := h.ui.Styles
	lines := []string{theme.Failed.Bold(true).Render("✕ The bring-up stopped at: " + row.Step.Title)}
	switch {
	case row.Stopped():
		lines[0] = theme.Text.Bold(true).Render("○ You stopped the bring-up at: " + row.Step.Title)
	case row.Err != nil:
		message := strings.Split(strings.TrimSpace(row.Err.Error()), "\n")
		lines = append(lines, h.ui.para(theme.Text, strings.Join(message[:min(len(message), failureRows)], "\n")))
		if hint := StepHint(row, row.Err); hint != "" {
			lines = append(lines, h.ui.para(theme.Hint, "→ "+hint))
		}
	}
	lines = append(lines, "",
		h.ui.actionLine("u", "Start again from: "+h.state.Run.Steps[h.state.ResumeIndex()].Step.Title),
		"")
	for _, step := range h.state.Run.Steps {
		lines = append(lines, h.stepLine(step, min(h.ui.bodyWidth(), readWidth)))
	}
	return strings.Join(lines, "\n")
}

// down says that the sandbox does not run, and how to bring it up.
func (h *Home) down() string {
	theme := h.ui.Styles
	why := "There is no sandbox yet."
	switch {
	case h.state.Reads.Status.ClusterErr != nil:
		why = "The cluster might exist. Checking it failed: " + component.FirstLine(h.state.Reads.Status.ClusterErr.Error())
	case h.state.Reads.Status.ClusterExists && h.state.StartsTunnel() && !h.state.Reads.Status.Tunnel.Running:
		why = "The cluster exists, but the tunnel is stopped, so Keycard cannot reach the cluster's issuer. " +
			"u starts a new tunnel. A quick tunnel gets a new URL, so the bring-up then asks to recreate the cluster."
	case h.state.Reads.Status.ClusterExists:
		why = "The cluster exists, but the sandbox was not set up in it."
	}
	return strings.Join([]string{
		theme.Title.Render("○ The sandbox is down"),
		h.ui.para(theme.Muted, why),
		"",
		h.ui.actionLine("u", "Bring it up. The first time takes a few minutes."),
		"",
		h.contextLine(),
	}, "\n")
}

// up shows the agent, what Keycard did for it, what to try, and what
// happened last.
func (h *Home) up() string {
	theme := h.ui.Styles
	lines := h.agentHeadline()
	lines = append(lines, "", theme.SectionTitle.Render("WHAT KEYCARD DID"))
	for _, line := range h.keycardLines() {
		lines = append(lines, "  "+line)
	}
	lines = append(lines, "", theme.SectionTitle.Render("TRY IT"))
	for _, action := range h.actions() {
		lines = append(lines, "  "+action)
	}
	rows := min(h.ui.BodyRows()-len(lines)-half, activityRows)
	activity, _ := h.ui.Logs.Log(state.SourceActivity).Snapshot()
	if rows > 0 && len(activity) > 0 {
		lines = append(lines, "", theme.SectionTitle.Render("RECENT ACTIVITY"))
		for _, line := range h.ui.colorActivity(activity[max(len(activity)-rows, 0):]) {
			lines = append(lines, "  "+line)
		}
	}
	return strings.Join(lines, "\n")
}

// colorActivity colors each activity row by its mark.
func (u *UI) colorActivity(lines []string) []string {
	theme := u.Styles
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
	theme := h.ui.Styles
	person, _ := h.state.Selected()
	user := cmp.Or(person.Email, "the zone's user")
	about := "It acts as " + user + " in zone " + shortHost(h.state.Setting(sandbox.KeyZoneURL)) + ", and does not " +
		"hold any secrets. The egress proxy asks Keycard for " + user + "'s credential on each request, and adds it on " +
		"the way out."
	name := h.agentName()
	if person.Email == "" {
		name = "The agent"
	}
	switch pod, row := h.state.PodState(person); pod {
	case state.PodRunning:
		return []string{theme.OK.Bold(true).Render("● " + name + " is running"), h.ui.para(theme.Muted, about)}
	case state.PodAgentFailed:
		return []string{
			theme.Failed.Bold(true).Render("✕ " + name + " is not running: " + containerDetail(row.Container)),
			h.ui.para(theme.Muted, "Press R to restart the pod."),
		}
	case state.PodUnread:
		return []string{
			theme.Failed.Bold(true).Render("✕ Cannot read the sandbox's pods"),
			h.ui.para(theme.Muted, component.FirstLine(h.state.Agents.PodsErr.Error())),
		}
	case state.PodNone:
		return []string{
			theme.Dim.Bold(true).Render("○ " + name + " is not up yet"),
			h.ui.para(theme.Muted, "Press u to bring the sandbox up again. It creates every agent's pod."),
		}
	case state.PodMissing, state.PodStarting:
	}
	return []string{
		theme.Running.Bold(true).Render(h.ui.spinner() + " " + name + "'s pod is starting"),
		h.ui.para(theme.Muted, about),
	}
}

// actions lists what to try, for how the demo runs its agent.
func (h *Home) actions() []string {
	actions := []string{}
	scenario := h.state.LoadedDemo()
	switch {
	case scenario == nil:
	case scenario.Run.Mode == sandbox.ModeShell && len(scenario.Run.Command) > 0:
		actions = append(actions, h.ui.actionLine("a", "Open "+scenario.Run.Command[0]+
			" in the agent, and ask for something that needs your accounts"))
	default:
		actions = append(actions, h.ui.actionLine("c", "Run the checks again"+h.checksScore()))
	}
	actions = append(actions,
		h.ui.actionLine("o", "Open the zone's events in the Keycard console"),
		h.ui.actionLine("x", "Open a shell in the agent"),
	)
	return actions
}

// checksScore says how the last checks went, or "".
func (h *Home) checksScore() string {
	for _, row := range h.state.Run.Steps {
		if row.Step.ID == sandbox.StepChecks && row.Total > 0 {
			return fmt.Sprintf(" (last time %d of %d passed)", row.Passed, row.Total)
		}
	}
	return ""
}

// contextLine names the demo, the zone, the user and the cluster.
func (h *Home) contextLine() string {
	theme := h.ui.Styles
	parts := make([]string, 0, half*half)
	for _, item := range [][2]string{
		{"demo", h.state.DemoName()},
		{"zone", shortHost(h.state.Setting(sandbox.KeyZoneURL))},
		{wordAgent, h.agentContext()},
		{"cluster", cmp.Or(h.state.Setting(sandbox.KeyKubeContext), h.state.Setting(sandbox.KeyCluster))},
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
	agent, _ := h.state.Selected()
	if len(h.state.Agents.List) > 1 {
		return agent.Email + " (" + component.Plural(len(h.state.Agents.List), wordAgent) + ")"
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

// StepHint says what to do about a failed step. An issuer check that
// cannot resolve the quick tunnel's name, and checks in which the agent
// could not resolve a name, get their own hints.
func StepHint(row *state.StepRow, err error) string {
	if row.Step.ID == sandbox.StepIssuer && strings.Contains(err.Error(), "trycloudflare.com") &&
		strings.Contains(err.Error(), "no such host") {
		return "The quick tunnel's URL no longer exists. Press u: the bring-up starts a new tunnel, " +
			"and asks to recreate the cluster for its URL."
	}
	lines, _ := row.Log.Snapshot()
	if row.Step.ID == sandbox.StepChecks && slices.ContainsFunc(lines, func(line string) bool {
		return strings.Contains(line, "Could not resolve host")
	}) {
		return "The agent could not resolve a host's name. Check kube-system/coredns, then press c to run " +
			"the checks again."
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
		{sandbox.ErrBuild, "Fix what the build's output says, then press u."},
		{sandbox.ErrOtherIssuer, "The cluster was made for another issuer URL. Press u and recreate it."},
		{sandbox.ErrBrokenCluster, "Your kubeconfig has no context for the cluster. Press u and recreate it."},
		{zonesetup.ErrPolicySetActive, "Run sandbox init, and allow replacing the active policy set."},
		{zonesetup.ErrAuth, "Keycard refused the service account. Check KEYCARD_CLIENT_ID and KEYCARD_CLIENT_SECRET."},
		{zonesetup.ErrMissingSecret, "A credential has no value. Run sandbox init to enter it."},
		{zonesetup.ErrValues, "Run sandbox init to fix the setting the error names."},
		{zonesetup.ErrUserNotFound, "The zone has no user with this email. Add them in the Keycard console."},
		{
			zonesetup.ErrProviderNotFound,
			"The zone lacks a provider the demo names. Run sandbox init and choose the demo again.",
		},
		{sandbox.ErrSettings, "Run sandbox init to set the sandbox up."},
		{sandbox.ErrScenario, "Run sandbox init and choose a demo."},
		{sandbox.ErrService, "Read the service's log: local/out/tunnel.log or local/out/mirror.log."},
		{sandbox.ErrIssuer, "Read local/out/tunnel.log and local/out/mirror.log, then press u."},
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
