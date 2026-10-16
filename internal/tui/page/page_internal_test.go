package page

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// TestSandboxUp counts a deployed sandbox as up once its cluster exists and
// the zone reaches the cluster's issuer: through the tunnel on a local
// cluster, and without one on an existing cluster or at an issuer URL the
// operator publishes.
func TestSandboxUp(t *testing.T) {
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
			local := &sandbox.Local{
				Root: t.TempDir(), Out: io.Discard,
				Settings: sandbox.Settings{sandbox.KeyKubeContext: tc.kubeContext, sandbox.KeyIssuer: tc.issuer},
			}
			shared := NewShared(local.NewSession("127.0.0.1:8480", time.Second))
			shared.Status.Tunnel.Running, shared.Status.Deployed = tc.tunnel, tc.deployed
			shared.Status.ClusterExists = !tc.missing
			if got := shared.SandboxUp(); got != tc.want {
				t.Errorf("SandboxUp = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOneJob starts one job at a time: a second one is refused with a
// notice that names the first. Ending the job returns it, and cancelling
// stops only a job that can stop.
func TestOneJob(t *testing.T) {
	t.Parallel()
	local := &sandbox.Local{Root: t.TempDir(), Settings: sandbox.Settings{}, Out: io.Discard}
	shared := NewShared(local.NewSession("127.0.0.1:8480", time.Second))
	cancelled := false
	if !shared.StartJob(Job{Kind: JobAction, Name: "Restarting ada's pod"}, func() { cancelled = true }) {
		t.Fatal("the first job was refused")
	}
	if shared.StartJob(Job{Kind: JobBringUp}, nil) || !shared.Job.Is(JobAction) ||
		shared.Toast.Text != "Restarting ada's pod is running. Wait for it to finish." {
		t.Errorf("a second job: job %+v, notice %q; want it refused with the first one's name",
			shared.Job, shared.Toast.Text)
	}
	shared.CancelJob()
	if ended := shared.EndJob(); !cancelled || ended.Name != "Restarting ada's pod" || shared.Job.Busy() {
		t.Errorf("cancelled %v, ended %+v, busy %v; want the first job cancelled, ended and gone",
			cancelled, ended, shared.Job.Busy())
	}
	if shared.Busy() {
		t.Error("Busy with no job, want false")
	}
}

// TestPhase says how the sandbox is from the job, the steps and the last
// read, in that order: a job that runs comes first, then where a bring-up
// stopped, then what the read found.
func TestPhase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(shared *Shared)
		want  Phase
	}{
		{"reading", func(shared *Shared) { shared.Read = false }, PhaseReading},
		{"bringing up", func(shared *Shared) { shared.Job = Job{Kind: JobBringUp} }, PhaseBringingUp},
		{"stopping", func(shared *Shared) { shared.Job = Job{Kind: JobStop} }, PhaseStopping},
		{"stopped", func(shared *Shared) {
			shared.Steps[0].State, shared.Steps[0].Err = StepFailed, context.Canceled
		}, PhaseStopped},
		{"failed", func(shared *Shared) {
			shared.Steps[0].State, shared.Steps[0].Err = StepFailed, sandbox.ErrDocker
		}, PhaseFailed},
		{"up", func(shared *Shared) {
			shared.Status = sandbox.Status{Deployed: true, ClusterExists: true}
			shared.Status.Tunnel.Running = true
		}, PhaseUp},
		{"down", func(*Shared) {}, PhaseDown},
	} {
		local := &sandbox.Local{Root: t.TempDir(), Settings: sandbox.Settings{}, Out: io.Discard}
		shared := NewShared(local.NewSession("127.0.0.1:8480", time.Second))
		shared.Read = true
		tc.setup(shared)
		if got, _ := shared.Phase(); got != tc.want {
			t.Errorf("%s: phase %v, want %v", tc.name, got, tc.want)
		}
	}
}
