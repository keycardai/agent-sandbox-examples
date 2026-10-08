package sandbox_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
// commands from that step on.
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

// TestUpRecreates checks that Up asks before it deletes a cluster with
// another issuer. Enter or y recreates the cluster. n or EOF keeps it. The
// issuer is a closed port, so check-issuer fails at once and the commands
// left include the mirror.
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

// TestUpShell runs Up on an existing cluster with a scenario in shell mode.
// Up opens the command after the run. An empty line opens it again, c runs
// the checks, and the end of the answers ends Up. When the command fails, Up
// prints the error and asks again.
func TestUpShell(t *testing.T) {
	t.Parallel()
	const (
		shellCall = "exec -i " + userID + " -c agent -- agent-cli\n"
		curlCall  = "exec " + userID + " -c agent -- curl"
		prompt    = "Press Enter to open agent-cli again, type c and Enter to run the checks, or press Ctrl-C to stop.\n"
	)
	tests := []struct {
		name, answers string
		exit          int
		wantCalls     []string
		wantOut       []string
	}{
		{
			name:      "shell then checks then shell",
			answers:   "c\n\n",
			wantCalls: []string{shellCall, curlCall, shellCall},
			wantOut:   []string{"\n$ go run ./cmd/sandbox shell\nagent-cli ran\n", prompt},
		},
		{
			name:      "command fails",
			exit:      3,
			wantCalls: []string{shellCall},
			wantOut:   []string{"agent-cli ran\nagent: agent-cli exited: exit status 3\n", prompt},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls, out := existingUp(t, "run: {mode: shell, command: [agent-cli]}", fmt.Sprintf(`case "$*" in
*"exec -i %s -c agent -- agent-cli") echo "agent-cli ran"; exit %d ;;
esac
`, userID, tc.exit))
			err := local.Up(t.Context(), sandbox.UpOptions{
				Run: sandbox.RunOptions{SkipZoneSetup: true}, Answers: strings.NewReader(tc.answers),
			})
			if err != nil {
				t.Fatal(err)
			}
			got := readCalls(t, calls)
			checkInOrder(t, got[strings.LastIndex(got, "condition=Ready"):], tc.wantCalls)
			if n, want := strings.Count(got, shellCall), strings.Count(strings.Join(tc.wantCalls, ""), shellCall); n != want {
				t.Errorf("agent-cli ran %d times, want %d\n%s", n, want, got)
			}
			checkContains(t, "output", out.String(), tc.wantOut)
		})
	}
}

// checkInOrder requires got to contain each of wants, in order.
func checkInOrder(t *testing.T, got string, wants []string) {
	t.Helper()
	rest := got
	for _, want := range wants {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("calls lack %q in order\n%s", want, got)
		}
		rest = rest[i+len(want):]
	}
}

// TestUpBackground runs Up on an existing cluster in background mode. Up
// follows the agent's log until the agent exits, and returns its exit code.
func TestUpBackground(t *testing.T) {
	t.Parallel()
	const logs = "logs --follow "
	tests := []struct {
		name string
		// running is how many times the log ends while the agent runs.
		running   int
		wantCalls []string
		wantOut   string
	}{
		{
			name:      "agent exits",
			wantCalls: []string{logs + "--tail=-1 " + userID + " -c agent\n"},
			wantOut:   "\n$ go run ./cmd/sandbox logs\n",
		},
		{
			name:    "log ends while the agent runs",
			running: 1,
			wantCalls: []string{
				logs + "--tail=-1 " + userID + " -c agent\n", logs + "--tail=0 " + userID + " -c agent\n",
			},
			wantOut: "agent log line\nagent log line\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls, out := existingUp(t, "run: {mode: background}", fmt.Sprintf(`case "$*" in
*"logs --follow"*) echo "agent log line"; exit 0 ;;
*"state.terminated.exitCode"*)
	n=$(cat "$CALLS.ended" 2>/dev/null || echo 0); echo $((n + 1)) >"$CALLS.ended"
	(( n < %d )) || printf 3
	exit 0 ;;
esac
`, tc.running))
			err := local.Up(t.Context(), sandbox.UpOptions{
				Run: sandbox.RunOptions{SkipZoneSetup: true}, Answers: strings.NewReader(""),
			})
			checkErr(t, err, []error{sandbox.ErrAgent}, "the agent exited with code 3. Its log is above")
			got := readCalls(t, calls)
			checkInOrder(t, got, tc.wantCalls)
			if n := strings.Count(got, logs); n != len(tc.wantCalls) {
				t.Errorf("followed the log %d times, want %d\n%s", n, len(tc.wantCalls), got)
			}
			checkContains(t, "output", out.String(), []string{tc.wantOut})
		})
	}
}

// existingUp returns a Local for Up on an existing cluster, its calls file
// and its output. The scenario is agentScenario with run as its run key. The
// tool script runs tool before the fake cluster. The issuer is a test TLS
// server that KEYCARD_LOCAL_CA_FILE trusts, so check-issuer passes.
func existingUp(t *testing.T, run, tool string) (*sandbox.Local, string, *bytes.Buffer) {
	t.Helper()
	issuerURL, caFile, token := servedIssuer(t)
	settings := existing()
	settings[sandbox.KeyCAFile], settings[sandbox.KeyScenario] = caFile, agentName
	local, calls := newLocal(t, settings, tool+`case "$FAKE_TOOL $*" in
*"get --raw /.well-known/openid-configuration") echo '{"issuer":"`+issuerURL+`"}'; exit 0 ;;
*"create token default"*) echo '`+token+`'; exit 0 ;;
esac
`+healthy().script())
	linkCharts(t, local.Root)
	addScenario(t, local.Root, agentName, strings.Replace(agentScenario, "run: {mode: checks}", run, 1), false)
	writeFile(t, filepath.Join(local.Root, "local", "answers.yaml"), "repo: "+agentRepo+"\n")
	writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), zoneSetupOutput)
	out := &bytes.Buffer{}
	local.Out = out
	return local, calls, out
}
