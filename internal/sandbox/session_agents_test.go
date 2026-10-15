package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// TestAddAgent offers only the zone's users without an agent in the demo,
// asks the per-user input for the new one alone, and gives them an agent in
// that demo alone. Problems then names an agent whose answer is gone.
func TestAddAgent(t *testing.T) {
	t.Parallel()
	run := firstRun(t)
	local := run.local
	addScenario(t, local.Root, teamName, teamScenario, false)
	editConfig(t, run, func(config *sandbox.Config) {
		config.Demo.Name, config.Demo.Answers = teamName, map[string]string{repoID: agentRepo}
		config.Demo.People = map[string]map[string]string{email: {chatID: adaChat}}
	})
	ask := &scriptedAsker{t: t, answers: map[string][]string{
		sandbox.QuestionNewUser: {otherEmail}, "Chat ID of " + otherEmail: {graceChat},
	}}
	local.Ask = ask
	added, err := local.AddAgent(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if added != otherEmail {
		t.Errorf("AddAgent = %q, want %q", added, otherEmail)
	}
	if offered := ask.offered[sandbox.QuestionNewUser]; slices.Contains(offered, email) ||
		!slices.Contains(offered, otherEmail) {
		t.Errorf("offered %q, want %s and not %s", offered, otherEmail, email)
	}
	checkNotAsked(t, ask, []string{"Chat ID of " + email})
	checkUsers(t, local, teamName, otherEmail)
	checkUsers(t, local, sandbox.DefaultScenario)

	editConfig(t, run, func(config *sandbox.Config) {
		config.Demo.People = map[string]map[string]string{email: {chatID: adaChat}}
	})
	problems := local.NewSession("127.0.0.1:8480", time.Second).Problems()
	if len(problems) != 1 || !strings.Contains(problems[0].Error(), otherEmail+"'s agent: ") ||
		sandbox.ProblemSection(problems[0]) != sandbox.SectionScenario {
		t.Errorf("problems = %v, want one about %s's agent, fixed in the demo section", problems, otherEmail)
	}
}

// TestAddAgentEveryoneHasOne says once that everyone in the zone has an
// agent, however often the operator checks again.
func TestAddAgentEveryoneHasOne(t *testing.T) {
	t.Parallel()
	run := firstRun(t)
	local := run.local
	editConfig(t, run, func(config *sandbox.Config) {
		config.Agents.Demos = map[string][]string{config.Demo.Name: {otherEmail}}
	})
	out := &bytes.Buffer{}
	local.Out = out
	local.Ask = &checkingAsker{scriptedAsker: &scriptedAsker{t: t}, checks: 3}
	_, err := local.AddAgent(t.Context())
	if !errors.Is(err, errChecked) {
		t.Fatalf("AddAgent = %v, want it to stop after the checks", err)
	}
	if got := strings.Count(out.String(), "Everyone in the zone has an agent in this demo."); got != 1 {
		t.Errorf("the note shows %d times, want once\n%s", got, out)
	}
}

// errChecked stops setup once checkingAsker has checked again enough.
var errChecked = errors.New("checked enough")

// checkingAsker checks again for someone without an agent as often as
// checks says, then stops setup with errChecked.
type checkingAsker struct {
	*scriptedAsker

	checks int
}

func (a *checkingAsker) Choose(
	ctx context.Context,
	question string,
	options []sandbox.Option,
	value string,
) (string, error) {
	if question != sandbox.QuestionNewUser {
		return a.scriptedAsker.Choose(ctx, question, options, value)
	}
	if a.checks == 0 {
		return "", errChecked
	}
	a.checks--
	return options[len(options)-1].Value, nil
}

// twoAgents gives grace an agent in firstRun's demo, next to ada's, and
// an answer for each of them.
func twoAgents(t *testing.T) *sandbox.Local {
	t.Helper()
	run := firstRun(t)
	local := run.local
	editConfig(t, run, func(config *sandbox.Config) {
		config.Agents.Demos = map[string][]string{config.Demo.Name: {otherEmail}}
		config.Demo.Answers = map[string]string{repoID: agentRepo}
		config.Demo.People = map[string]map[string]string{email: {chatID: adaChat}, otherEmail: {chatID: graceChat}}
	})
	return local
}

// TestRemoveAgent takes an agent out of the demo's settings and its
// answers, and moves the commands to the agent of the person setup picked.
// It refuses that person, and someone without an agent.
func TestRemoveAgent(t *testing.T) {
	t.Parallel()
	local := twoAgents(t)
	session := local.NewSession("127.0.0.1:8480", time.Second)
	session.SelectAgent(otherEmail)
	out := &bytes.Buffer{}
	if err := session.RemoveAgent(t.Context(), otherEmail, out); err != nil {
		t.Fatal(err)
	}
	checkContains(t, "output", out.String(), []string{"Removed " + otherEmail + "'s agent from the settings."})
	checkUsers(t, session.Local(), sandbox.DefaultScenario)
	checkAnswers(t, local, map[string]map[string]string{email: {chatID: adaChat}})
	if session.Selected() != email {
		t.Errorf("Selected = %q after removing the selected agent, want %q", session.Selected(), email)
	}
	err := session.RemoveAgent(t.Context(), email, out)
	checkErr(t, err, []error{sandbox.ErrSettings}, "every demo has an agent for "+email)
	err = session.RemoveAgent(t.Context(), otherEmail, out)
	checkErr(t, err, []error{sandbox.ErrSettings}, otherEmail+" has no agent in this demo")
}

// TestRemoveDeployedAgent deletes the sandbox of an agent the cluster runs,
// then takes them out of the settings and runs zone-setup. When zone-setup
// fails, RemoveAgent runs again for them.
func TestRemoveDeployedAgent(t *testing.T) {
	t.Parallel()
	local := twoAgents(t)
	setTool(t, local, `case "$FAKE_TOOL $1 $2" in "k3d cluster list") echo '[{"name":"keycard-sandbox-local"}]' ;; esac`)
	writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), zoneSetupOutput+secondAgent)
	holdIssuerFile(t, local.QuickIssuerFile(), "https://quick-brown-fox.trycloudflare.com\n")
	session := local.NewSession("127.0.0.1:8480", time.Second)
	for range 2 {
		out := &bytes.Buffer{}
		err := session.RemoveAgent(t.Context(), otherEmail, out)
		checkErr(t, err, []error{sandbox.ErrZoneSetup}, fakeAPIStops)
		checkContains(t, "output", out.String(),
			[]string{"Deleted the sandbox of user " + otherUserID + ", and its pod."})
		checkUsers(t, session.Local(), sandbox.DefaultScenario)
	}
	// zone-setup.yaml still lists the removed agent, and the bring-up must
	// not create their sandbox again.
	err := session.Run(t.Context(), sandbox.StepSandbox, &bytes.Buffer{})
	checkErr(t, err, []error{sandbox.ErrZoneSetup}, otherEmail+" has no agent in this demo")
}

