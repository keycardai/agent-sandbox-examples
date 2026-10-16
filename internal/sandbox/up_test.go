package sandbox_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/issuer"
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
		// running fakes a running quick tunnel. It writes QuickIssuerFile and
		// locks it.
		running bool
		// killed writes QuickIssuerFile without the lock, as a killed quick
		// tunnel leaves it.
		killed bool
		// config is the settings file, when it is not the default.
		config string
		opts   sandbox.UpOptions
		want   string
	}{
		{name: "quick tunnel", want: tunnel + rest},
		{
			name:     "named tunnel",
			settings: sandbox.Settings{sandbox.KeyTunnel: namedTunnel, sandbox.KeyIssuer: testIssuer},
			want:     tunnel + rest,
		},
		{name: "quick tunnel already running", running: true, want: rest},
		{name: "quick tunnel killed", killed: true, want: tunnel + rest},
		{
			name:     "issuer published another way",
			settings: sandbox.Settings{sandbox.KeyIssuer: testIssuer},
			want:     rest,
		},
		{
			name:   "settings file and run flags",
			config: "/work/my.yaml",
			opts:   sandbox.UpOptions{Run: sandbox.RunOptions{SkipZoneSetup: true, ReplaceActive: true}},
			want: "  go run ./cmd/sandbox --config /work/my.yaml tunnel --listen 127.0.0.1:8480" +
				"    # in a terminal of its own\n" +
				"  go run ./cmd/sandbox --config /work/my.yaml bootstrap\n" +
				"  go run ./cmd/sandbox --config /work/my.yaml mirror --listen 127.0.0.1:8480    # in another terminal\n" +
				"  go run ./cmd/sandbox --config /work/my.yaml check-issuer\n" +
				"  go run ./cmd/sandbox --config /work/my.yaml run --skip-zone-setup --replace-active-policy-set\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, tc.settings, "")
			if tc.config != "" {
				local.Store = sandbox.NewStore(tc.config, nil, nil)
			}
			switch {
			case tc.running:
				holdIssuerFile(t, local.QuickIssuerFile(), "https://quick-brown-fox.trycloudflare.com\n")
			case tc.killed:
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
// IssuerWait is a minute, and only a failed discovery fetch may retry. With
// settings this session did not save, it says the commands do not see them.
func TestUpStops(t *testing.T) {
	t.Parallel()
	const (
		cmd    = "  go run ./cmd/sandbox "
		tunnel = cmd + "tunnel --listen LISTEN    # in a terminal of its own\n"
		rest   = cmd + "mirror --listen LISTEN    # in another terminal\n" +
			cmd + "check-issuer\n" +
			cmd + "run\n"
	)
	verifier, caFile := tlsIssuer(t)
	tests := []struct {
		name     string
		settings sandbox.Settings
		tool     string
		// holdPort listens on the mirror's address before Up starts.
		holdPort bool
		// unsaved gives Up settings this session did not save.
		unsaved  bool
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
			name:     "settings not saved",
			settings: sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyIssuer: testIssuer},
			tool:     clusterTool("FAIL", "", ""),
			unsaved:  true,
			want:     []error{sandbox.ErrDocker},
			wantLeft: strings.ReplaceAll(cmd+"bootstrap\n"+rest, "/sandbox ", "/sandbox --config - ") +
				"\nThe settings are not saved, so these commands do not see them. " +
				"Run go run ./cmd/sandbox init, save the settings, and run these commands without --config -.\n",
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
		{
			// The fetch passes, and the fake cluster mints an empty token.
			name: "token refused",
			settings: sandbox.Settings{
				sandbox.KeyCluster: cluster, sandbox.KeyIssuer: verifier, sandbox.KeyCAFile: caFile,
			},
			tool: clusterTool(present, verifier, verifier) + `case "$*" in
*"get --raw /openid/v1/jwks") echo '{"keys":[{"kty":"RSA","kid":"k","n":"AQAB","e":"AQAB"}]}' ;;
esac
`,
			want:     []error{sandbox.ErrIssuer, issuer.ErrToken},
			wantText: "token refused",
			wantLeft: rest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, tc.settings, tc.tool)
			if tc.unsaved {
				local.Store = sandbox.NewStore(sandbox.NoConfig, &sandbox.Config{}, nil)
			}
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
			err := local.Up(t.Context(), sandbox.UpOptions{Listen: addr, IssuerWait: time.Minute})
			checkErr(t, err, tc.want, tc.wantText)
			want := leftHeader + strings.ReplaceAll(tc.wantLeft, "LISTEN", addr)
			if !strings.HasSuffix(out.String(), want) {
				t.Fatalf("output = %q, want it to end with %q", out, want)
			}
			if strings.Contains(out.String(), "does not answer yet") {
				t.Fatalf("output = %q, want no retry", out)
			}
			if tc.tool == "" {
				if got := readCalls(t, calls); got != "" {
					t.Fatalf("calls = %q, want none", got)
				}
			}
		})
	}
}

