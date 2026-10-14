package sandbox_test

import (
	"bytes"
	"cmp"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"io/fs"
	"maps"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/issuer"
	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

const (
	// cluster is the cluster name the tests pass.
	cluster = "asp"
	// namedTunnel is the Cloudflare tunnel name the tests use. It differs
	// from cluster, so a test fails if the code uses one name for the other.
	namedTunnel = "asp-tunnel"
	// email is the user the tests' sandbox acts for.
	email = "ada@example.com"
	// zoneURL is the zone the tests' sandbox dials.
	zoneURL = "https://zone.example.com"
	// goodSecret is the service account secret the fake management API takes.
	goodSecret = "good"
)

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
			name: "first non-empty value for a repeated key",
			file: "KEYCARD_LOCAL_CLUSTER=\nKEYCARD_LOCAL_CLUSTER=first\nKEYCARD_LOCAL_CLUSTER=last\n",
			want: sandbox.Settings{sandbox.KeyCluster: "first"},
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
				sandbox.KeyKubeContext: "", sandbox.KeyImageTag: "", sandbox.KeyClusterCIDRs: "",
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
		noHost = "the named tunnel " + namedTunnel +
			" needs KEYCARD_LOCAL_ISSUER set to https://<hostname>, with no port or path"
	)
	named := func(issuerURL string) sandbox.Settings {
		return sandbox.Settings{sandbox.KeyTunnel: namedTunnel, sandbox.KeyIssuer: issuerURL}
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
			wantCall: "cloudflared KUBERC= tunnel --no-autoupdate run --url http://127.0.0.1:8480 " + namedTunnel + "\n",
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
// then waits. While cloudflared runs, QuickIssuerFile has the URL and is
// locked. After Tunnel returns, the file is gone unless another tunnel
// replaced it. Tunnel deletes a file left by a killed tunnel before it
// starts. The environment sets TUNNEL_NAME, and cloudflared must not see it.
func TestQuickTunnel(t *testing.T) {
	t.Parallel()
	const (
		url   = "https://quick-brown-fox.trycloudflare.com"
		other = "https://other-tunnel.trycloudflare.com\n"
	)
	tests := []struct {
		name string
		// killed is the content of an issuer file left by a killed tunnel.
		killed string
		// replace is what another tunnel writes to the issuer file while this one runs.
		replace string
		// wantFile is the issuer file after Tunnel returns, or "" for none.
		wantFile string
	}{
		{name: "deletes its URL"},
		{name: "keeps another tunnel's URL", replace: other, wantFile: other},
		{name: "replaces a killed tunnel's URL", killed: other},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{}, `env | grep '^TUNNEL_' >>"$CALLS"
echo "INF Requesting new quick Tunnel on api.trycloudflare.com..." >&2
echo "INF |  `+url+`  |" >&2
exec sleep 60
`)
			tool := local.Tool
			local.Tool = func(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
				cmd, err := tool(ctx, name, args...)
				cmd.Env = append(cmd.Env, "TUNNEL_NAME=asp")
				return cmd, err
			}
			if tc.killed != "" {
				writeFile(t, local.QuickIssuerFile(), tc.killed)
			}
			out := &bytes.Buffer{}
			local.Out = out
			ctx, stop := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- local.Tunnel(ctx, "127.0.0.1:8480") }()

			published := waitForHeld(t, local.QuickIssuerFile())
			if tc.replace != "" {
				if err := os.Remove(local.QuickIssuerFile()); err != nil {
					t.Fatal(err)
				}
				writeFile(t, local.QuickIssuerFile(), tc.replace)
			}
			stop()
			if err := <-done; err != nil {
				t.Fatalf("Tunnel() = %v, want nil once stopped", err)
			}
			if published != url+"\n" {
				t.Fatalf("issuer file = %q, want %q", published, url+"\n")
			}
			checkIssuerFile(t, local.QuickIssuerFile(), tc.wantFile)
			checkQuickCall(t, readCalls(t, calls))
			deleted := "Deleted " + local.QuickIssuerFile()
			for line, want := range map[string]bool{
				deleted + ". The quick tunnel's URL has stopped working.\n":     tc.wantFile == "",
				deleted + ". The quick tunnel that wrote it is not running.\n":  tc.killed != "",
				"INF Requesting new quick Tunnel on api.trycloudflare.com...\n": true,
			} {
				if got := strings.Contains(out.String(), line); got != want {
					t.Errorf("output has %q = %t, want %t\n%s", line, got, want, out)
				}
			}
		})
	}
}

