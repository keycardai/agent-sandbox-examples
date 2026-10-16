// Package page holds the TUI's screens. Each page keeps its own state,
// draws itself, and takes the keys that move within it. The root model
// routes messages and keys to the pages, and does the work that spans
// them. What the pages share, such as the sandbox as the TUI last read it
// and the logs, is in Shared.
package page

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/style"
)

// SourceActivity names the activity log. StepSource names a step's log.
const SourceActivity = "activity"

// The containers of the sandbox's pod, and the state of one that finished.
const (
	AgentContainer  = "agent"
	ProxyContainer  = "egress-proxy"
	StateTerminated = "terminated"
)

const (
	// toastFor is how long a notice stays in the footer.
	toastFor = 6 * time.Second
	// readWidth bounds paragraphs, so they stay readable on a wide window.
	readWidth = 76
	// shortID is how much of a zone's ID the header shows.
	shortID = 8
	// half halves a size, to center or split it.
	half = 2
	// wordQuit, wordKeys and wordAgent are words several pages show.
	wordQuit  = "quit"
	wordKeys  = "keys"
	wordAgent = "agent"
)

// StepState is where one bring-up step is.
type StepState int

// The states of a bring-up step.
const (
	StepPending StepState = iota
	StepRunning
	StepDone
	StepFailed
)

// StepRow is one bring-up step, as the bring-up last reported it.
type StepRow struct {
	Step           sandbox.Step
	State          StepState
	Started, Ended time.Time
	Err            error
	Log            *component.Log
	// Passed and Total count the checks, for the checks step.
	Passed, Total int
}

// ContainerRow is one container of a pod.
type ContainerRow struct {
	Pod       string
	Container sandbox.ContainerStatus
}

// Shared is what the pages share: the session, the window, the logs and
// the notice in the footer, and the sandbox as the TUI last read it.
// The root model writes it, and the pages read it. Update runs on one
// goroutine, so it does not need a lock.
type Shared struct {
	Session *sandbox.Session
	Styles  style.Styles
	Keys    component.KeyMap
	// Width and Height are the window's.
	Width, Height int
	// Frame counts the redraws, and turns the spinners.
	Frame int
	// Logs holds every log.
	Logs  *component.LogView
	Toast component.Toast

	// Steps are the bring-up's steps, and StepCursor the one it is on.
	// Running is set while a bring-up runs, and RunFrom is the step it
	// started at.
	Steps      []*StepRow
	StepCursor int
	Running    bool
	RunFrom    int

	// Status is the sandbox as the TUI last read it. Read is set once the
	// first read came back.
	Status sandbox.Status
	Read   bool
	// Agents lists an agent for each person the settings give one, and
	// AllPods the containers of every pod. Pods holds those of the selected
	// agent's pod, which Home shows. PodsErr is the last read's error.
	Agents  []sandbox.Agent
	AllPods []ContainerRow
	Pods    []ContainerRow
	PodsErr error
	// Traffic holds the proxy's decisions, from its audit log.
	Traffic proxylog.Store

	// Loaded is the demo the settings name, read once.
	Loaded *sandbox.Scenario
}

// NewShared returns what the pages share for a session, with the steps of
// its settings.
func NewShared(session *sandbox.Session) *Shared {
	shared := &Shared{Session: session, Styles: style.New(true), Keys: component.NewKeyMap()}
	shared.Logs = component.NewLogView(func(_ string, lines []string) []string { return lines })
	shared.ResetSteps()
	return shared
}

// StepSource names a step's log.
func StepSource(id sandbox.StepID) string { return "step " + string(id) }

// ResetSteps lists the steps for the current settings, all pending.
func (s *Shared) ResetSteps() {
	s.Steps = nil
	for _, step := range s.Session.Steps() {
		s.Steps = append(s.Steps, &StepRow{Step: step, Log: s.Logs.Log(StepSource(step.ID))})
	}
	s.StepCursor = min(s.StepCursor, max(len(s.Steps)-1, 0))
}