// TestUpPaints colors Up's progress lines through Paint: the command
// before a step, the line that says Up stopped, and the commands left.
func TestUpPaints(t *testing.T) {
	t.Parallel()
	settings := sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyIssuer: testIssuer}
	local, _ := newLocal(t, settings, clusterTool("FAIL", "", ""))
	out := &bytes.Buffer{}
	local.Out = out
	local.Paint = func(tone sandbox.Tone, text string) string { return fmt.Sprintf("<%d>%s</%d>", tone, text, tone) }
	err := local.Up(t.Context(), sandbox.UpOptions{Listen: listen, IssuerWait: time.Minute})
	checkErr(t, err, []error{sandbox.ErrDocker}, "")
	for _, want := range []string{
		"\n<0>$ go run ./cmd/sandbox bootstrap</0>\n",
		"\n<3>Up stopped.</3> To finish by hand, run these commands from the repository root:\n",
		"\n  <0>go run ./cmd/sandbox bootstrap</0>\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
}

// TestUpRecreates checks that Up asks down's question before it deletes a
// cluster with another issuer, or one without a kubeconfig context, and
// deletes it only on yes. Up prints why, but not Bootstrap's advice to run
// down. When Up keeps the cluster, the commands left start with bootstrap.
// The issuer is a closed port, so after a recreate check-issuer fails at
// once and the commands left include the mirror. A nil ask means stdin is
// not a terminal.
func TestUpRecreates(t *testing.T) {
	t.Parallel()
	const closed = "https://127.0.0.1:1"
	tests := []struct {
		name     string
		ask      *confirmAsker
		recreate bool
		// before runs ahead of recreateTool.
		before   string
		want     []error
		wantText string
	}{
		{
			name: "answered yes", ask: &confirmAsker{answer: true}, recreate: true,
			want: []error{sandbox.ErrIssuer}, wantText: "discovery refused",
		},
		{
			name: "no kubeconfig context, answered yes", ask: &confirmAsker{answer: true}, recreate: true,
			before: noContextUntilDeleted,
			want:   []error{sandbox.ErrIssuer}, wantText: "discovery refused",
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
				tc.before+recreateTool(closed))
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
			for text, want := range map[string]bool{
				wantOut:                                  true,
				"was created with other settings":        tc.before == "",
				"your kubeconfig has no context k3d-asp": tc.before != "",
				deleteAdvice:                             false,
				"down --cluster asp":                     false,
			} {
				if got := strings.Contains(out.String(), text); got != want {
					t.Errorf("output has %q = %t, want %t\n%s", text, got, want, out)
				}
			}
		})
	}
}

// noContextUntilDeleted leaves the kubeconfig without the cluster's context
// until recreateTool deletes the cluster.
const noContextUntilDeleted = `[[ -f "$CALLS.deleted" ]] || ` + noContext

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

