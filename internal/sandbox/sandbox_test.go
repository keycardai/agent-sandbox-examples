package sandbox_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/issuer"
	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// cluster is the cluster name the tests pass.
const cluster = "asp"

// record is the start of every fake tool. It appends the tool's name, the
// KUBERC it runs with and its arguments to $CALLS, one call per line.
const record = `#!/usr/bin/env bash
printf '%s KUBERC=%s %s\n' "$FAKE_TOOL" "${KUBERC:-}" "$*" >>"$CALLS"
`

// newLocal returns a Local rooted in a new directory, in which every tool
// runs record and then body. It also returns the path of the calls file.
func newLocal(t *testing.T, settings sandbox.Settings, body string) (*sandbox.Local, string) {
	t.Helper()
	dir := t.TempDir()
	script, calls := filepath.Join(dir, "tool"), filepath.Join(dir, "calls")
	//nolint:gosec // G306: the fake tool must be executable.
	if err := os.WriteFile(script, []byte(record+body), 0o700); err != nil {
		t.Fatal(err)
	}
	tool := func(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, script, args...) //nolint:gosec // G204: the script is the test's fake tool.
		cmd.Env = append(os.Environ(), "FAKE_TOOL="+name, "CALLS="+calls)
		return cmd, nil
	}
	return &sandbox.Local{Root: dir, Settings: settings, Tool: tool, Out: &bytes.Buffer{}}, calls
}