// TestDeployAgent runs zone-setup before it changes the cluster, so that
// no sandbox comes up for someone zone-setup did not set up.
func TestDeployAgent(t *testing.T) {
	t.Parallel()
	local := twoAgents(t)
	holdIssuerFile(t, local.QuickIssuerFile(), "https://quick-brown-fox.trycloudflare.com\n")
	calls := filepath.Join(local.Root, "calls")
	if err := os.Remove(calls); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	err := local.NewSession("127.0.0.1:8480", time.Second).DeployAgent(t.Context(), otherEmail, &bytes.Buffer{})
	checkErr(t, err, []error{sandbox.ErrZoneSetup}, fakeAPIStops)
	if got := readCalls(t, calls); got != "" {
		t.Errorf("calls = %q, want none", got)
	}
}

// setTool makes every tool of a Local from newLocal run record and then
// body.
func setTool(t *testing.T, local *sandbox.Local, body string) {
	t.Helper()
	//nolint:gosec // G306: the fake tool must be executable.
	if err := os.WriteFile(filepath.Join(local.Root, "tool"), []byte(record+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// TestAgentRequest reports a request curl could not complete, as when the
// proxy refused it, in the response's Failure. A request that did not reach
// curl, as when the pod is gone, is an ErrAgent error.
func TestAgentRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body  string
		want        []error
		wantText    string
		wantFailure string
	}{
		{
			name: "curl fails",
			body: "echo 'curl: (6) Could not resolve host: example.com' >&2\n" +
				"echo 'command terminated with exit code 6' >&2\nexit 6\n",
			wantFailure: "curl: (6) Could not resolve host: example.com",
		},
		{
			name:     "pod gone",
			body:     `echo 'Error from server (NotFound): pods "` + userID + `" not found' >&2` + "\nexit 1\n",
			want:     []error{sandbox.ErrAgent},
			wantText: `pods "` + userID + `" not found`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, _ := newLocal(t, sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyUserEmail: email}, tc.body)
			writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), zoneSetupOutput)
			session := local.NewSession("127.0.0.1:8480", time.Second)
			response, err := session.AgentRequest(t.Context(), "GET", "https://example.com", "")
			checkErr(t, err, tc.want, tc.wantText)
			if response.Failure != tc.wantFailure {
				t.Errorf("Failure = %q, want %q", response.Failure, tc.wantFailure)
			}
		})
	}
}

