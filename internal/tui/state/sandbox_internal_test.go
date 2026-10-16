package state

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
)

// testSandbox is the state of a session with settings.
func testSandbox(t *testing.T, settings sandbox.Settings) *Sandbox {
	t.Helper()
	local := &sandbox.Local{Root: t.TempDir(), Settings: settings, Out: io.Discard}
	return New(local.NewSession("127.0.0.1:8480", time.Second), component.NewLogView(nil).Log)
}

// TestUp counts a deployed sandbox as up once its cluster exists and
// the zone reaches the cluster's issuer: through the tunnel on a local
// cluster, and without one on an existing cluster or at an issuer URL the
// operator publishes.
func TestUp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                            string
		kubeContext, issuer             string
		tunnel, deployed, missing, want bool
	}{
		{name: "local cluster with the tunnel", tunnel: true, deployed: true, want: true},
		{name: "local cluster without the tunnel", deployed: true},
		{name: "tunnel without the cluster", tunnel: true, deployed: true, missing: true},
		{name: "local cluster at the operator's issuer", issuer: "https://keys.example.com", deployed: true, want: true},
		{name: "existing cluster", kubeContext: "prod", deployed: true, want: true},
		{name: "existing cluster before the zone step", kubeContext: "prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			known := testSandbox(t, sandbox.Settings{
				sandbox.KeyKubeContext: tc.kubeContext, sandbox.KeyIssuer: tc.issuer,
			})
			known.Status.Tunnel.Running, known.Status.Deployed = tc.tunnel, tc.deployed
			known.Status.ClusterExists = !tc.missing
			if got := known.Up(); got != tc.want {
				t.Errorf("Up = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOneJob starts one job at a time: a second one is refused, and the
// first one says what waits for it. Ending the job returns it, and
// cancelling stops only a job that can stop.
func TestOneJob(t *testing.T) {
	t.Parallel()
	known := testSandbox(t, sandbox.Settings{})
	cancelled := false
	if !known.StartJob(Job{Kind: JobAction, Name: "Restarting ada's pod"}, func() { cancelled = true }) {
		t.Fatal("the first job was refused")
	}
	if known.StartJob(Job{Kind: JobBringUp}, nil) || !known.Job.Is(JobAction) ||
		known.Job.WaitText() != "Restarting ada's pod is running. Wait for it to finish." {
		t.Errorf("a second job: job %+v, wait %q; want it refused, and the first one's name",
			known.Job, known.Job.WaitText())
	}
	known.CancelJob()
	if ended := known.EndJob(); !cancelled || ended.Name != "Restarting ada's pod" || known.Job.Busy() {
		t.Errorf("cancelled %v, ended %+v, busy %v; want the first job cancelled, ended and gone",
			cancelled, ended, known.Job.Busy())
	}
	if text := known.Job.WaitText(); text != "" {
		t.Errorf("WaitText with no job = %q, want none", text)
	}
}

// TestPhase says how the sandbox is from the job, the steps and the last
// read, in that order: a job that runs comes first, then where a bring-up
// stopped, then what the read found.
func TestPhase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(known *Sandbox)
		want  Phase
	}{
		{"reading", func(known *Sandbox) { known.Read = false }, PhaseReading},
		{"bringing up", func(known *Sandbox) { known.Job = Job{Kind: JobBringUp} }, PhaseBringingUp},
		{"checking", func(known *Sandbox) { known.Job = Job{Kind: JobChecks} }, PhaseChecking},
		{"stopping", func(known *Sandbox) { known.Job = Job{Kind: JobStop} }, PhaseStopping},
		{"stopped", func(known *Sandbox) {
			known.Steps[0].State, known.Steps[0].Err = StepFailed, context.Canceled
		}, PhaseStopped},
		{"failed", func(known *Sandbox) {
			known.Steps[0].State, known.Steps[0].Err = StepFailed, sandbox.ErrDocker
		}, PhaseFailed},
		{"up", func(known *Sandbox) {
			known.Status = sandbox.Status{Deployed: true, ClusterExists: true}
			known.Status.Tunnel.Running = true
		}, PhaseUp},
		{"down", func(*Sandbox) {}, PhaseDown},
	} {
		known := testSandbox(t, sandbox.Settings{})
		known.Read = true
		tc.setup(known)
		if got, _ := known.Phase(); got != tc.want {
			t.Errorf("%s: phase %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPodState says how the selected agent's pod is from the last read:
// none before a bring-up, unread or missing, starting, running or failed.
func TestPodState(t *testing.T) {
	t.Parallel()
	const pod = "sandbox-ada-0"
	agentIn := func(status sandbox.ContainerStatus) []ContainerRow {
		status.Name = AgentContainer
		return []ContainerRow{{Pod: pod, Container: status}}
	}
	for _, tc := range []struct {
		name  string
		agent sandbox.Agent
		pods  []ContainerRow
		err   error
		want  PodState
	}{
		{"no pod", sandbox.Agent{}, nil, nil, PodNone},
		{"unread", sandbox.Agent{Pod: pod}, nil, sandbox.ErrPod, PodUnread},
		{"missing", sandbox.Agent{Pod: pod}, nil, nil, PodMissing},
		{"starting", sandbox.Agent{Pod: pod}, agentIn(sandbox.ContainerStatus{State: "waiting"}), nil, PodStarting},
		{"running", sandbox.Agent{Pod: pod}, agentIn(sandbox.ContainerStatus{Ready: true}), nil, PodRunning},
		{
			"crashing",
			sandbox.Agent{Pod: pod},
			agentIn(sandbox.ContainerStatus{State: "waiting", Reason: "CrashLoopBackOff"}), nil, PodAgentFailed,
		},
	} {
		known := testSandbox(t, sandbox.Settings{})
		known.Pods, known.PodsErr = tc.pods, tc.err
		if got, _ := known.PodState(tc.agent); got != tc.want {
			t.Errorf("%s: pod state %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStepEnded fails a step with its error, and counts the checks that
// passed from the checks' log.
func TestStepEnded(t *testing.T) {
	t.Parallel()
	known := testSandbox(t, sandbox.Settings{})
	last := len(known.Steps) - 1
	known.Steps[last].Log.Add("  PASS with a credential", "  FAIL to a blocked host", "  PASS without one")
	known.StepStarted(last)
	if row := known.StepEnded(last, sandbox.ErrCheck); row.State != StepFailed || row.Passed != 2 || row.Total != 3 {
		t.Errorf("checks: state %v, %d of %d passed; want failed, 2 of 3", row.State, row.Passed, row.Total)
	}
	if known.FailedStep() != nil {
		t.Error("a failed check stops the bring-up, want the sandbox counted up")
	}
}
