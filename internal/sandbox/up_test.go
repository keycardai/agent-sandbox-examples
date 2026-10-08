package sandbox_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

const (
	leftHeader = "\nUp stopped. To finish by hand, run these commands from the repository root:\n"
	listen     = "127.0.0.1:8480"
)

// TestUpPrint prints the steps as commands and does not run them.
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
		// running writes the URL a quick tunnel writes while it runs.
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

// TestUpStops stops at the step that fails, and prints the commands that
// finish the run by hand from that step.
func TestUpStops(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyIssuer: testIssuer},
		clusterTool("FAIL", "", ""))
	out := &bytes.Buffer{}
	local.Out = out
	err := local.Up(t.Context(), sandbox.UpOptions{Listen: listen, Answers: strings.NewReader("")})
	checkErr(t, err, []error{sandbox.ErrDocker}, "Is Docker running?")
	want := leftHeader +
		"  go run ./cmd/sandbox bootstrap\n" +
		"  go run ./cmd/sandbox mirror --listen 127.0.0.1:8480    # in another terminal\n" +
		"  go run ./cmd/sandbox check-issuer\n" +
		"  go run ./cmd/sandbox run\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Fatalf("output = %q, want it to end with %q", out, want)
	}
}

// TestUpRecreates asks before it deletes a cluster created with another
// issuer. Enter or y creates the cluster again and starts the mirror. Nothing
// listens at the issuer's address, so check-issuer fails at once and Up
// stops there. The mirror stopped with Up, so it is listed again. An answer
// of n, or no answer at all, keeps the cluster.
func TestUpRecreates(t *testing.T) {
	t.Parallel()
	const closed = "https://127.0.0.1:1"
	type outcome struct {
		want      []error
		wantText  string
		wantCalls []string
		wantOut   string
	}
	recreated := outcome{
		want:     []error{sandbox.ErrIssuer},
		wantText: "discovery refused",
		wantCalls: []string{
			"k3d KUBERC= cluster delete asp\n",
			"k3d KUBERC= cluster create asp ",
			"kubectl KUBERC=off --context k3d-asp get --raw /openid/v1/jwks\n",
		},
		wantOut: leftHeader + "  go run ./cmd/sandbox mirror --listen 127.0.0.1:", /* port follows */
	}
	kept := outcome{
		want:     []error{sandbox.ErrConfirm},
		wantText: "the cluster has another issuer",
		wantOut:  "Delete the cluster and create it again with the new issuer? [Y/n] ",
	}
	tests := []struct {
		outcome

		name, answer string
	}{
		{name: "pressed Enter", answer: "\n", outcome: recreated},
		{name: "answered y", answer: "y\n", outcome: recreated},
		{name: "answered n", answer: "n\n", outcome: kept},
		{name: "no answer", answer: "", outcome: kept},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyIssuer: closed},
				recreateTool(closed))
			out := &bytes.Buffer{}
			local.Out = out
			err := local.Up(t.Context(), sandbox.UpOptions{Listen: freeAddr(t), Answers: strings.NewReader(tc.answer)})
			checkErr(t, err, tc.want, tc.wantText)
			got := readCalls(t, calls)
			for _, want := range tc.wantCalls {
				if !strings.Contains(got, want) {
					t.Errorf("calls lack %q\n%s", want, got)
				}
			}
			if len(tc.wantCalls) == 0 && strings.Contains(got, "cluster delete") {
				t.Errorf("calls = %q, want no delete", got)
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("output lacks %q\n%s", tc.wantOut, out)
			}
		})
	}
}

// recreateTool is a fake k3d and kubectl for a cluster created with another
// issuer. After k3d deletes it, the cluster list is empty, and the
// cluster's discovery document and tokens name issuerURL.
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

// TestUpStopsTunnel starts a quick tunnel, then stops at bootstrap. The
// tunnel has removed its issuer file by the time Up returns, and the
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
	err := local.Up(t.Context(), sandbox.UpOptions{Listen: listen, Answers: strings.NewReader("")})
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