// Record adds an event to the activity log: the time, a mark and what
// happened. ✓ is done, ✕ failed, ▸ started and • a note.
func (s *Shared) Record(mark, text string) {
	s.Logs.Log(SourceActivity).Add(time.Now().Format(time.TimeOnly) + "  " + mark + " " + text)
}

// Notify shows text in the footer for a while.
func (s *Shared) Notify(text string, isErr bool) {
	s.Toast = component.Toast{Text: text, Err: isErr, Until: time.Now().Add(toastFor)}
}

// PageUnder draws a body under header and over the footer, indented, and
// cut to the window.
func (s *Shared) PageUnder(header, body string, hints []component.Hint) string {
	return component.Screen(s.Width, s.Height, header, body, component.Footer(s.Styles, s.Width, hints, s.Toast))
}

// BodyRows is the rows a page's body has under its top padding.
func (s *Shared) BodyRows() int { return max(s.Height-component.ChromeRows-1, 1) }

// Scenario returns the demo the settings name, or nil when it does not
// load. It reads the demo once per name.
func (s *Shared) Scenario() *sandbox.Scenario {
	name := cmp.Or(s.Session.Local().Settings[sandbox.KeyScenario], sandbox.DefaultScenario)
	if s.Loaded != nil && s.Loaded.Name == name {
		return s.Loaded
	}
	scenario, err := s.Session.Scenario()
	if err != nil {
		return nil
	}
	s.Loaded = scenario
	return scenario
}

// SelectedAgent returns the agent the session has selected, and its index
// in the list, or -1 when the list does not have it.
func (s *Shared) SelectedAgent() (sandbox.Agent, int) {
	email := s.Session.Selected()
	for i, agent := range s.Agents {
		if agent.Email == email {
			return agent, i
		}
	}
	return sandbox.Agent{Email: email}, -1
}

// AgentName names the selected agent in a sentence, by its person.
func (s *Shared) AgentName() string {
	if agent, _ := s.SelectedAgent(); agent.Email != "" {
		return agent.Email + "'s agent"
	}
	return "the agent"
}

// Container finds the selected pod's container of name.
func (s *Shared) Container(name string) (ContainerRow, bool) {
	index := slices.IndexFunc(s.Pods, func(row ContainerRow) bool { return row.Container.Name == name })
	if index < 0 {
		return ContainerRow{}, false
	}
	return s.Pods[index], true
}

// FailedStep returns the step a bring-up stopped at, or nil. A failed check
// does not count: the sandbox is up.
func (s *Shared) FailedStep() *StepRow {
	for _, row := range s.Steps {
		if row.State == StepFailed && row.Step.ID != sandbox.StepChecks {
			return row
		}
	}
	return nil
}

// SandboxUp reports whether the sandbox runs: the zone was set up, the
// cluster exists, and the zone reaches the cluster's issuer. A local
// cluster needs the tunnel for that, and an existing cluster publishes its
// issuer itself.
func (s *Shared) SandboxUp() bool {
	return s.Status.Deployed && s.Status.ClusterExists && (s.Status.Tunnel.Running || s.Session.Local().Existing())
}

// ResumeIndex is where a bring-up starts again: at the step that failed,
// or from the beginning. While the tunnel does not run, it starts at the
// tunnel, because every later step needs its URL. A quick tunnel's new URL
// then makes the cluster step offer to recreate the cluster.
func (s *Shared) ResumeIndex() int {
	if len(s.Steps) > 0 && s.Steps[0].Step.ID == sandbox.StepTunnel && !s.Status.Tunnel.Running {
		return 0
	}
	for i, row := range s.Steps {
		if row.State == StepFailed {
			return i
		}
	}
	return 0
}

// AgentActionName says what a does, for the footer.
func (s *Shared) AgentActionName() string {
	scenario := s.Scenario()
	switch {
	case scenario == nil:
		return wordAgent
	case scenario.Run.Mode == sandbox.ModeShell && len(scenario.Run.Command) > 0:
		return "open " + scenario.Run.Command[0]
	case scenario.Run.Mode == sandbox.ModeBackground:
		return "agent log"
	}
	return "open agent"
}

