package sandbox_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

const (
	leftHeader = "\nUp stopped. To finish by hand, run these commands from the repository root:\n"
	listen     = "127.0.0.1:8480"
)

// TestUpPrint checks that --print prints the commands and does not run any
// tool.
func TestUpPrint(t *testing.T) {
	t.Parallel()
	const (
		cmd    = "  go run ./cmd/sandbox "
		tunnel = cmd + "tunnel --listen 127.0.0.1:8480    # in a terminal of its own\n"
		rest   = cmd + "bootstrap\n" +
			cmd + "mirror --listen 127.0.0.1:8480    # in another terminal\n" +
			cmd + "check-issuer\n" +
			cmd + "run\n"
	)
	tests := []struct {
		name     string
		settings sandbox.Settings
		// running writes QuickIssuerFile to fake a running quick tunnel.
		running bool
		opts    sandbox.UpOptions
		want    string
	}{
		{name: "quick tunnel", want: tunnel + rest},
		{
			name:     "named tunnel",
			settings: sandbox.Settings{sandbox.KeyTunnel: namedTunnel, sandbox.KeyIssuer: testIssuer},
			want:     tunnel + rest,
		},
		{name: "quick tunnel already running", running: true, want: rest},
		{
			name:     "issuer published another way",
			settings: sandbox.Settings{sandbox.KeyIssuer: testIssuer},
			want:     rest,
		},
		{
			name: "settings file and run flags",
			opts: sandbox.UpOptions{
				EnvFile: "my.env",
				Run:     sandbox.RunOptions{SkipZoneSetup: true, ReplaceActive: true},
			},
			want: "  go run ./cmd/sandbox --env my.env tunnel --listen 127.0.0.1:8480    # in a terminal of its own\n" +
				"  go run ./cmd/sandbox --env my.env bootstrap\n" +
				"  go run ./cmd/sandbox --env my.env mirror --listen 127.0.0.1:8480    # in another terminal\n" +
				"  go run ./cmd/sandbox --env my.env check-issuer\n" +
				"  go run ./cmd/sandbox --env my.env run --skip-zone-setup --replace-active-policy-set\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, tc.settings, "")
			if tc.running {
				writeFile(t, local.QuickIssuerFile(), "https://quick-brown-fox.trycloudflare.com\n")
			}
			out := &bytes.Buffer{}
			local.Out = out
			opts := tc.opts
			opts.Listen, opts.Print = listen, true
			if err := local.Up(t.Context(), opts); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Fatalf("output = %q, want %q", out, tc.want)
			}
			if got := readCalls(t, calls); got != "" {
				t.Fatalf("calls = %q, want none", got)
			}
		})
	}
}

// TestUpStops checks that Up stops at the failed step and prints the
// commands from that step on, and the background steps before it.
func TestUpStops(t *testing.T) {
	t.Parallel()
	const (
		cmd    = "  go run ./cmd/sandbox "
		tunnel = cmd + "tunnel --listen LISTEN    # in a terminal of its own\n"
		rest   = cmd + "mirror --listen LISTEN    # in another terminal\n" +
			cmd + "check-issuer\n" +
			cmd + "run\n"
	)
	tests := []struct {
		name     string
		settings sandbox.Settings
		tool     string
		// holdPort listens on the mirror's address before Up starts.
		holdPort bool
		want     []error
		wantText string
		wantLeft string
	}{
		{
			name:     "Docker is down",
			settings: sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyIssuer: testIssuer},
			tool:     clusterTool("FAIL", "", ""),
			want:     []error{sandbox.ErrDocker},
			wantText: "Is Docker running?",
			wantLeft: cmd + "bootstrap\n" + rest,
		},
		{
			name:     "named tunnel without an issuer",
			settings: sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyTunnel: namedTunnel},
			want:     []error{sandbox.ErrIssuer},
			wantText: "the named tunnel " + namedTunnel + " needs KEYCARD_LOCAL_ISSUER",
			wantLeft: tunnel + cmd + "bootstrap\n" + rest,
		},
		{
			name:     "mirror's port taken",
			settings: sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyIssuer: testIssuer},
			tool:     clusterTool(present, testIssuer, testIssuer),
			holdPort: true,
			want:     []error{sandbox.ErrService},
			wantText: "address already in use",
			wantLeft: rest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, tc.settings, tc.tool)
			out := &bytes.Buffer{}
			local.Out = out
			addr := listen
			if tc.holdPort {
				held, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = held.Close() }()
				addr = held.Addr().String()
			}
			err := local.Up(t.Context(), sandbox.UpOptions{Listen: addr})
			checkErr(t, err, tc.want, tc.wantText)
			want := leftHeader + strings.ReplaceAll(tc.wantLeft, "LISTEN", addr)
			if !strings.HasSuffix(out.String(), want) {
				t.Fatalf("output = %q, want it to end with %q", out, want)
			}
			if tc.tool == "" {
				if got := readCalls(t, calls); got != "" {
					t.Fatalf("calls = %q, want none", got)
				}
			}
		})
	}
}