// tlsIssuer serves a discovery document and an empty JWKS over TLS. It
// returns the issuer URL and a CA file that trusts the server.
func tlsIssuer(t *testing.T) (string, string) {
	t.Helper()
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"issuer":%q,"jwks_uri":%q}`, server.URL, server.URL+"/openid/v1/jwks")
	})
	mux.HandleFunc("/openid/v1/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"keys":[]}`)
	})
	return server.URL, trustFile(t, server)
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

// TestUpShell runs Up on an existing cluster with a scenario in shell mode.
// Up opens the command after the run, then asks what to do next: open it
// again, run the checks, or stop. Ctrl-C ends Up. When the command fails, Up
// prints the error. Without a terminal, Up opens the command once and
// returns.
func TestUpShell(t *testing.T) {
	t.Parallel()
	const (
		shellCall = "exec -i " + userID + " -c agent -- agent-cli\n"
		curlCall  = "exec " + userID + " -c agent -- curl"
		question  = "What next? [Open agent-cli again, Run the checks, Stop up]"
	)
	tests := []struct {
		name string
		// ask is nil when stdin is not a terminal.
		ask       *choiceAsker
		exit      int
		wantCalls []string
		wantAsked []string
		wantOut   []string
	}{
		{
			name:      "shell then checks then shell",
			ask:       &choiceAsker{answers: []string{"checks", "shell"}},
			wantCalls: []string{shellCall, curlCall, shellCall},
			wantAsked: []string{question, question, question},
			wantOut:   []string{"\n$ go run ./cmd/sandbox --config - shell\nagent-cli ran\n"},
		},
		{
			name:      "command fails without a terminal",
			exit:      3,
			wantCalls: []string{shellCall},
			wantOut:   []string{"agent-cli ran\nagent: agent-cli exited: exit status 3\n"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls, out := existingUp(t, "run: {mode: shell, command: [agent-cli]}", fmt.Sprintf(`case "$*" in
*"exec -i %s -c agent -- agent-cli") echo "agent-cli ran"; exit %d ;;
esac
`, userID, tc.exit))
			if tc.ask != nil {
				local.Ask = tc.ask
			}
			if err := local.Up(t.Context(), sandbox.UpOptions{Run: sandbox.RunOptions{SkipZoneSetup: true}}); err != nil {
				t.Fatal(err)
			}
			got := readCalls(t, calls)
			checkInOrder(t, got[strings.LastIndex(got, "condition=Ready"):], tc.wantCalls)
			if n, want := strings.Count(got, shellCall), strings.Count(strings.Join(tc.wantCalls, ""), shellCall); n != want {
				t.Errorf("agent-cli ran %d times, want %d\n%s", n, want, got)
			}
			if tc.ask != nil && !slices.Equal(tc.ask.asked, tc.wantAsked) {
				t.Errorf("asked %q, want %q", tc.ask.asked, tc.wantAsked)
			}
			checkContains(t, "output", out.String(), tc.wantOut)
		})
	}
}

// choiceAsker answers each choice with the next of answers, and returns
// ErrInterrupted, as on Ctrl-C, once they run out. It records each question
// with the labels of its options. Any other kind of question panics, because
// the embedded Asker is nil.
type choiceAsker struct {
	sandbox.Asker

	answers []string
	asked   []string
}

func (a *choiceAsker) Choose(_ context.Context, question string, options []sandbox.Option, _ string) (string, error) {
	labels := make([]string, 0, len(options))
	for _, option := range options {
		labels = append(labels, option.Label)
	}
	a.asked = append(a.asked, question+" ["+strings.Join(labels, ", ")+"]")
	if len(a.answers) == 0 {
		return "", sandbox.ErrInterrupted
	}
	answer := a.answers[0]
	a.answers = a.answers[1:]
	return answer, nil
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
			wantOut:   "\n$ go run ./cmd/sandbox --config - logs\n",
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
			err := local.Up(t.Context(), sandbox.UpOptions{Run: sandbox.RunOptions{SkipZoneSetup: true}})
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
	withAnswers(local, map[string]string{repoID: agentRepo})
	writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), zoneSetupOutput)
	out := &bytes.Buffer{}
	local.Out = out
	return local, calls, out
}
