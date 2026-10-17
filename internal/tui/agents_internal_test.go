package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
)

const (
	// The team's other people: one whose pod starts, and one whose agent
	// has not come up yet.
	sampleStarting = "grace@example.com"
	sampleNotUp    = "linus@example.com"
	startingPod    = "p9x4starts"
)

// teamModel is upModel with two more agents: one whose pod is starting,
// and one that has not come up.
func teamModel(t *testing.T) *model {
	t.Helper()
	root := upModel(t)
	root.state.Agents.List = append(root.state.Agents.List,
		sandbox.Agent{Email: sampleStarting, Pod: startingPod}, sandbox.Agent{Email: sampleNotUp})
	root.state.Agents.Pods = append(slices.Clone(root.state.Agents.Pods),
		state.ContainerRow{Pod: startingPod, Container: sandbox.ContainerStatus{Name: state.ProxyContainer, Ready: true}},
		state.ContainerRow{Pod: startingPod, Container: sandbox.ContainerStatus{
			Name: state.AgentContainer, State: "waiting", Reason: "ContainerCreating",
		}})
	return root
}

// TestAgentsList shows the agent the tabs show, and enter lists every
// agent with how its pod is.
func TestAgentsList(t *testing.T) {
	t.Parallel()
	root := teamModel(t)
	root.Update(press("2"))
	view := ansi.Strip(root.View().Content)
	for _, want := range []string{sampleUser, "running · pod " + samplePod, "1 of 3"} {
		if !strings.Contains(view, want) {
			t.Errorf("Agents tab lacks %q:\n%s", want, view)
		}
	}
	if act(root, tea.KeyPressMsg{Code: tea.KeyEnter}); root.overlay.kind != overlayAgents {
		t.Fatalf("enter on the agent: overlay %v, want the list of agents", root.overlay.kind)
	}
	view = ansi.Strip(root.View().Content)
	for _, want := range []string{
		"Switch agent", sampleStarting, "starting · pod " + startingPod, sampleNotUp, "not brought up yet",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the list of agents lacks %q:\n%s", want, view)
		}
	}
}

// TestSwitchAgent switches to the agent chosen from the list. Every tab
// then shows its pod alone, says whose agent it shows, and forgets the
// last request.
func TestSwitchAgent(t *testing.T) {
	t.Parallel()
	root := teamModel(t)
	root.Update(press("2"))
	root.Update(press("s"))
	root.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	pick(root, 1)
	if root.overlay.kind != overlayNone || root.session.Selected() != sampleStarting ||
		root.shownPod != startingPod {
		t.Fatalf("chose %q with pod %q, overlay %v; want %s", root.session.Selected(), root.shownPod,
			root.overlay.kind, sampleStarting)
	}
	if slices.ContainsFunc(
		root.state.SelectedPods(),
		func(row state.ContainerRow) bool { return row.Pod != startingPod },
	) {
		t.Errorf("pods = %v, want only %s's", root.state.SelectedPods(), startingPod)
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "Nothing sent yet") {
		t.Errorf("the request of %s still shows for %s:\n%s", sampleUser, sampleStarting, view)
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	for _, tab := range []string{"1", "3", "4"} {
		root.Update(press(tab))
		if view := ansi.Strip(root.View().Content); !strings.Contains(view, "Showing "+sampleStarting+"'s agent, 2 of 3") {
			t.Errorf("tab %s does not say whose agent it shows:\n%s", tab, view)
		}
	}
	root.Update(press("2"))
	pick(root, 1)
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "No pod for "+sampleNotUp+"'s agent yet") {
		t.Errorf("an agent that has not come up:\n%s", view)
	}
}

// pick opens the list of agents, moves down steps and switches.
func pick(root *model, steps int) {
	act(root, tea.KeyPressMsg{Code: tea.KeyEnter})
	for range steps {
		root.Update(press("j"))
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
}

// TestOneAgent leaves out which agent a tab shows when there is only one,
// and offers no X: that agent is the picked person's, which X cannot remove.
// On enter, it says how to add an agent, as there is none to switch to.
func TestOneAgent(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	if view := ansi.Strip(root.View().Content); strings.Contains(view, "Showing ") {
		t.Errorf("Home says which agent it shows, with one agent:\n%s", view)
	}
	root.Update(press("2"))
	if slices.ContainsFunc(root.agents.Hints(), func(hint component.Hint) bool { return hint.Key == "X" }) {
		t.Error("the only agent offers X to remove it")
	}
	if root.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); root.overlay.kind != overlayNone ||
		!strings.Contains(root.ui.Toast.Text, "only agent") {
		t.Errorf("enter with one agent: overlay %v, notice %q", root.overlay.kind, root.ui.Toast.Text)
	}
}

// TestRemoveAgentKey keeps the agent of the person setup picked, and asks
// before removing another person's.
func TestRemoveAgentKey(t *testing.T) {
	t.Parallel()
	team := teamModel(t)
	team.Update(press("2"))
	team.Update(press("X"))
	if team.overlay.kind != overlayNone || !strings.Contains(team.ui.Toast.Text, "Every demo has an agent") {
		t.Errorf("X on the picked person's agent: overlay %v, notice %q", team.overlay.kind, team.ui.Toast.Text)
	}
	team.session.SelectAgent(sampleStarting)
	if !slices.ContainsFunc(team.agents.Hints(), func(hint component.Hint) bool { return hint.Key == "X" }) {
		t.Error("another person's agent offers no X to remove it")
	}
	team.Update(press("X"))
	if team.overlay.kind != overlayConfirm || team.overlay.title != "Remove "+sampleStarting+"'s agent?" {
		t.Fatalf("X on another person's agent: overlay %+v, want a confirmation", team.overlay)
	}
	team.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if team.overlay.kind != overlayNone || team.ui.Logs.Log(state.SourceAction).Len() != 0 {
		t.Error("enter removed the agent, want it to keep it")
	}
}

// TestAddAgentNeedsSetup refuses to add an agent before setup has run.
func TestAddAgentNeedsSetup(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.screen = screenDashboard
	if root.Update(
		press("n"),
	); root.state.Run.Job.Busy() ||
		!strings.Contains(root.ui.Toast.Text, "Set the sandbox up first") {
		t.Errorf("n with no settings: job %+v, notice %q", root.state.Run.Job, root.ui.Toast.Text)
	}
}

func snapshotAgents(t *testing.T) *model {
	t.Helper()
	root := teamModel(t)
	root.Update(tea.WindowSizeMsg{Width: 150, Height: 42})
	root.ui.Frame = 3
	root.Update(press("2"))
	settle(t, root, state.AgentContainer)
	return root
}

func snapshotAgentPicker(t *testing.T) *model {
	t.Helper()
	root := snapshotAgents(t)
	act(root, tea.KeyPressMsg{Code: tea.KeyEnter})
	return root
}

// act gives msg to root, then the intents a page answers with, such as a
// log to follow, as the program does.
func act(root *model, msg tea.Msg) {
	_, cmd := root.Update(msg)
	for _, intent := range intents(cmd) {
		root.Update(intent)
	}
}

// intents runs cmd, and returns the intents of the pages it sends. Other
// messages, such as a tick, are left out.
func intents(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		var out []tea.Msg
		for _, next := range msg {
			out = append(out, intents(next)...)
		}
		return out
	case page.FollowMsg, page.ShowAgentsMsg:
		return []tea.Msg{msg}
	}
	return nil
}
