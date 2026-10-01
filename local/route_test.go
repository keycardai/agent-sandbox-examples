package local_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoute(t *testing.T) {
	t.Parallel()

	bin := linkTools(t, "basename", "bash", "cat", "dirname")
	env := filepath.Join(t.TempDir(), "local.env")
	if err := os.WriteFile(env, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, issuer string
		wantErr      bool
		want         []string
	}{
		{"valid", "https://sandbox-oidc.localdev.keycard.sh", false, []string{
			"hostnames:\n    - sandbox-oidc.localdev.keycard.sh\n",
			`addresses: ["192.0.2.10"]`,
			"port: 8480\n",
		}},
		{"explicit port", "https://sandbox-oidc.localdev.keycard.sh:443/oidc", true, []string{
			"the issuer names a port, but the route matches the bare hostname",
		}},
		{"second hostname", "https://sandbox-oidc.localdev.keycard.sh\n    - \"*.localdev.keycard.sh\"", true, []string{
			"the issuer must name a host of lowercase ASCII DNS labels",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			//nolint:gosec // The arguments are a test table's inputs to a script in this repository.
			cmd := exec.CommandContext(t.Context(), filepath.Join(bin, "bash"), "route.sh", "--env", env,
				"--issuer", tc.issuer, "--host-address", "192.0.2.10", "--listen", "127.0.0.1:8480")
			cmd.Env = []string{"PATH=" + bin}

			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("route.sh err = %v, want failure %t\n%s", err, tc.wantErr, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(out), want) {
					t.Errorf("output lacks %q\n%s", want, out)
				}
			}
		})
	}
}
