package page

import (
	"io"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// TestSandboxUp counts a deployed sandbox as up once the zone reaches the
// cluster's issuer: through the tunnel on a local cluster, and without one
// on an existing cluster.
func TestSandboxUp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                   string
		kubeContext            string
		tunnel, deployed, want bool
	}{
		{name: "local cluster with the tunnel", tunnel: true, deployed: true, want: true},
		{name: "local cluster without the tunnel", deployed: true},
		{name: "existing cluster", kubeContext: "prod", deployed: true, want: true},
		{name: "existing cluster before the zone step", kubeContext: "prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local := &sandbox.Local{
				Root: t.TempDir(), Settings: sandbox.Settings{sandbox.KeyKubeContext: tc.kubeContext}, Out: io.Discard,
			}
			shared := NewShared(local.NewSession("127.0.0.1:8480", time.Second))
			shared.Status.Tunnel.Running, shared.Status.Deployed = tc.tunnel, tc.deployed
			if got := shared.SandboxUp(); got != tc.want {
				t.Errorf("SandboxUp = %v, want %v", got, tc.want)
			}
		})
	}
}