// TestUpRecreates checks that Up asks before it deletes a cluster with
// another issuer, as down does, and deletes it only on yes. When it keeps
// the cluster, the commands left start with bootstrap. The issuer is a closed
// port, so after a recreate check-issuer fails at once and the commands left
// include the mirror. A nil ask is a stdin that is not a terminal.
func TestUpRecreates(t *testing.T) {
	t.Parallel()
	const closed = "https://127.0.0.1:1"
	tests := []struct {
		name     string
		ask      *confirmAsker
		recreate bool
		want     []error
		wantText string
	}{
		{
			name: "answered yes", ask: &confirmAsker{answer: true}, recreate: true,
			want: []error{sandbox.ErrIssuer}, wantText: "discovery refused",
		},
		{name: "answered no", ask: &confirmAsker{}, want: []error{sandbox.ErrConfirm}, wantText: nothingDeleted},
		{name: "no terminal", want: []error{sandbox.ErrConfirm, sandbox.ErrNoTerminal}, wantText: noTerminal},
		{
			name: "Ctrl-C", ask: &confirmAsker{err: sandbox.ErrInterrupted},
			want: []error{sandbox.ErrConfirm, sandbox.ErrInterrupted}, wantText: "nothing deleted: interrupted",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyIssuer: closed},
				recreateTool(closed))
			out := &bytes.Buffer{}
			local.Out = out
			tc.ask.use(local)
			err := local.Up(t.Context(), sandbox.UpOptions{Listen: freeAddr(t)})
			checkErr(t, err, tc.want, tc.wantText)
			if tc.ask != nil && !slices.Equal(tc.ask.asked, []string{deleteQuestion}) {
				t.Errorf("asked %q, want %q", tc.ask.asked, deleteQuestion)
			}
			got := readCalls(t, calls)
			wantOut := leftHeader + "  go run ./cmd/sandbox bootstrap\n"
			if tc.recreate {
				for _, want := range []string{
					"k3d KUBERC= cluster delete asp\n",
					"k3d KUBERC= cluster create asp ",
					"kubectl KUBERC=off --context k3d-asp get --raw /openid/v1/jwks\n",
				} {
					if !strings.Contains(got, want) {
						t.Errorf("calls lack %q\n%s", want, got)
					}
				}
				wantOut = leftHeader + "  go run ./cmd/sandbox mirror --listen 127.0.0.1:" /* port follows */
			} else if strings.Contains(got, "cluster delete") {
				t.Errorf("calls = %q, want no delete", got)
			}
			if !strings.Contains(out.String(), wantOut) {
				t.Errorf("output lacks %q\n%s", wantOut, out)
			}
		})
	}
}

// recreateTool is a fake k3d and kubectl for a cluster with another issuer.
// After the delete, the cluster list is empty, and the discovery document and
// tokens name issuerURL.
func recreateTool(issuerURL string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + issuerURL + `"}`))
	return fmt.Sprintf(`deleted="$CALLS.deleted"
issuer=https://other.example.com
[[ -f $deleted ]] && issuer=%[1]s
case "$FAKE_TOOL $*" in
"k3d cluster list -o json") [[ -f $deleted ]] && echo '[]' || echo '[{"name":"asp"}]' ;;
"k3d cluster delete asp") touch "$deleted" ;;
*"get --raw /.well-known/openid-configuration")
	printf '{"issuer":"%%s","jwks_uri":"%%s/openid/v1/jwks"}\n' "$issuer" "$issuer" ;;
*"get --raw /openid/v1/jwks") echo '{"keys":[{"kty":"RSA","kid":"k","n":"AQAB","e":"AQAB"}]}' ;;
*"create token default"*) echo 'e30.%[2]s.c2ln' ;;
esac
`, issuerURL, payload)
}

// freeAddr returns a loopback address with a port nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestUpStopsTunnel starts a quick tunnel and fails at bootstrap. It checks
// that the tunnel removed its issuer file before Up returned, and that the
// commands left start with the tunnel.
func TestUpStopsTunnel(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{sandbox.KeyCluster: cluster}, `
if [[ $FAKE_TOOL == cloudflared ]]; then
	echo "INF |  https://quick-brown-fox.trycloudflare.com  |" >&2
	exec sleep 60
fi
`+clusterTool("FAIL", "", ""))
	out := &bytes.Buffer{}
	local.Out = out
	err := local.Up(t.Context(), sandbox.UpOptions{Listen: listen})
	checkErr(t, err, []error{sandbox.ErrDocker}, "Is Docker running?")
	if _, statErr := os.Stat(local.QuickIssuerFile()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("issuer file after Up returned: %v, want it removed", statErr)
	}
	want := leftHeader +
		"  go run ./cmd/sandbox tunnel --listen 127.0.0.1:8480    # in a terminal of its own\n" +
		"  go run ./cmd/sandbox bootstrap\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q\n%s", want, out)
	}
}