// TestEndpointList names each endpoint's resource, and its ID matches the
// endpoint the proxy names on an audit record.
func TestEndpointList(t *testing.T) {
	t.Parallel()
	const credentialKey, urlKey = "credential", "url"
	scenario := &sandbox.Scenario{
		Resources: []sandbox.ScenarioResource{
			{Identifier: "https://api.github.com", Name: "GitHub"},
			{Identifier: openRouterAPI + "/", Name: openRouterName},
		},
		Endpoints: []any{
			map[string]any{urlKey: "https://api.github.com", credentialKey: map[string]any{}},
			map[string]any{urlKey: openRouterAPI, credentialKey: map[string]any{}},
			map[string]any{urlKey: "https://pypi.org"},
			map[string]any{urlKey: "https://localhost:8443/v1"},
			map[string]any{credentialKey: map[string]any{}},
			"not a map",
		},
	}
	got := scenario.EndpointList()
	want := []struct {
		id, host, name string
		credential     bool
	}{
		{"api.github.com:443/", "api.github.com", "GitHub", true},
		{"openrouter.ai:443/api/v1", "openrouter.ai", openRouterName, true},
		{"pypi.org:443/", "pypi.org", "", false},
		{"localhost:8443/v1", "localhost", "", false},
	}
	if len(got) != len(want) {
		t.Fatalf("EndpointList = %+v, want %d endpoints", got, len(want))
	}
	for i, endpoint := range got {
		if endpoint.ID() != want[i].id || endpoint.Host() != want[i].host || endpoint.Name != want[i].name ||
			endpoint.Credential != want[i].credential {
			t.Errorf("endpoint %d = %+v (ID %q, host %q), want %+v", i, endpoint, endpoint.ID(), endpoint.Host(), want[i])
		}
	}
}

// TestAgentCurl runs AgentCurl's command in the agent's container word for
// word, so that the command the TUI shows is the one that ran. Quoted, it
// is one line to paste.
func TestAgentCurl(t *testing.T) {
	t.Parallel()
	local, calls := newLocal(t, sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyUserEmail: email},
		`printf '%s\0' "$@" >"$CALLS.args"`+"\n"+`printf '{"ok":true}\n__sandbox_status__:200'`+"\n")
	writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), zoneSetupOutput)
	session := local.NewSession("127.0.0.1:8480", time.Second)
	method, address, body := "POST", "https://api.linear.app/graphql", `{"query":"{ viewer { id } }"}`
	response, err := session.AgentRequest(t.Context(), method, address, body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != 200 || response.Body != `{"ok":true}` {
		t.Errorf("response = %d %q, want 200 and the body", response.Status, response.Body)
	}
	raw, err := os.ReadFile(calls + ".args") //nolint:gosec // G304: the path is the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	curl := sandbox.AgentCurl(method, address, body)
	if ran := args[slices.Index(args, "--")+1:]; !slices.Equal(ran, curl) {
		t.Errorf("the container ran %q, want %q", ran, curl)
	}
	want := `curl -sS -m 20 -X POST -H 'Authorization: Bearer placeholder' ` +
		`-w '\n__sandbox_status__:%{http_code}' -H 'Content-Type: application/json' ` +
		`--data '{"query":"{ viewer { id } }"}' https://api.linear.app/graphql`
	if got := strings.Join(sandbox.ShellWords(curl), " "); got != want {
		t.Errorf("the command reads\n%s\nwant\n%s", got, want)
	}
}

// TestSessionAgents lists an agent for each person the settings give one
// in the demo, with the pod of each the zone step ran for, and points Target
// at the selected one.
func TestSessionAgents(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{sandbox.KeyCluster: cluster, sandbox.KeyUserEmail: email},
		`case "$*" in *"get pods -o json") echo '{"items":[]}' ;; esac`)
	local.Store = sandbox.NewStore(sandbox.NoConfig,
		&sandbox.Config{Agents: sandbox.ConfigAgents{Default: email, Demos: map[string][]string{
			sandbox.DefaultScenario: {otherEmail}, teamName: {"alan@example.com"},
		}}}, nil)
	outPath := filepath.Join(local.Root, "local", "out", "zone-setup.yaml")
	writeFile(t, outPath, zoneSetupOutput)
	session := local.NewSession("127.0.0.1:8480", time.Second)
	want := []sandbox.Agent{{Email: email, Pod: userID, Default: true}, {Email: otherEmail}}
	if got := session.Agents(); !slices.Equal(got, want) {
		t.Errorf("Agents = %+v, want %+v", got, want)
	}
	if target, err := session.Target(); err != nil || target.Pod != userID {
		t.Errorf("Target = %+v, %v; want the pod of %s", target, err, email)
	}
	session.SelectAgent(otherEmail)
	if _, err := session.Target(); !errors.Is(err, sandbox.ErrZoneSetup) {
		t.Errorf("Target of an agent the zone step has not run for: %v, want ErrZoneSetup", err)
	}
	// The pods are every agent's, whichever is selected.
	if _, err := session.Pods(t.Context()); err != nil {
		t.Errorf("Pods with an agent selected that has no pod: %v", err)
	}
	writeFile(t, outPath, zoneSetupOutput+secondAgent)
	if target, err := session.Target(); err != nil || target.Pod != otherUserID {
		t.Errorf("Target = %+v, %v; want the pod of %s", target, err, otherEmail)
	}
}
