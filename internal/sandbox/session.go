package sandbox

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

// StepID names one bring-up step of a Session.
type StepID string

// The bring-up steps, in the order they run.
const (
	StepTunnel  StepID = "tunnel"
	StepCluster StepID = "cluster"
	StepMirror  StepID = "mirror"
	StepIssuer  StepID = "issuer"
	StepImages  StepID = "images"
	StepZone    StepID = "zone"
	StepInstall StepID = "install"
	StepSandbox StepID = "sandbox"
	StepPod     StepID = "pod"
	StepChecks  StepID = "checks"
)

// execVerb is kubectl's command that runs a command in a container.
const execVerb = "exec"

// ErrStep is a step a Session does not have.
var ErrStep = errors.New("unknown step")

// Step is one bring-up step and what it does, in a few words.
type Step struct {
	ID    StepID
	Title string
}

// Session runs the bring-up steps one at a time, for a caller that shows
// each step's progress, such as the TUI. Each step writes its output to the
// writer it gets, and keeps what the next steps need. The tunnel and the
// mirror run as services, which outlive the Session.
type Session struct {
	// listen is the mirror's address, which the tunnel forwards to.
	listen string
	// IssuerWait bounds the retries of an issuer that does not answer yet.
	IssuerWait time.Duration
	// ReplaceActive lets zone-setup replace the zone's active policy set.
	ReplaceActive bool

	// mu guards local, output, agent, built and changes. Reload and the
	// steps write them, and the TUI reads them from other goroutines. Reload
	// replaces local instead of changing it.
	mu     sync.Mutex
	local  *Local
	output *zonesetup.Output
	// agent is the email of the agent Target names, or "" for the one
	// KEYCARD_LOCAL_USER_EMAIL names.
	agent string
	// built is what Status found set up. Status keeps it until a step, an
	// action or Reload clears it. changes goes up each time one does, so
	// Status drops what it read while something changed.
	built   built
	changes int
}

// built is what the bring-up set up and only the Session's steps and
// actions take down.
type built struct {
	cluster, deployed bool
}

// NewSession returns a Session for the settings.
func (l *Local) NewSession(listen string, issuerWait time.Duration) *Session {
	return &Session{local: l, listen: listen, IssuerWait: issuerWait}
}

// Local returns the checkout and the settings the Session runs with. Reload
// replaces them with a new Local.
func (s *Session) Local() *Local {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.local
}

// Reload takes the settings from the store and the environment again,
// after setup changed them, and forgets what the steps learned from the old
// settings. A goroutine that holds the old Local keeps reading the old
// settings.
func (s *Session) Reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	local := *s.local
	local.UseConfig()
	s.local, s.output = &local, nil
	s.built = built{}
	s.changes++
}

// Steps returns the steps for the settings. An existing cluster publishes
// its own issuer, so it has no tunnel, cluster, mirror or images step. A
// tunnel runs unless KEYCARD_LOCAL_ISSUER names an issuer the operator
// publishes without one.
func (s *Session) Steps() []Step {
	local := s.Local()
	deploy := []Step{
		{StepZone, "Set up the Keycard zone"},
		{StepInstall, "Install the charts"},
		{StepSandbox, "Create each agent's sandbox"},
		{StepPod, "Wait for each agent's pod"},
		{StepChecks, "Run the checks in each agent"},
	}
	if local.Existing() {
		return slices.Concat([]Step{{StepIssuer, "Check the cluster's issuer"}}, deploy)
	}
	var steps []Step
	if local.StartsTunnel() {
		steps = append(steps, Step{StepTunnel, "Start the tunnel"})
	}
	return slices.Concat(steps, []Step{
		{StepCluster, "Create or check the cluster"},
		{StepMirror, "Start the issuer mirror"},
		{StepIssuer, "Verify the issuer with Keycard"},
		{StepImages, "Import the images"},
	}, deploy)
}

// Run runs one step with its output on out.
func (s *Session) Run(ctx context.Context, stepID StepID, out io.Writer) error {
	local := s.Local().WithIO(out, nil)
	run, ok := map[StepID]func(context.Context, *Local) error{
		StepTunnel: s.runTunnel, StepCluster: s.runCluster, StepMirror: s.runMirror,
		StepIssuer: s.runIssuer, StepImages: s.runImages, StepZone: s.runZone,
		StepInstall: s.runInstall, StepSandbox: s.runSandbox, StepPod: s.runPod, StepChecks: s.runChecks,
	}[stepID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrStep, stepID)
	}
	defer s.changed()
	return run(ctx, local)
}