// readCalls returns the calls file, or "" when no tool ran.
func readCalls(t *testing.T, calls string) string {
	t.Helper()
	raw, err := os.ReadFile(calls) //nolint:gosec // G304: the path is the test's own file.
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func checkErr(t *testing.T, err error, want []error, wantText string) {
	t.Helper()
	if len(want) == 0 {
		if err != nil {
			t.Fatalf("err = %v, want none", err)
		}
		return
	}
	for _, target := range want {
		if !errors.Is(err, target) {
			t.Fatalf("err = %v, want %v", err, target)
		}
	}
	if !strings.Contains(err.Error(), wantText) {
		t.Fatalf("err = %v, want it to contain %q", err, wantText)
	}
}

func TestLoadSettings(t *testing.T) {
	t.Parallel()
	example, err := os.ReadFile("../../local/local.env.example")
	if err != nil {
		t.Fatal(err)
	}
	const refused = "line 1 is not KEY=value for a key local.env.example lists"

	tests := []struct {
		name string
		// file is the settings file's content. noFile writes none.
		file   string
		noFile bool
		// named passes the file's path. Otherwise it is local/local.env.
		named    bool
		env      map[string]string
		want     sandbox.Settings
		wantText string
	}{
		{
			name: "comments and blank lines",
			file: "# comment\n\n   # indented\nKEYCARD_LOCAL_CLUSTER=asp\n",
			want: sandbox.Settings{sandbox.KeyCluster: cluster},
		},
		{
			name: "value taken literally",
			file: "KEYCARD_LOCAL_CLUSTER=\"quoted\" a=b",
			want: sandbox.Settings{sandbox.KeyCluster: `"quoted" a=b`},
		},
		{name: "unknown key", file: "cluster=other\n", wantText: refused},
		{name: "command", file: "touch MARKER\n", wantText: refused},
		{name: "key after a space", file: " KEYCARD_LOCAL_CLUSTER=asp\n", wantText: refused},
		{
			name: "environment beats file",
			file: "KEYCARD_LOCAL_CLUSTER=from-file\n",
			env:  map[string]string{sandbox.KeyCluster: "from-env", "OTHER": "x"},
			want: sandbox.Settings{sandbox.KeyCluster: "from-env"},
		},
		{
			name: "empty environment value",
			file: "KEYCARD_LOCAL_CLUSTER=from-file\n",
			env:  map[string]string{sandbox.KeyCluster: ""},
			want: sandbox.Settings{sandbox.KeyCluster: "from-file"},
		},
		{
			name:   "missing default file",
			noFile: true,
			env:    map[string]string{sandbox.KeyIssuer: "https://oidc.example.com"},
			want:   sandbox.Settings{sandbox.KeyIssuer: "https://oidc.example.com"},
		},
		{name: "missing named file", noFile: true, named: true, wantText: "no such file"},
		{
			name: "example file",
			file: string(example),
			want: sandbox.Settings{
				sandbox.KeyCluster: "keycard-sandbox-local", sandbox.KeyIssuer: "", sandbox.KeyTunnel: "",
				sandbox.KeyZoneURL: "", sandbox.KeyZoneOnHost: "", sandbox.KeyZoneSetupValues: "zone-setup.yaml",
				sandbox.KeyUserEmail: "", sandbox.KeyGitHubLogin: "", sandbox.KeyCAFile: "",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			path, named := filepath.Join(root, "local", "local.env"), ""
			if tc.named {
				path = filepath.Join(root, "settings.env")
				named = path
			}
			if !tc.noFile {
				writeFile(t, path, tc.file)
			}
			got, err := sandbox.LoadSettings(root, named, func(key string) string { return tc.env[key] })
			if tc.wantText != "" {
				checkErr(t, err, []error{sandbox.ErrSettings}, tc.wantText)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got, tc.want) {
				t.Fatalf("settings = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTunnel runs a named tunnel, and checks that each bad setting fails
// before cloudflared starts.
func TestTunnel(t *testing.T) {
	t.Parallel()
	const (
		host   = "https://oidc.example.com"
		noHost = "the named tunnel asp needs KEYCARD_LOCAL_ISSUER set to https://<hostname>, with no port or path"
	)
	named := func(issuerURL string) sandbox.Settings {
		return sandbox.Settings{sandbox.KeyTunnel: "asp", sandbox.KeyIssuer: issuerURL}
	}

	tests := []struct {
		name     string
		settings sandbox.Settings
		listen   string
		want     []error
		wantText string
		wantCall string
	}{
		{
			name:     "named tunnel",
			settings: named(host),
			wantCall: "cloudflared KUBERC= tunnel --no-autoupdate run --url http://127.0.0.1:8480 asp\n",
		},
		{
			name:     "quick tunnel with an issuer",
			settings: sandbox.Settings{sandbox.KeyIssuer: host},
			want:     []error{sandbox.ErrIssuer},
			wantText: "KEYCARD_LOCAL_ISSUER is set, but a quick tunnel picks its own URL",
		},
		{name: "no issuer", settings: named(""), want: []error{sandbox.ErrIssuer}, wantText: noHost},
		{name: "issuer with a path", settings: named(host + "/a"), want: []error{sandbox.ErrIssuer}, wantText: noHost},
		{name: "issuer with a port", settings: named(host + ":8443"), want: []error{sandbox.ErrIssuer}, wantText: noHost},
		{
			name:     "http issuer",
			settings: named("http://oidc.example.com"),
			want:     []error{sandbox.ErrIssuer},
			wantText: noHost,
		},
		{
			name:     "tunnel name with a space",
			settings: sandbox.Settings{sandbox.KeyTunnel: "a sp", sandbox.KeyIssuer: host},
			want:     []error{sandbox.ErrTunnel},
			wantText: "KEYCARD_LOCAL_TUNNEL must be a tunnel name of letters, digits, dots, dashes and underscores",
		},
		{
			name:     "listen without a port",
			settings: named(host),
			listen:   "127.0.0.1",
			want:     []error{sandbox.ErrTunnel},
			wantText: "--listen must be HOST:PORT",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, tc.settings, "")
			err := local.Tunnel(t.Context(), cmp.Or(tc.listen, "127.0.0.1:8480"))
			checkErr(t, err, tc.want, tc.wantText)
			if got := readCalls(t, calls); got != tc.wantCall {
				t.Fatalf("calls = %q, want %q", got, tc.wantCall)
			}
		})
	}
}

// TestQuickTunnel runs a fake cloudflared that prints a quick tunnel URL and
// then waits. The URL is in QuickIssuerFile while cloudflared runs, and gone
// once Tunnel returns.
func TestQuickTunnel(t *testing.T) {
	t.Parallel()
	const url = "https://quick-brown-fox.trycloudflare.com"
	local, calls := newLocal(t, sandbox.Settings{}, `
echo "INF Requesting new quick Tunnel on api.trycloudflare.com..." >&2
echo "INF |  `+url+`  |" >&2
exec sleep 60
`)
	out := &bytes.Buffer{}
	local.Out = out
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- local.Tunnel(ctx, "127.0.0.1:8480") }()

	published := waitForFile(t, local.QuickIssuerFile())
	stop()
	if err := <-done; err != nil {
		t.Fatalf("Tunnel() = %v, want nil once stopped", err)
	}
	if published != url+"\n" {
		t.Fatalf("issuer file = %q, want %q", published, url+"\n")
	}
	if _, err := os.Stat(local.QuickIssuerFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("issuer file after Tunnel returned: %v, want it removed", err)
	}
	// The config path is a new temporary file on every run.
	call := strings.Fields(readCalls(t, calls))
	want := []string{
		"cloudflared",
		"KUBERC=",
		"tunnel",
		"--no-autoupdate",
		"--config",
		"*",
		"--url",
		"http://127.0.0.1:8480",
	}
	if len(call) != len(want) ||
		!slices.EqualFunc(call, want, func(got, want string) bool { return want == "*" || got == want }) {
		t.Fatalf("call = %q, want %q", call, want)
	}
	if want := "Deleted " + local.QuickIssuerFile() + ". The quick tunnel's URL has stopped working.\n"; !strings.Contains(
		out.String(),
		want,
	) {
		t.Errorf("output lacks %q\n%s", want, out)
	}
	if !strings.Contains(out.String(), "api.trycloudflare.com") {
		t.Errorf("output = %q, want cloudflared's log lines", out)
	}
}

// waitForFile returns the content of path once it exists, and fails the test
// after 10 seconds.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: the path is the test's own file.
			return string(raw)
		}
	}
	t.Fatalf("%s did not appear within 10 seconds", path)
	return ""
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestQuickTunnelRefused checks that a quick tunnel fails when cloudflared
// exits before it prints a URL, or when another tunnel's issuer file exists.
// A failed tunnel must leave the issuer file as it found it, so it does not
// delete a running tunnel's URL.
func TestQuickTunnelRefused(t *testing.T) {
	t.Parallel()
	const other = "https://other-tunnel.trycloudflare.com\n"
	tests := []struct {
		name string
		// body is the fake cloudflared. It is not run when file is set.
		body string
		// file is the issuer file that exists before Tunnel starts.
		file     string
		want     []error
		wantText string
		// wantFile is the issuer file after Tunnel returns, or "" for none.
		wantFile string
	}{
		{
			name:     "cloudflared fails",
			body:     "echo 'ERR failed to request quick Tunnel' >&2\nexit 1\n",
			want:     []error{sandbox.ErrTunnel},
			wantText: "cloudflared exited before it printed a trycloudflare.com URL: exit status 1",
		},
		{
			name:     "cloudflared exits 0",
			body:     "exit 0\n",
			want:     []error{sandbox.ErrTunnel},
			wantText: "cloudflared exited with status 0 before it printed a trycloudflare.com URL",
		},
		{
			name:     "issuer file exists",
			file:     other,
			want:     []error{sandbox.ErrTunnel},
			wantText: "local/out/issuer exists. Another quick tunnel is running, or one was killed",
			wantFile: other,
		},
		{
			name: "another tunnel writes the issuer file first",
			body: `printf '%s' '` + other + `' >"$(dirname "$0")/local/out/issuer"
echo "INF |  https://quick-brown-fox.trycloudflare.com  |" >&2
exec sleep 60
`,
			want:     []error{sandbox.ErrTunnel, fs.ErrExist},
			wantText: "writing ",
			wantFile: other,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{}, tc.body)
			if tc.file != "" {
				writeFile(t, local.QuickIssuerFile(), tc.file)
			}
			err := local.Tunnel(t.Context(), "127.0.0.1:8480")
			checkErr(t, err, tc.want, tc.wantText)
			raw, err := os.ReadFile(local.QuickIssuerFile())
			switch {
			case tc.wantFile == "" && !errors.Is(err, fs.ErrNotExist):
				t.Fatalf("issuer file = %q, %v, want none", raw, err)
			case tc.wantFile != "" && string(raw) != tc.wantFile:
				t.Fatalf("issuer file = %q, %v, want %q", raw, err, tc.wantFile)
			}
			if ran := readCalls(t, calls) != ""; ran == (tc.file != "") {
				t.Fatalf("cloudflared ran = %t, want %t", ran, tc.file == "")
			}
		})
	}
}

