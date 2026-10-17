// Package state is what the TUI knows about the sandbox, and the rules that
// read and change it: the bring-up's steps, the job that runs, the sandbox
// as the last read found it, and the agents and their pods. The TUI's root
// model changes it through its methods, and the pages read it to draw. It
// draws nothing.
package state

import (
	"cmp"
	"slices"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
)

// SourceActivity names the activity log.
const SourceActivity = "activity"

// Sandbox is the sandbox as the TUI knows it, in parts. A part's methods
// change only that part. The rules that read more than one part, the
// session or the clock are Sandbox's. Update runs on one goroutine, so it
// needs no lock.
type Sandbox struct {
	session *sandbox.Session
	logs    func(source string) *component.Log
	// Now is the clock the steps' times come from.
	Now func() time.Time

	// Run is the work that changes the sandbox, Reads what the last read of
	// it found, and Agents the agents and their pods.
	Run    Run
	Reads  Reads
	Agents Agents

	// Loaded is the demo the settings name, once it loaded.
	Loaded *sandbox.Scenario
}

// Run is the work that changes the sandbox: the job that runs, and the
// bring-up's steps.
type Run struct {
	// Steps are the bring-up's steps, and Cursor the one it is on.
	Steps  []*StepRow
	Cursor int
	// Job is the work that runs, or the zero Job.
	Job Job
}

// Reads is the sandbox as the last read found it.
type Reads struct {
	// Status is what the last read found. Done is set once the first read
	// came back.
	Status sandbox.Status
	Done   bool
	// mark moves on each time a job ends. A read takes it when it starts,
	// so that a read from before the job can be told apart.
	mark int
}

// Agents are the agents the settings give, their pods, and what the egress
// proxy decided for them.
type Agents struct {
	// List has an agent for each person the settings give one, and Pods the
	// containers of every pod. PodsErr is the last read's error.
	List    []sandbox.Agent
	Pods    []ContainerRow
	PodsErr error
	// Traffic holds the proxy's decisions, from its audit log.
	Traffic proxylog.Store
}

// New returns the sandbox of session, with the steps of its settings. logs
// returns the log of a source, such as a step's.
func New(session *sandbox.Session, logs func(source string) *component.Log) *Sandbox {
	state := &Sandbox{session: session, logs: logs, Now: time.Now}
	state.ResetSteps()
	return state
}

// Local is the session's settings and machine. The pages read the
// settings through Setting and the facts here, and the root model does the
// work that changes the sandbox.
func (s *Sandbox) Local() *sandbox.Local { return s.session.Local() }

// Setting returns the setting of key, or "".
func (s *Sandbox) Setting(key string) string { return s.Local().Settings[key] }

// StartsTunnel reports whether the bring-up starts a quick tunnel.
func (s *Sandbox) StartsTunnel() bool { return s.Local().StartsTunnel() }

// Record adds an event to the activity log: the time, a mark and what
// happened. ✓ is done, ✕ failed, ▸ started and • a note.
func (s *Sandbox) Record(mark, text string) {
	s.logs(SourceActivity).Add(s.Now().Format(time.TimeOnly) + "  " + mark + " " + text)
}

// Up reports whether the sandbox runs: the zone was set up, the cluster
// exists, and the zone reaches the cluster's issuer. When the bring-up
// starts a tunnel, that needs the tunnel. An existing cluster, or an issuer
// the operator publishes, needs none.
func (s *Sandbox) Up() bool {
	status := s.Reads.Status
	return status.Deployed && status.ClusterExists && (status.Tunnel.Running || !s.StartsTunnel())
}

// Mark returns the mark a read that starts now takes.
func (r *Reads) Mark() int { return r.mark }

// Current reports whether a read that started with mark found the
// sandbox as it is: no job ended since it started. The root model drops a
// read that is not, and reads again.
func (r *Reads) Current(mark int) bool { return mark == r.mark }

// StoppedAll forgets the bring-up and what the reads found, once
// everything stopped. The sandbox they describe is gone, so the screens say
// they read it until a read made after the stop comes back.
func (s *Sandbox) StoppedAll() {
	s.ResetSteps()
	s.Reads.Status, s.Reads.Done = sandbox.Status{}, false
	s.Agents.Pods, s.Agents.PodsErr = nil, nil
}

// Set keeps what a read of the services and the cluster found.
func (r *Reads) Set(status sandbox.Status) {
	r.Status, r.Done = status, true
}

// SetPods keeps what a read of the pods found, and keeps the selection on
// an agent in the list.
func (s *Sandbox) SetPods(pods []ContainerRow, agents []sandbox.Agent, err error) {
	s.Agents.Pods, s.Agents.List, s.Agents.PodsErr = pods, agents, err
	if _, index := s.Selected(); index < 0 && len(agents) > 0 {
		s.session.SelectAgent(agents[0].Email)
	}
}

// Selected returns the agent the session has selected, and its index in
// the list, or -1 when the list does not have it.
func (s *Sandbox) Selected() (sandbox.Agent, int) {
	email := s.session.Selected()
	for i, agent := range s.Agents.List {
		if agent.Email == email {
			return agent, i
		}
	}
	return sandbox.Agent{Email: email}, -1
}

// SelectedPods returns the containers of the selected agent's pod.
func (s *Sandbox) SelectedPods() []ContainerRow {
	agent, _ := s.Selected()
	if agent.Pod == "" {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(s.Agents.Pods), func(row ContainerRow) bool { return row.Pod != agent.Pod })
}

// Container finds the selected agent's container of name.
func (s *Sandbox) Container(name string) (ContainerRow, bool) {
	pods := s.SelectedPods()
	index := slices.IndexFunc(pods, func(row ContainerRow) bool { return row.Container.Name == name })
	if index < 0 {
		return ContainerRow{}, false
	}
	return pods[index], true
}

// DemoName names the demo the settings name.
func (s *Sandbox) DemoName() string {
	return cmp.Or(s.Setting(sandbox.KeyScenario), sandbox.DefaultScenario)
}

// Demo returns the demo the settings name, or why it does not load. It
// keeps the demo it loaded, and reads it again for another name, or while
// it does not load.
func (s *Sandbox) Demo() (*sandbox.Scenario, error) {
	if s.Loaded != nil && s.Loaded.Name == s.DemoName() {
		return s.Loaded, nil
	}
	scenario, err := s.session.Scenario()
	if err != nil {
		return nil, err //nolint:wrapcheck // The session names the scenario file that does not load.
	}
	s.Loaded = scenario
	return scenario, nil
}

// LoadedDemo returns the demo the settings name, or nil when it does not
// load.
func (s *Sandbox) LoadedDemo() *sandbox.Scenario {
	scenario, _ := s.Demo()
	return scenario
}