// RecreateCluster deletes the k3d cluster and creates it again with the
// current issuer. Run it when StepCluster fails with ErrOtherIssuer or
// ErrBrokenCluster.
func (s *Session) RecreateCluster(ctx context.Context, out io.Writer) error {
	defer s.changed()
	local := s.Local().WithIO(out, nil)
	if err := local.Down(ctx, "", true); err != nil {
		return err
	}
	return local.Bootstrap(ctx, "", "", false)
}

// Target returns the sandbox of the selected agent, as the steps created
// it, or as an earlier run did, from local/out/zone-setup.yaml.
func (s *Session) Target() (CheckTarget, error) {
	email := s.Selected()
	output, err := s.zoneOutput()
	if err != nil {
		return CheckTarget{}, err
	}
	access, err := userAccess(output, email)
	if err != nil {
		return CheckTarget{}, err
	}
	local := s.Local()
	cluster, err := local.cluster("")
	if err != nil {
		return CheckTarget{}, err
	}
	return local.agentTarget(cluster, output, access), nil
}

// Agent is one person's agent: their sandbox and its pod.
type Agent struct {
	// Email is the person the agent acts as.
	Email string
	// Pod is the agent's pod, named for the person's user ID, or "" before
	// the zone step has run for them.
	Pod string
	// Default is set for the person setup picked, who has an agent in
	// every demo.
	Default bool
}

// Agents lists the agent of each person the settings give one in the
// demo, the person setup picked first.
func (s *Session) Agents() []Agent {
	local := s.Local()
	users := local.previousValues().values.Users
	output, outputErr := s.zoneOutput()
	agents := make([]Agent, 0, len(users))
	for _, user := range users {
		agent := Agent{Email: user.Email, Default: user.Email == local.Settings[KeyUserEmail]}
		if outputErr == nil {
			if access, err := userAccess(output, user.Email); err == nil {
				agent.Pod = access.UserID
			}
		}
		agents = append(agents, agent)
	}
	return agents
}

// SelectAgent makes Target, and so the logs, the shell and a restart, act
// on the agent of email.
func (s *Session) SelectAgent(email string) {
	s.mu.Lock()
	s.agent = email
	s.mu.Unlock()
}

// Selected returns the email of the agent Target names.
func (s *Session) Selected() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cmp.Or(s.agent, s.local.Settings[KeyUserEmail])
}

// Scenario returns the scenario the settings name.
func (s *Session) Scenario() (*Scenario, error) {
	return s.Local().scenario()
}

// ContainerStatus is the state of one container of a pod.
type ContainerStatus struct {
	Name string
	// Init is set for an init container, which runs before the others.
	Init     bool
	Ready    bool
	Restarts int
	// State is waiting, running or terminated, and Reason says why, such
	// as CrashLoopBackOff or Completed.
	State, Reason string
	ExitCode      int
}

// PodStatus is the state of one pod in the sandbox's namespace.
type PodStatus struct {
	Name       string
	Containers []ContainerStatus
}

// Pods lists the pods in the sandbox's namespace: every agent's.
func (s *Session) Pods(ctx context.Context) ([]PodStatus, error) {
	local := s.Local()
	cluster, output, err := s.deployed(local)
	if err != nil {
		return nil, err
	}
	raw, err := local.kubectl(
		ctx,
		cluster,
		nil,
		namespaceFlag,
		output.K8sSandbox.Namespace,
		"get",
		"pods",
		"-o",
		"json",
	)
	if err != nil {
		return nil, err
	}
	return parsePods(raw)
}