// TestCheckIssuer checks that a bad cluster name, issuer or CA file fails
// before kubectl runs. Valid settings mint a token and then fail to fetch
// from 127.0.0.1:1, where nothing listens. The error shows which issuer URL
// the check used.
func TestCheckIssuer(t *testing.T) {
	t.Parallel()
	const (
		mintCall = "kubectl KUBERC=off --context k3d-asp --namespace default create token default" +
			" --audience oidc-check --duration 10m\n"
		closed  = "https://127.0.0.1:1"
		badName = "the name must be at most 32 lowercase letters, digits and dashes (got "
	)
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
	fetch := []error{sandbox.ErrIssuer, issuer.ErrFetch}
	withCA := func(file string) sandbox.Settings {
		return sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyCAFile: file}
	}

	tests := []struct {
		name               string
		settings           sandbox.Settings
		cluster, issuerURL string
		quick              string
		want               []error
		wantText           string
		wantCall           string
	}{
		{
			name:     "no cluster",
			want:     []error{sandbox.ErrCluster},
			wantText: "no cluster name. Set KEYCARD_LOCAL_CLUSTER or pass --cluster",
		},
		{
			name:     "option as cluster",
			cluster:  "--verbose",
			want:     []error{sandbox.ErrCluster},
			wantText: badName + `"--verbose")`,
		},
		{
			name:     "quoted cluster",
			settings: sandbox.Settings{sandbox.KeyCluster: `"quoted"`},
			want:     []error{sandbox.ErrCluster},
			wantText: badName + `"\"quoted\"")`,
		},
		{
			name:     "cluster over 32 bytes",
			cluster:  strings.Repeat("a", 33),
			want:     []error{sandbox.ErrCluster},
			wantText: badName,
		},
		{
			name:     "no issuer",
			cluster:  "asp",
			want:     []error{sandbox.ErrIssuer},
			wantText: "no issuer. Set KEYCARD_LOCAL_ISSUER, pass --issuer, or run `go run ./cmd/sandbox tunnel`",
		},
		{
			name:      "flags beat settings",
			settings:  sandbox.Settings{sandbox.KeyCluster: "other", sandbox.KeyIssuer: closed + "/setting"},
			cluster:   cluster,
			issuerURL: closed + "/flag",
			quick:     closed + "/quick",
			want:      fetch,
			wantText:  closed + "/flag/.well-known/openid-configuration",
			wantCall:  mintCall,
		},
		{
			name:     "setting beats quick tunnel",
			settings: sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyIssuer: closed + "/setting"},
			quick:    closed + "/quick",
			want:     fetch,
			wantText: closed + "/setting/.well-known/openid-configuration",
			wantCall: mintCall,
		},
		{
			name:     "quick tunnel",
			settings: sandbox.Settings{sandbox.KeyCluster: cluster},
			quick:    closed + "/quick",
			want:     fetch,
			wantText: closed + "/quick/.well-known/openid-configuration",
			wantCall: mintCall,
		},
		{
			name:      "relative CA file",
			settings:  withCA("roots.pem"),
			issuerURL: closed,
			want:      []error{sandbox.ErrCAFile},
			wantText:  `KEYCARD_LOCAL_CA_FILE must be an absolute path (got "roots.pem")`,
		},
		{
			name:      "missing CA file",
			settings:  withCA(dir + "/missing.pem"),
			issuerURL: closed,
			want:      []error{sandbox.ErrCAFile},
			wantText:  "KEYCARD_LOCAL_CA_FILE " + dir + "/missing.pem is not a readable file",
		},
		{
			name:      "CA directory",
			settings:  withCA(dir),
			issuerURL: closed,
			want:      []error{sandbox.ErrCAFile},
			wantText:  "KEYCARD_LOCAL_CA_FILE " + dir + " is not a readable file",
		},
		{
			name:      "PEM inside JSON",
			settings:  withCA(json),
			issuerURL: closed,
			want:      []error{sandbox.ErrCAFile},
			wantText:  "KEYCARD_LOCAL_CA_FILE " + json + " does not hold a PEM certificate",
		},
		{
			// The file passes the settings check but is not a valid
			// certificate. The error from internal/issuer shows that the
			// file reached the verifier.
			name:      "PEM file",
			settings:  withCA(pem),
			issuerURL: closed,
			want:      []error{sandbox.ErrIssuer, issuer.ErrCA},
			wantText:  "does not hold a PEM certificate",
			wantCall:  mintCall,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, tc.settings, "echo fake-token\n")
			if tc.quick != "" {
				writeFile(t, local.QuickIssuerFile(), tc.quick+"\n")
			}
			err := local.CheckIssuer(t.Context(), tc.cluster, tc.issuerURL)
			checkErr(t, err, tc.want, tc.wantText)
			if got := readCalls(t, calls); got != tc.wantCall {
				t.Fatalf("calls = %q, want %q", got, tc.wantCall)
			}
		})
	}
}
