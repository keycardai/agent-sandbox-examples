package local_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTunnelSettings runs tunnel.sh with no mise on PATH. A refused setting
// stops the run with its message. Settings that pass reach the tool check and
// stop there, before cloudflared runs or local/out/issuer is written.
func TestTunnelSettings(t *testing.T) {
	t.Parallel()

	const (
		toolCheck = "tunnel.sh: mise is not installed. See README.md."
		quickSet  = "KEYCARD_LOCAL_ISSUER is set, but a quick tunnel picks its own URL."
		noHost    = "the named tunnel asp needs KEYCARD_LOCAL_ISSUER set to https://<hostname>, with no port or path"
		badName   = "KEYCARD_LOCAL_TUNNEL must be a tunnel name of letters, digits, dots, dashes and underscores"
	)
	bin := linkTools(t, "basename", "bash", "dirname")

	tests := []struct {
		name, file, want string
	}{
		{"quick tunnel", "", toolCheck},
		{"quick tunnel with an issuer", "KEYCARD_LOCAL_ISSUER=https://oidc.example.com\n", quickSet},
		{"named tunnel", "KEYCARD_LOCAL_TUNNEL=asp\nKEYCARD_LOCAL_ISSUER=https://oidc.example.com\n", toolCheck},
		{"named tunnel without an issuer", "KEYCARD_LOCAL_TUNNEL=asp\n", noHost},
		{"named tunnel with a path", "KEYCARD_LOCAL_TUNNEL=asp\nKEYCARD_LOCAL_ISSUER=https://oidc.example.com/a\n", noHost},
		{
			"named tunnel with a port",
			"KEYCARD_LOCAL_TUNNEL=asp\nKEYCARD_LOCAL_ISSUER=https://oidc.example.com:8443\n",
			noHost,
		},
		{"named tunnel over http", "KEYCARD_LOCAL_TUNNEL=asp\nKEYCARD_LOCAL_ISSUER=http://oidc.example.com\n", noHost},
		{"tunnel name with a space", "KEYCARD_LOCAL_TUNNEL=a sp\nKEYCARD_LOCAL_ISSUER=https://oidc.example.com\n", badName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := filepath.Join(t.TempDir(), "local.env")
			if err := os.WriteFile(env, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			//nolint:gosec // The arguments are a test table's inputs to a script in this repository.
			cmd := exec.CommandContext(t.Context(), filepath.Join(bin, "bash"), "tunnel.sh", "--env", env)
			cmd.Env = []string{"PATH=" + bin}

			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("tunnel.sh succeeded with no mise on PATH\n%s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("output lacks %q\n%s", tc.want, out)
			}
		})
	}
}