// TestQuickTunnelLost checks that Tunnel stops a quick tunnel that Cloudflare
// no longer knows, which cloudflared would retry forever, and removes its URL.
func TestQuickTunnelLost(t *testing.T) {
	t.Parallel()
	notFound := `echo 'ERR Register tunnel error from server side error="Unauthorized: Tunnel not found"' >&2` + "\n"
	local, _ := newLocal(t, sandbox.Settings{}, `
echo "INF |  https://lost-brown-fox.trycloudflare.com  |" >&2
`+strings.Repeat(notFound, 3)+`exec sleep 60
`)
	out := &bytes.Buffer{}
	local.Out = out
	err := local.Tunnel(t.Context(), "127.0.0.1:8480")
	checkErr(
		t,
		err,
		[]error{sandbox.ErrTunnel},
		"Cloudflare deleted the quick tunnel https://lost-brown-fox.trycloudflare.com",
	)
	if _, err := os.Stat(local.QuickIssuerFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("issuer file: %v, want it removed", err)
	}
	if !strings.Contains(out.String(), "Start the tunnel again for a new URL") {
		t.Errorf("output = %q, want it to say what to do", out)
	}
}

// checkQuickCall fails the test unless calls is a single cloudflared call
// that starts a quick tunnel, with no TUNNEL_ variables. The config path
// changes on every run, so any path matches.
func checkQuickCall(t *testing.T, calls string) {
	t.Helper()
	call := strings.Fields(calls)
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
	if !slices.EqualFunc(call, want, func(got, want string) bool { return want == "*" || got == want }) {
		t.Fatalf("call = %q, want %q", call, want)
	}
}

// waitForHeld returns the content of path once a running tunnel has it
// locked. It fails the test after 10 seconds.
func waitForHeld(t *testing.T, path string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if body, held := heldFile(path); held {
			return body
		}
	}
	t.Fatalf("no tunnel held %s within 10 seconds", path)
	return ""
}

// heldFile returns the content of path, and whether another open file has an
// exclusive lock on it.
func heldFile(path string) (string, bool) {
	file, err := os.Open(path) //nolint:gosec // G304: the path is the test's own file.
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		return "", false
	}
	raw, err := io.ReadAll(file)
	return string(raw), err == nil
}

// checkIssuerFile fails the test unless path holds want, or does not exist
// when want is "".
func checkIssuerFile(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the test's own file.
	switch {
	case want == "" && !errors.Is(err, fs.ErrNotExist):
		t.Fatalf("issuer file = %q, %v, want none", raw, err)
	case want != "" && string(raw) != want:
		t.Fatalf("issuer file = %q, %v, want %q", raw, err, want)
	}
}

// trustFile writes a PEM file that trusts server's certificate, and returns
// its path.
func trustFile(t *testing.T, server *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	writeFile(t, path, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})))
	return path
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G703: the path is under the test's own directory.
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// holdIssuerFile fakes a running quick tunnel. It writes the issuer file and
// locks it until the test ends.
func holdIssuerFile(t *testing.T, path, body string) {
	t.Helper()
	writeFile(t, path, body)
	file, err := os.Open(path) //nolint:gosec // G304: the path is the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

// TestQuickTunnelRefused checks that a quick tunnel fails when cloudflared
// exits before it prints a URL, or when a running tunnel has the issuer file
// locked. A failed tunnel must not change the issuer file, because the file
// may belong to a running tunnel.
func TestQuickTunnelRefused(t *testing.T) {
	t.Parallel()
	const other = "https://other-tunnel.trycloudflare.com\n"
	tests := []struct {
		name string
		// body is the fake cloudflared. It is not run when file is set.
		body string
		// file is the content of an issuer file that a running tunnel has
		// locked when Tunnel starts.
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
			name:     "another tunnel holds the issuer file",
			file:     other,
			want:     []error{sandbox.ErrTunnel},
			wantText: "another quick tunnel is running and holds ",
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
				holdIssuerFile(t, local.QuickIssuerFile(), tc.file)
			}
			err := local.Tunnel(t.Context(), "127.0.0.1:8480")
			checkErr(t, err, tc.want, tc.wantText)
			checkIssuerFile(t, local.QuickIssuerFile(), tc.wantFile)
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
		// quick is the URL in the issuer file of a running quick tunnel.
		quick string
		// killed writes the issuer file without its lock, as a killed tunnel
		// leaves it.
		killed   bool
		want     []error
		wantText string
		wantCall string
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
			name:     "killed quick tunnel",
			settings: sandbox.Settings{sandbox.KeyCluster: cluster},
			quick:    closed + "/quick",
			killed:   true,
			want:     []error{sandbox.ErrIssuer},
			wantText: "local/out/issuer is not running. Run `go run ./cmd/sandbox tunnel`",
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
			switch {
			case tc.killed:
				writeFile(t, local.QuickIssuerFile(), tc.quick+"\n")
			case tc.quick != "":
				holdIssuerFile(t, local.QuickIssuerFile(), tc.quick+"\n")
			}
			err := local.CheckIssuer(t.Context(), tc.cluster, tc.issuerURL)
			checkErr(t, err, tc.want, tc.wantText)
			if got := readCalls(t, calls); got != tc.wantCall {
				t.Fatalf("calls = %q, want %q", got, tc.wantCall)
			}
		})
	}
}
