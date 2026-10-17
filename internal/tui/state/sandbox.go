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

// The logs the state keeps, besides the steps'.
const (
	SourceSetup    = "setup"
	SourceActivity = "activity"
)

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

	// Describe says what each section of the settings holds, and Problems
	// what would stop the bring-up. KeychainErr is why the keychain cannot
	// keep secrets, or nil. SetupErr is why the last setup failed, until
	// the next one starts.
	Describe    map[string]string
	Problems    []error
	KeychainErr error
	SetupErr    error
	// Preflight is the last run of the machine checks.
	Preflight Preflight

	// Loaded is the demo the dashboard shows, once it loaded. Demos is every
	// demo in the checkout that loads, read once, and DemosErr says why any
	// did not load. Launched names the demo the sandbox runs, as the last
	// bring-up deployed it, or the one the settings name when none says.
	Loaded   *sandbox.Scenario
	Demos    []*sandbox.Scenario
	DemosErr error
	Launched string
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

// New returns the sandbox of session. logs returns the log of a source.
func New(session *sandbox.Session, logs func(source string) *component.Log) *Sandbox {
	state := &Sandbox{session: session, logs: logs, Now: time.Now}
	state.ResetSteps()
	state.Launched = state.deployedDemo()
	return state
}

// Setting returns the setting of key, or "".
func (s *Sandbox) Setting(key string) string { return s.local().Settings[key] }

// StartsTunnel reports whether the bring-up starts a quick tunnel.
func (s *Sandbox) StartsTunnel() bool { return s.local().StartsTunnel() }

// Existing reports whether the settings name a cluster of the operator's,
// which the sandbox does not create.
func (s *Sandbox) Existing() bool { return s.local().Existing() }

// Root is the checkout the sandbox runs from.
func (s *Sandbox) Root() string { return s.local().Root }

// Saving is whether the settings are saved, and whether they can be: the
// keychain must keep the secrets. File and Keychain are where they are
// saved.
type Saving struct {
	Saved, CanSave bool
	File, Keychain string
}

// Saving says whether the settings are saved, and where.
func (s *Sandbox) Saving() Saving {
	store := s.local().Store
	return Saving{Saved: store.Saved(), CanSave: store.CanSave(), File: store.Path(), Keychain: store.KeychainWhere()}
}

// SetSettings keeps what each section of the settings holds and their
// problems, as read after work that changed them.
func (s *Sandbox) SetSettings(describe map[string]string, problems []error) {
	s.Describe, s.Problems = describe, problems
}

// SetupStarted forgets why the last setup failed.
func (s *Sandbox) SetupStarted() { s.SetupErr = nil }

// SetupFailed keeps why the setup failed, until the next one starts.
func (s *Sandbox) SetupFailed(err error) { s.SetupErr = err }

// RefreshSettings reads the settings, their problems, and whether the
// keychain works. The store asks the keychain once and keeps the answer.
// The TUI reads the settings before it draws, so that on Linux a dialog to
// unlock the keyring shows before the TUI, and the screens read the answer
// without waiting on it.
func (s *Sandbox) RefreshSettings() {
	s.KeychainErr = s.local().Store.KeychainErr()
	s.Describe = s.session.Describe()
	s.Problems = s.session.Problems()
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
// runs while it is up, else the one the settings name, which u brings up.
func (s *Sandbox) RunningDemoName() string {
	if s.Up() && s.Launched != "" {
		return s.Launched
	}
	return s.DemoName()
}

// Demo returns the demo the dashboard shows, the one RunningDemoName names,
// or why it does not load. It keeps the demo it loaded, and reads it again
// for another name, or while it does not load.
func (s *Sandbox) Demo() (*sandbox.Scenario, error) {
	name := s.RunningDemoName()
	if s.Loaded != nil && s.Loaded.Name == name {
		return s.Loaded, nil
	}
	load := s.session.Scenario
	if name != s.DemoName() {
		load = s.session.RunningScenario
	}
	scenario, err := load()
	if err != nil {
		return nil, err
	}
	s.Loaded = scenario
	return scenario, nil
}

// LoadedDemo returns the demo the dashboard shows, or nil when it does not
// load.
func (s *Sandbox) LoadedDemo() *sandbox.Scenario {
	scenario, _ := s.Demo()
	return scenario
}

// DemoList returns the checkout's demos that load, read once, sorted by
// name.
func (s *Sandbox) DemoList() []*sandbox.Scenario {
	if s.Demos == nil && s.DemosErr == nil {
		s.Demos, s.DemosErr = sandbox.Scenarios(s.local().Root)
	}
	return s.Demos
}

// local is the session's settings and machine. The pages read the
// settings through Setting and the facts here, and the root model does the
// work that changes the sandbox.
func (s *Sandbox) local() *sandbox.Local { return s.session.Local() }

// deployedDemo names the demo the last bring-up deployed, or the one the
// settings name when no bring-up says.
func (s *Sandbox) deployedDemo() string { return cmp.Or(s.local().DeployedDemo(), s.DemoName()) }