func (s *Shared) spinner() string { return component.Spinner(s.Frame) }

// bodyWidth is the width of a page's body inside the indent on both sides.
func (s *Shared) bodyWidth() int { return max(s.Width-component.PageIndent*half, 1) }

// para wraps text to the body's width, or a readable width, in st.
func (s *Shared) para(st lipgloss.Style, text string) string {
	return st.Width(min(s.bodyWidth(), readWidth+readWidth/half)).Render(text)
}

// actionLine is a key and what it does.
func (s *Shared) actionLine(key, text string) string {
	return s.Styles.KeyChip.Render(key) + " " + s.Styles.Text.Render(text)
}

// endpoints returns the demo's endpoints, or none.
func (s *Shared) endpoints() []sandbox.ScenarioEndpoint {
	if scenario := s.Scenario(); scenario != nil {
		return scenario.EndpointList()
	}
	return nil
}

// stepLine draws one step: its icon, its title, and how long it took.
func (s *Shared) stepLine(row *StepRow, width int) string {
	theme := s.Styles
	icon, elapsed := s.stepIcon(row), s.stepElapsed(row)
	space := max(width-lipgloss.Width(icon)-1-lipgloss.Width(elapsed)-1, 1)
	title := ansi.Truncate(row.Step.Title, space, "…")
	lineStyle := theme.Muted
	switch row.State {
	case StepRunning:
		lineStyle = theme.Text.Bold(true)
	case StepFailed:
		lineStyle = theme.Failed
	case StepDone:
		lineStyle = theme.Text
	case StepPending:
	}
	return icon + " " + lineStyle.Render(title) + strings.Repeat(" ", max(space-lipgloss.Width(title), 0)) + " " +
		elapsed
}

func (s *Shared) stepIcon(row *StepRow) string {
	theme := s.Styles
	switch row.State {
	case StepRunning:
		return theme.Running.Render(s.spinner())
	case StepDone:
		return theme.OK.Render("✓")
	case StepFailed:
		return theme.Failed.Render("✕")
	case StepPending:
	}
	return theme.Faint.Render("·")
}

// stepElapsed says how long a step took, or the checks' score.
func (s *Shared) stepElapsed(row *StepRow) string {
	theme := s.Styles
	if row.Total > 0 && row.State != StepRunning {
		scoreStyle := theme.OK
		if row.Passed < row.Total {
			scoreStyle = theme.Warn
		}
		return scoreStyle.Render(strconv.Itoa(row.Passed) + "/" + strconv.Itoa(row.Total) + " passed")
	}
	switch row.State {
	case StepRunning:
		return theme.Running.Render(component.Duration(time.Since(row.Started)))
	case StepDone, StepFailed:
		return theme.Dim.Render(component.Duration(row.Ended.Sub(row.Started)))
	case StepPending:
	}
	return ""
}

// containerDetail says what a container does, and how often it restarted.
func containerDetail(ctr sandbox.ContainerStatus) string {
	detail := ctr.State
	switch {
	case ctr.Init && ctr.State == StateTerminated && ctr.ExitCode == 0:
		detail = "done"
	case ctr.Reason != "":
		detail = ctr.Reason
	}
	if ctr.State == StateTerminated && ctr.ExitCode != 0 {
		detail += " (exit " + strconv.Itoa(ctr.ExitCode) + ")"
	}
	if ctr.Restarts > 0 {
		detail += " ↻" + strconv.Itoa(ctr.Restarts)
	}
	return detail
}

// crashReasons are the reasons of a container that does not come up by
// itself.
func crashReasons() []string {
	return []string{"CrashLoopBackOff", "ErrImagePull", "ImagePullBackOff", "CreateContainerConfigError"}
}

// shortHost shortens a zone URL for the header: a zone's host starts with
// its long ID, so the header keeps its first characters and the domain.
func shortHost(url string) string {
	host := strings.TrimPrefix(url, "https://")
	id, domain, ok := strings.Cut(host, ".")
	if !ok || len(id) <= shortID {
		return host
	}
	return id[:shortID] + "…" + domain
}
