// Package state is what the TUI knows about the sandbox, and the rules that
// read and change it. The pages read it to draw, and it draws nothing.
package state

import (
	"cmp"
	"slices"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
)

// Other log sources, besides StepSource, ServiceSource and ContainerSource.
const (
	SourceSetup    = "setup"
	SourceAction   = "action"
	SourceActivity = "activity"
	// SourceTraffic is the feed of requests, with the lookups of hosts the
	// proxy blocked, and SourceTrafficAll the feed with every DNS lookup.
	SourceTraffic    = "traffic"
	SourceTrafficAll = "traffic dns"
	// SourceResponse holds the body of the Agents tab's last response.
	SourceResponse = "response"
)

// ServiceSource names a service's log.
func ServiceSource(name string) string { return "service " + name }

// ContainerSource names the log of a pod's container.
func ContainerSource(pod, container string) string {
	return "container " + pod + "/" + container
}

// Sandbox is the sandbox as the TUI knows it, in parts. A part's methods
// change only that part. The rules that read more than one part, the
// session or the clock are Sandbox's. Update runs on one goroutine, so it
// needs no lock.
type Sandbox struct {
	session *sandbox.Session
	logs    func(source string) *component.Log
	// Now is the clock the steps' times come from.
	Now func() time.Time

	Run    Run
	Reads  Reads
	Agents Agents
	Setup  Setup
	Demos  Demos
}

// Run is the job that runs, and the bring-up's steps.
type Run struct {
	// Steps are the bring-up's steps, and Cursor the one it is on.
	Steps  []*StepRow
	Cursor int
	// Job is the work that runs, or the zero Job.
	Job Job
}

// Reads is the sandbox as the last read found it.
type Reads struct {
	// Done is set once the first read came back.
	Status sandbox.Status
	Done   bool
	// mark moves on each time a job ends, so a stale read can be told apart.
	mark int
}

// Agents are the agents the settings give, and their pods and traffic.
type Agents struct {
	// Pods has every pod's containers, and PodsErr the last read's error.
	List    []sandbox.Agent
	Pods    []ContainerRow
	PodsErr error
	// Traffic holds the proxy's decisions, from its audit log.
	Traffic proxylog.Store
}

// Setup is what setting the sandbox up needs to know.
type Setup struct {
	// Describe says what each settings section holds, and Problems what
	// would stop the bring-up. KeychainErr is why the keychain cannot keep
	// secrets, and Err why the last setup failed.
	Describe    map[string]string
	Problems    []error
	KeychainErr error
	Err         error
	// Preflight is the last run of the machine checks.
	Preflight Preflight
}

// Demos are the checkout's demos, and the one the sandbox runs.
type Demos struct {
	// Loaded is the demo the dashboard shows, and Err why a demo did not
	// load. Launched names the demo the last bring-up deployed.
	Loaded   *sandbox.Scenario
	List     []*sandbox.Scenario
	Err      error
	Launched string
}

// New returns the sandbox of session. logs returns the log of a source.
func New(session *sandbox.Session, logs func(source string) *component.Log) *Sandbox {
	state := &Sandbox{session: session, logs: logs, Now: time.Now}
	state.ResetSteps()
	state.Demos.Launched = state.deployedDemo()
	return state
}

// Setting returns the setting of key, or "".
func (s *Sandbox) Setting(key string) string { return s.local().Settings[key] }

// StartsTunnel reports whether the bring-up starts a quick tunnel.
func (s *Sandbox) StartsTunnel() bool { return s.local().StartsTunnel() }

// Existing reports whether the settings name the operator's own cluster.
func (s *Sandbox) Existing() bool { return s.local().Existing() }

// Root is the checkout the sandbox runs from.
func (s *Sandbox) Root() string { return s.local().Root }

// Saving is whether the settings are saved or can be, and where.
type Saving struct {
	Saved, CanSave bool
	File, Keychain string
}

// Saving says whether the settings are saved, and where.
func (s *Sandbox) Saving() Saving {
	store := s.local().Store
	return Saving{Saved: store.Saved(), CanSave: store.CanSave(), File: store.Path(), Keychain: store.KeychainWhere()}
}

// SetSettings keeps what each settings section holds, and the problems.
func (s *Setup) SetSettings(describe map[string]string, problems []error) {
	s.Describe, s.Problems = describe, problems
}

