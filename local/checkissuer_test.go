package local_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckIssuerCAFile runs check-issuer.sh with KEYCARD_LOCAL_CA_FILE set
// in its settings file and no mise on PATH. A setting the script accepts
// reaches the tool check and stops there, so every row fails and its message
// says which check stopped it.
func TestCheckIssuerCAFile(t *testing.T) {
	t.Parallel()

	const noMise = "check-issuer.sh: mise is not installed. See README.md."
	bin := linkTools(t, "basename", "bash", "dirname")
	dir := t.TempDir()
	pem := filepath.Join(dir, "roots.pem")
	// A JSON file whose string field holds a PEM certificate, so the marker
	// is not at the start of a line.
	json := filepath.Join(dir, "roots.json")
	for path, body := range map[string]string{
		pem:  "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n",
		json: `{"root_certificate_pem": "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----\n"}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name, settings, want string
	}{
		{"unset", "", noMise},
		{"empty", "KEYCARD_LOCAL_CA_FILE=\n", noMise},
		{"PEM file", "KEYCARD_LOCAL_CA_FILE=" + pem + "\n", noMise},
		{
			"relative path",
			"KEYCARD_LOCAL_CA_FILE=roots.pem\n",
			"check-issuer.sh: KEYCARD_LOCAL_CA_FILE must be an absolute path (got 'roots.pem')",
		},
		{
			"missing file",
			"KEYCARD_LOCAL_CA_FILE=" + dir + "/missing.pem\n",
			"check-issuer.sh: KEYCARD_LOCAL_CA_FILE " + dir + "/missing.pem is not a readable file",
		},
		{
			"directory",
			"KEYCARD_LOCAL_CA_FILE=" + dir + "\n",
			"check-issuer.sh: KEYCARD_LOCAL_CA_FILE " + dir + " is not a readable file",
		},
		{
			"PEM inside JSON",
			"KEYCARD_LOCAL_CA_FILE=" + json + "\n",
			"check-issuer.sh: KEYCARD_LOCAL_CA_FILE " + json + " does not hold a PEM certificate",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := filepath.Join(t.TempDir(), "local.env")
			if err := os.WriteFile(env, []byte(tc.settings), 0o600); err != nil {
				t.Fatal(err)
			}
			//nolint:gosec // The arguments are a test table's inputs to a script in this repository.
			cmd := exec.CommandContext(t.Context(), filepath.Join(bin, "bash"), "check-issuer.sh",
				"--env", env, "--cluster", "asp", "--issuer", "https://oidc.example.com")
			cmd.Env = []string{"PATH=" + bin}

			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("check-issuer.sh succeeded with no mise on PATH\n%s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("output lacks %q\n%s", tc.want, out)
			}
		})
	}
}