// podList is the part of `kubectl get pods -o json` Pods reads.
type podList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
			ContainerStatuses     []containerStatus `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type containerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	State        map[string]struct {
		Reason   string `json:"reason"`
		ExitCode int    `json:"exitCode"`
	} `json:"state"`
}

func parsePods(raw []byte) ([]PodStatus, error) {
	var list podList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("%w: reading the pod list: %w", ErrPod, err)
	}
	pods := make([]PodStatus, 0, len(list.Items))
	for _, item := range list.Items {
		pod := PodStatus{Name: item.Metadata.Name}
		for _, status := range item.Status.InitContainerStatuses {
			pod.Containers = append(pod.Containers, status.convert(true))
		}
		for _, status := range item.Status.ContainerStatuses {
			pod.Containers = append(pod.Containers, status.convert(false))
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

func (c containerStatus) convert(init bool) ContainerStatus {
	out := ContainerStatus{Name: c.Name, Init: init, Ready: c.Ready, Restarts: c.RestartCount}
	for _, state := range []string{"terminated", "waiting", "running"} {
		if detail, ok := c.State[state]; ok {
			out.State, out.Reason, out.ExitCode = state, detail.Reason, detail.ExitCode
			break
		}
	}
	return out
}

// LogsCommand returns a kubectl command that prints the last tail lines of
// a container's log in the sandbox's namespace, and follows it when follow
// is set. A container that finished has nothing more to follow.
func (s *Session) LogsCommand(ctx context.Context, pod, container string, tail int, follow bool) (*exec.Cmd, error) {
	local := s.Local()
	cluster, output, err := s.deployed(local)
	if err != nil {
		return nil, err
	}
	args := []string{
		namespaceFlag, output.K8sSandbox.Namespace, "logs", "--tail=" + strconv.Itoa(tail), pod, "-c", container,
	}
	if follow {
		args = append(args, "--follow")
	}
	return local.kubectlCommand(ctx, cluster, args...)
}

// ShellCommand returns a kubectl command that runs a command in the agent
// container, attached to a terminal: the scenario's run.command, or sh when
// the scenario has none or plain is set.
func (s *Session) ShellCommand(ctx context.Context, plain bool) (*exec.Cmd, error) {
	target, err := s.Target()
	if err != nil {
		return nil, err
	}
	local := s.Local()
	scenario, err := local.scenario()
	if err != nil {
		return nil, err
	}
	command := scenario.Run.Command
	if len(command) == 0 || plain {
		command = []string{"sh"}
	}
	return local.kubectlCommand(ctx, target.Cluster, slices.Concat([]string{
		namespaceFlag, target.Namespace, execVerb, "-it", target.Pod, "-c", agentContainer, "--",
	}, command)...)
}

// RestartAgent deletes the sandbox's pod. The controller creates it again
// from the sandbox's template, with the images and settings of now.
func (s *Session) RestartAgent(ctx context.Context, out io.Writer) error {
	target, err := s.Target()
	if err != nil {
		return err
	}
	local := s.Local().WithIO(out, nil)
	if _, err = local.kubectl(ctx, target.Cluster, nil, namespaceFlag, target.Namespace,
		"delete", "pod", target.Pod, "--wait=true"); err != nil {
		return fmt.Errorf("%w: %w", ErrPod, err)
	}
	_, _ = fmt.Fprintf(out, "Deleted the pod %s. The controller creates it again.\n", target.Pod)
	return local.waitForPod(ctx, target.Cluster, target.Namespace, target.Pod)
}

// Down stops the tunnel and the mirror and deletes the k3d cluster. What
// zone-setup created in the zone stays.
func (s *Session) Down(ctx context.Context, out io.Writer) error {
	defer s.changed()
	local := s.Local().WithIO(out, nil)
	if err := local.StopServices(ctx); err != nil {
		return err
	}
	if local.Existing() {
		return nil
	}
	return local.Down(ctx, "", true)
}

// placeholderIssuer stands in for the cluster's issuer while Problems
// checks the values file, before the tunnel has a URL.
const placeholderIssuer = "https://issuer.invalid"

// Problems returns what would stop the bring-up with the current settings,
// before any step runs: setup that was never done, a values file zone-setup
// would refuse, a secret that is not set, and a demo that cannot load.
func (s *Session) Problems() []error {
	local := s.Local()
	if !local.Initialized() {
		return []error{fmt.Errorf("%w: setup has not run yet", ErrSettings)}
	}
	var problems []error
	var users []zonesetup.UserValues
	values, err := local.zoneSetupValues(placeholderIssuer, "the check before the bring-up")
	if err != nil {
		problems = append(problems, err)
	} else {
		users = values.Users
		local.useAccountEnv(&values.ManagementServiceAccount)
		if _, err = local.loadSecrets(values); err != nil {
			problems = append(problems, err)
		}
	}
	problems = append(problems, local.agentProblems(users)...)
	if local.Settings[KeyZoneURL] == "" || local.Settings[KeyUserEmail] == "" {
		problems = append(problems, fmt.Errorf("%w: %w", ErrSettings, errNoZoneOrUser))
	}
	return problems
}

// errNoZoneOrUser is settings without a zone or a user.
var errNoZoneOrUser = errors.New("the zone or the user is missing")

// ProblemSection names the setup section that fixes a problem Problems
// returned, or "" when setup should ask every section. A values file
// problem about one field goes to the section that asks for that field.
func ProblemSection(err error) string {
	var field *zonesetup.FieldError
	switch {
	case errors.Is(err, zonesetup.ErrAuth):
		return SectionAccount
	case errors.As(err, &field):
		return fieldSection(field.Field)
	case errors.Is(err, zonesetup.ErrMissingSecret), errors.Is(err, ErrScenario):
		return SectionScenario
	case errors.Is(err, errNoZoneOrUser):
		return SectionZone
	}
	return ""
}

// fieldSection names the setup section that asks for a field of the
// values file, such as resources[2].identifier, or "" for none.
func fieldSection(field string) string {
	if end := strings.IndexAny(field, ".["); end >= 0 {
		field = field[:end]
	}
	switch field {
	case "management_api_url", "management_service_account":
		return SectionAccount
	case "zone_url", "zone_id", "users":
		return SectionZone
	case "resources":
		return SectionScenario
	}
	return ""
}

// Status is what is running now, without changing anything.
type Status struct {
	Tunnel, Mirror ServiceState
	// ClusterExists reports whether the k3d cluster exists. It is always
	// true for an existing cluster.
	ClusterExists bool
	// ClusterErr is why Status could not tell whether the k3d cluster
	// exists, such as Docker not running. ClusterExists is false then.
	ClusterErr error
	// Deployed reports whether an earlier run wrote local/out/zone-setup.yaml.
	Deployed bool
}

// Status reads what runs now. It does not start or change anything. Once
// the cluster exists, or zone-setup.yaml, Status does not check it again
// until a step or an action of the Session runs. A cluster deleted from
// outside the Session still shows as existing until then.
func (s *Session) Status(ctx context.Context) Status {
	s.mu.Lock()
	local, found, changes := s.local, s.built, s.changes
	s.mu.Unlock()
	status := Status{
		Tunnel: local.ServiceStatus(ServiceTunnel),
		Mirror: local.ServiceStatus(ServiceMirror),
	}
	if !found.cluster {
		found.cluster, status.ClusterErr = clusterBuilt(ctx, local)
	}
	if !found.deployed {
		_, err := local.readZoneSetup()
		found.deployed = err == nil
	}
	s.mu.Lock()
	if s.changes == changes {
		s.built = found
	}
	s.mu.Unlock()
	status.ClusterExists, status.Deployed = found.cluster, found.deployed
	return status
}

// clusterBuilt reports whether the cluster exists. An existing cluster
// always does. Without a cluster name in the settings, there is no cluster.
func clusterBuilt(ctx context.Context, local *Local) (bool, error) {
	switch {
	case local.Existing():
		return true, nil
	case local.Settings[KeyCluster] == "":
		return false, nil
	}
	cluster, err := local.cluster("")
	if err != nil {
		return false, err
	}
	return local.clusterExists(ctx, cluster)
}

// Describe says in one line what each setting section holds, for a
// settings view: the account, the zone and user, the demo, and the
// cluster.
func (s *Session) Describe() map[string]string {
	local := s.Local()
	w := &wizard{l: local, prev: local.previousValues()}
	return map[string]string{
		SectionAccount:  w.accountSummary(),
		SectionZone:     w.zoneSummary(),
		SectionScenario: w.scenarioSummary(),
		SectionCluster:  w.clusterSummary(),
	}
}

// DeployAgent brings up the agent of email once the sandbox is up, after
// AddAgent gave them one. It runs zone-setup, so that the zone knows them,
// creates their sandbox, waits for its pod and runs its checks. The cluster,
// the tunnel and the other agents stay as they are.
func (s *Session) DeployAgent(ctx context.Context, email string, out io.Writer) error {
	defer s.changed()
	local := s.Local().WithIO(out, nil)
	if err := s.runZone(ctx, local); err != nil {
		return err
	}
	cfg, err := local.runSettings(ctx)
	if err != nil {
		return err
	}
	output, err := s.zoneOutput()
	if err != nil {
		return err
	}
	access, err := userAccess(output, email)
	if err != nil {
		return err
	}
	if err = local.writeLocalValues(ctx, cfg); err != nil {
		return err
	}
	if err = local.createSandbox(ctx, cfg, output, access); err != nil {
		return err
	}
	if err = local.waitForAgent(ctx, cfg.cluster, output, access); err != nil {
		return err
	}
	return local.checkAgents(ctx, cfg.cluster, output, []zonesetup.UserAccess{access})
}

// RemoveAgent takes away the agent of email in the demo the settings name.
// When the zone step ran for them and the cluster exists, it deletes their
// sandbox and its pod, removes them from the demo's settings, and runs
// zone-setup, which archives their policies. Otherwise it only removes them
// from the settings, and the next zone step archives their policies. After
// zone-setup fails, RemoveAgent can run again for them, because
// local/out/zone-setup.yaml still lists them.
func (s *Session) RemoveAgent(ctx context.Context, email string, out io.Writer) error {
	defer s.changed()
	local := s.Local().WithIO(out, nil)
	removal, err := s.removal(ctx, local, email)
	if err != nil {
		return err
	}
	if removal.running {
		if err = local.deleteSandbox(ctx, removal.cluster, removal.namespace, removal.userID); err != nil {
			return err
		}
	}
	if removal.inSettings {
		if err = s.removeUser(ctx, local, email); err != nil {
			return err
		}
	}
	if !removal.running {
		_, _ = fmt.Fprintf(out, "Removed %s's agent from the settings. Nothing of it is running.\n", email)
		return nil
	}
	local = s.Local().WithIO(out, nil)
	return s.runZone(ctx, local)
}

// agentRemoval is what RemoveAgent changes for one person.
type agentRemoval struct {
	// inSettings is set when the settings give the person an agent.
	inSettings bool
	// running is set when the zone step ran for them and the cluster
	// exists, so that their sandbox may run there in namespace, labeled
	// with userID.
	running                    bool
	cluster, namespace, userID string
}

// changed makes Status check the cluster and zone-setup.yaml again. A step
// or an action that can change either calls it as it returns.
func (s *Session) changed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.built = built{}
	s.changes++
}

// removal finds what RemoveAgent changes for email. It refuses when they
// have no agent, in the demo's settings or in the zone step's output, and
// when they are the person setup picked. A zone step's output it cannot
// read, or a cluster it cannot list, is an error.
func (s *Session) removal(ctx context.Context, local *Local, email string) (agentRemoval, error) {
	var removal agentRemoval
	agents := local.previousValues().agents
	if email == agents.Default {
		return removal, errDefaultAgent(email)
	}
	removal.inSettings = slices.Contains(agents.people(local.demo()), email)
	output, err := s.zoneOutput()
	if errors.Is(err, fs.ErrNotExist) {
		// The zone step never ran.
		output, err = &zonesetup.Output{}, nil
	}
	if err != nil {
		return removal, err
	}
	i := slices.IndexFunc(output.SandboxUserAccess, func(access zonesetup.UserAccess) bool {
		return access.UserEmail == email
	})
	switch {
	case i < 0 && !removal.inSettings:
		return removal, fmt.Errorf("%w: %s has no agent in this demo", ErrSettings, email)
	case i < 0:
		// The zone step never ran for them.
		return removal, nil
	}
	removal.namespace, removal.userID = output.K8sSandbox.Namespace, output.SandboxUserAccess[i].UserID
	if removal.cluster, err = local.cluster(""); err != nil {
		return removal, err
	}
	removal.running = local.Existing()
	if !removal.running {
		removal.running, err = local.clusterExists(ctx, removal.cluster)
	}
	return removal, err
}

// removeUser takes email out of the settings, and selects the agent the
// settings name when theirs was selected.
func (s *Session) removeUser(ctx context.Context, local *Local, email string) error {
	if err := local.removeUser(ctx, email); err != nil {
		return err
	}
	s.Reload()
	if s.Selected() == email {
		s.SelectAgent("")
	}
	return nil
}

// runTunnel keeps a running tunnel when it started with the tunnel the
// settings name, or starts one and waits for a quick tunnel's URL. A
// running quick tunnel keeps its URL, so the cluster made for it keeps
// working.
func (s *Session) runTunnel(ctx context.Context, local *Local) error {
	tunnel := local.Settings[KeyTunnel]
	if running := s.runningTunnel(local); running != "" {
		_, _ = fmt.Fprintln(local.Out, running)
		return nil
	}
	if err := s.prepareTunnel(ctx, local); err != nil {
		return err
	}
	if err := local.spawnService(ServiceTunnel, s.listen); err != nil {
		return err
	}
	if err := os.WriteFile(local.serviceFile(ServiceTunnel, tunnelSuffix), []byte(tunnel), fileMode); err != nil {
		return fmt.Errorf("recording the tunnel's name: %w", err)
	}
	_, _ = fmt.Fprintf(local.Out, "Started the tunnel. Its log is %s.\n", local.serviceFile(ServiceTunnel, ".log"))
	if tunnel != "" {
		return nil
	}
	if err := s.waitService(ctx, local, ServiceTunnel, "the quick tunnel's URL", local.quickTunnelRuns); err != nil {
		return err
	}
	url, err := local.issuer("")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(local.Out, "The tunnel is at %s.\n", url)
	return nil
}

// prepareTunnel checks the tunnel settings and stops the tunnel the Session
// started. For a quick tunnel, it refuses while another quick tunnel holds
// QuickIssuerFile, and deletes a file no tunnel holds, so that the wait for
// the URL sees only the new tunnel's.
func (s *Session) prepareTunnel(ctx context.Context, local *Local) error {
	if err := local.checkTunnel(s.listen); err != nil {
		return err
	}
	if err := local.StopService(ctx, ServiceTunnel); err != nil && !errors.Is(err, ErrNotRunning) {
		return err
	}
	if local.Settings[KeyTunnel] != "" {
		return nil
	}
	return checkNoQuickURL(local.QuickIssuerFile(), local.Out)
}

// tunnelSuffix names the file next to the tunnel's pid file that holds the
// tunnel name it started with, empty for a quick tunnel.
const tunnelSuffix = ".name"

// runningTunnel returns a line about the running tunnel when it started
// with the tunnel the settings name. It returns "" when the step must start
// one: no tunnel runs, another tunnel runs, or a quick tunnel has no URL.
func (s *Session) runningTunnel(local *Local) string {
	tunnel := local.Settings[KeyTunnel]
	started, err := os.ReadFile(local.serviceFile(ServiceTunnel, tunnelSuffix))
	if err != nil || string(started) != tunnel || !local.ServiceStatus(ServiceTunnel).Running {
		return ""
	}
	if tunnel != "" {
		return "The tunnel " + tunnel + " is running."
	}
	if url, err := local.issuer(""); err == nil {
		return "The tunnel is running at " + url + "."
	}
	return ""
}

func (s *Session) runCluster(ctx context.Context, local *Local) error {
	return local.Bootstrap(ctx, "", "", false)
}

// runMirror starts the mirror again, because it reads the cluster's keys
// once and the cluster may be new. It refuses when something else answers
// on the mirror's address, such as a mirror started in another terminal,
// which would serve the keys of an older cluster.
func (s *Session) runMirror(ctx context.Context, local *Local) error {
	if err := local.StopService(ctx, ServiceMirror); err != nil && !errors.Is(err, ErrNotRunning) {
		return err
	}
	if answers(ctx, s.listen) {
		return fmt.Errorf("%w: something else answers on %s, where the mirror listens. "+
			"Stop it, such as a sandbox mirror in another terminal, and run this step again", ErrService, s.listen)
	}
	if err := local.spawnService(ServiceMirror, s.listen); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(local.Out, "Started the mirror on %s. Its log is %s.\n",
		s.listen, local.serviceFile(ServiceMirror, ".log"))
	return s.waitService(ctx, local, ServiceMirror, "the mirror on "+s.listen, mirrorServes(ctx, s.listen))
}

// answers reports whether something accepts a TCP connection on address.
func answers(ctx context.Context, address string) bool {
	conn, err := (&net.Dialer{Timeout: pollEvery}).DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// runIssuer checks the issuer until it passes or IssuerWait runs out. It
// stops early when the tunnel or the mirror the steps start is not running,
// because the issuer cannot answer without them.
func (s *Session) runIssuer(ctx context.Context, local *Local) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return local.checkIssuerUntil(ctx, s.IssuerWait, watchServices(ctx, local, s.stepServices()...))
}

// stepServices returns the services the steps start.
func (s *Session) stepServices() []string {
	services := map[StepID]string{StepTunnel: ServiceTunnel, StepMirror: ServiceMirror}
	var names []string
	for _, step := range s.Steps() {
		if name, ok := services[step.ID]; ok {
			names = append(names, name)
		}
	}
	return names
}

// watchServices sends an error once one of the named services is not
// running. It stops watching when ctx ends.
func watchServices(ctx context.Context, local *Local, names ...string) <-chan error {
	stopped := make(chan error, 1)
	go func() {
		for {
			for _, name := range names {
				if !local.ServiceStatus(name).Running {
					stopped <- fmt.Errorf("%w: the %s is not running. Its log is %s",
						ErrService, name, local.serviceFile(name, ".log"))
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(pollEvery):
			}
		}
	}()
	return stopped
}

func (s *Session) runImages(ctx context.Context, local *Local) error {
	cfg, err := local.runSettings(ctx)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(local.outDir(), dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", local.outDir(), err)
	}
	return local.importImages(ctx, cfg.cluster, cfg.imageTag, cfg.scenario)
}

func (s *Session) runZone(ctx context.Context, local *Local) error {
	cfg, err := local.runSettings(ctx)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(local.outDir(), dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", local.outDir(), err)
	}
	output, err := local.zoneSetup(ctx, cfg, RunOptions{ReplaceActive: s.ReplaceActive})
	if err != nil {
		return err
	}
	if _, err = userAccess(output, cfg.email); err != nil {
		return err
	}
	s.mu.Lock()
	s.output = output
	s.mu.Unlock()
	return nil
}

func (s *Session) runInstall(ctx context.Context, local *Local) error {
	cfg, err := local.runSettings(ctx)
	if err != nil {
		return err
	}
	output, err := s.needZone(local)
	if err != nil {
		return err
	}
	if err = local.writeLocalValues(ctx, cfg); err != nil {
		return err
	}
	return local.install(ctx, cfg.cluster, output.K8sSandbox.Namespace)
}

func (s *Session) runSandbox(ctx context.Context, local *Local) error {
	cfg, err := local.runSettings(ctx)
	if err != nil {
		return err
	}
	output, err := s.needZone(local)
	if err != nil {
		return err
	}
	return local.createSandboxes(ctx, cfg, output)
}

func (s *Session) runPod(ctx context.Context, local *Local) error {
	cluster, err := local.cluster("")
	if err != nil {
		return err
	}
	output, err := s.needZone(local)
	if err != nil {
		return err
	}
	for _, access := range output.SandboxUserAccess {
		if err = local.waitForAgent(ctx, cluster, output, access); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) runChecks(ctx context.Context, local *Local) error {
	cluster, err := local.cluster("")
	if err != nil {
		return err
	}
	output, err := s.needZone(local)
	if err != nil {
		return err
	}
	return local.checkAgents(ctx, cluster, output, output.SandboxUserAccess)
}

// deployed returns the cluster and the zone step's output.
func (s *Session) deployed(local *Local) (string, *zonesetup.Output, error) {
	cluster, err := local.cluster("")
	if err != nil {
		return "", nil, err
	}
	output, err := s.zoneOutput()
	return cluster, output, err
}

// zoneOutput returns the zone step's output, or the one an earlier run
// wrote.
func (s *Session) zoneOutput() (*zonesetup.Output, error) {
	s.mu.Lock()
	output := s.output
	s.mu.Unlock()
	if output != nil {
		return output, nil
	}
	return s.Local().readZoneSetup()
}

// needZone returns the zone step's output. When the bring-up starts again
// after the zone step, it reads local/out/zone-setup.yaml, which that step
// wrote. It refuses an output that lists someone without an agent in the
// demo, so that a step does not bring back an agent RemoveAgent took away
// before zone-setup archived their policies.
func (s *Session) needZone(local *Local) (*zonesetup.Output, error) {
	output, err := s.zoneOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: run the zone step first: %w", ErrZoneSetup, err)
	}
	prev := local.previousValues()
	if !prev.exists {
		return output, nil
	}
	people := prev.agents.people(local.demo())
	for _, access := range output.SandboxUserAccess {
		if !slices.Contains(people, access.UserEmail) {
			return nil, fmt.Errorf("%w: %s has no agent in this demo. Run the zone step again",
				ErrZoneSetup, access.UserEmail)
		}
	}
	return output, nil
}

// waitService waits for ready as Up does, and stops waiting when the
// service is not running.
func (s *Session) waitService(ctx context.Context, local *Local, name, what string, ready func() bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return waitFor(ctx, watchServices(ctx, local, name), what, ready)
}