// Started forgets why the last setup failed.
func (s *Setup) Started() { s.Err = nil }

// Failed keeps why the setup failed, until the next one starts.
func (s *Setup) Failed(err error) { s.Err = err }

// RefreshSettings reads the settings, their problems, and whether the
// keychain works. The store asks the keychain once and keeps the answer.
func (s *Sandbox) RefreshSettings() {
	s.Setup.KeychainErr = s.local().Store.KeychainErr()
	s.Setup.Describe = s.session.Describe()
	s.Setup.Problems = s.session.Problems()
}

// FirstRun reports whether the settings were never written.
func (s *Sandbox) FirstRun() bool { return !s.local().Initialized() }

// Record adds an event to the activity log: the time, a mark and what
// happened. ✓ is done, ✕ failed, ▸ started and • a note.
func (s *Sandbox) Record(mark, text string) {
	s.logs(SourceActivity).Add(s.Now().Format(time.TimeOnly) + "  " + mark + " " + text)
}

// Up reports whether the sandbox runs: the zone is set up, the cluster
// exists, and the tunnel runs when the bring-up starts one.
func (s *Sandbox) Up() bool {
	status := s.Reads.Status
	return status.Deployed && status.ClusterExists && (status.Tunnel.Running || !s.StartsTunnel())
}

// Mark returns the mark a read that starts now takes.
func (r *Reads) Mark() int { return r.mark }

// Current reports whether no job ended since the read with mark started.
func (r *Reads) Current(mark int) bool { return mark == r.mark }

// StoppedAll forgets the bring-up and the reads once everything stopped.
func (s *Sandbox) StoppedAll() {
	s.ResetSteps()
	s.Reads.Status, s.Reads.Done = sandbox.Status{}, false
	s.Agents.Pods, s.Agents.PodsErr = nil, nil
}

// Set keeps what a read of the services and the cluster found.
func (r *Reads) Set(status sandbox.Status) {
	r.Status, r.Done = status, true
}

// SetPods keeps a read of the pods, and keeps the selection in the list.
func (s *Sandbox) SetPods(pods []ContainerRow, agents []sandbox.Agent, err error) {
	s.Agents.Pods, s.Agents.List, s.Agents.PodsErr = pods, agents, err
	if _, index := s.Selected(); index < 0 && len(agents) > 0 {
		s.session.SelectAgent(agents[0].Email)
	}
}

// SetAgents keeps the agents the settings now give, as after adding one.
func (s *Sandbox) SetAgents(agents []sandbox.Agent) { s.Agents.List = agents }

// Selected returns the selected agent and its index, or -1 when unlisted.
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

// RunningDemoName names the demo the dashboard shows: the one the sandbox
// runs while it is up, else the one the settings name.
func (s *Sandbox) RunningDemoName() string {
	if s.Up() && s.Demos.Launched != "" {
		return s.Demos.Launched
	}
	return s.DemoName()
}

// Demo returns the demo RunningDemoName names, or why it does not load.
func (s *Sandbox) Demo() (*sandbox.Scenario, error) {
	name := s.RunningDemoName()
	if s.Demos.Loaded != nil && s.Demos.Loaded.Name == name {
		return s.Demos.Loaded, nil
	}
	load := s.session.Scenario
	if name != s.DemoName() {
		load = s.session.RunningScenario
	}
	scenario, err := load()
	if err != nil {
		return nil, err
	}
	s.Demos.Loaded = scenario
	return scenario, nil
}

// LoadedDemo returns the demo Demo returns, or nil when it does not load.
func (s *Sandbox) LoadedDemo() *sandbox.Scenario {
	scenario, _ := s.Demo()
	return scenario
}

// DemoList returns the checkout's demos that load, by name, read once.
func (s *Sandbox) DemoList() []*sandbox.Scenario {
	if s.Demos.List == nil && s.Demos.Err == nil {
		s.Demos.List, s.Demos.Err = sandbox.Scenarios(s.local().Root)
	}
	return s.Demos.List
}

// local is the session's settings and machine.
func (s *Sandbox) local() *sandbox.Local { return s.session.Local() }

func (s *Sandbox) deployedDemo() string { return cmp.Or(s.local().DeployedDemo(), s.DemoName()) }
