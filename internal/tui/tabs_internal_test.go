package tui

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/page"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/state"
)

// Sample hosts and endpoints the tab tests share.
const (
	githubHost     = "api.github.com"
	githubEndpoint = "api.github.com:443/"
	routerHost     = "openrouter.ai"
	routerEndpoint = "openrouter.ai:443/api/v1"
	pypiHost       = "pypi.org"
	sampleUser     = "ada@example.com"
	reasonRefused  = "exchange_denied"
	scopeRequest   = "request"
	credentialKey  = "credential"
	urlKey         = "url"
	// reasonUnanswered is the reason on a request the server did not
	// answer.
	reasonUnanswered = "upstream_failed"
	// The kinds of a demo's inputs, as its scenario file names them.
	inputGitHub = "github"
	inputSecret = "secret"
	inputText   = "text"
)

// piScenario is the pi demo's endpoints, under the default demo's name so
// that the model takes it for the settings' demo.
func piScenario() *sandbox.Scenario {
	credential := map[string]any{"placement": map[string]any{"kind": "header"}}
	return &sandbox.Scenario{
		Name: sandbox.DefaultScenario,
		Resources: []sandbox.ScenarioResource{
			{Identifier: "https://api.github.com", Name: "GitHub"},
			{Identifier: "https://github.com", Name: "GitHub git"},
			{Identifier: "https://openrouter.ai/api/v1", Name: "OpenRouter"},
		},
		Endpoints: []any{
			map[string]any{urlKey: "https://api.github.com", credentialKey: credential},
			map[string]any{urlKey: "https://github.com", credentialKey: credential},
			map[string]any{urlKey: "https://openrouter.ai/api/v1", credentialKey: credential},
			map[string]any{urlKey: "https://pypi.org"},
			map[string]any{urlKey: "https://files.pythonhosted.org"},
		},
		Run: sandbox.ScenarioRun{Mode: sandbox.ModeShell, Command: []string{"pi-session"}},
	}
}

// newRecord makes a proxy record. A request record gets a GET of path and,
// when allowed, a 200.
func newRecord(stamp time.Time, scope, host, path, verdict, reason, endpoint string) proxylog.Record {
	record := proxylog.Record{TS: stamp, Scope: scope}
	record.Destination.Host = host
	record.Decision.Verdict, record.Decision.Reason, record.Decision.Endpoint = verdict, reason, endpoint
	switch {
	case scope == proxylog.ScopeDNS:
		record.Query = &proxylog.Query{Type: "A", Rcode: "NOERROR"}
	case verdict == verdictAllow:
		record.Request = &proxylog.Request{Method: sampleMethod, Path: path}
		record.Outcome = &proxylog.Outcome{Status: http.StatusOK}
	default:
		record.Request = &proxylog.Request{Method: sampleMethod, Path: path}
	}
	return record
}

// sampleRecords are what the proxy writes for a lookup, a search domain
// the resolver tried, a GitHub request with the credential, a pypi.org
// request, an OpenRouter request Keycard refused, and a host off the allow
// list.
func sampleRecords(stamp time.Time) []proxylog.Record {
	return []proxylog.Record{
		newRecord(stamp, proxylog.ScopeDNS, githubHost, "", verdictAllow, sampleAllowed, githubEndpoint),
		newRecord(stamp, proxylog.ScopeDNS, "api.github.com.default.svc.cluster.local", "", verdictDeny, sampleReason,
			""),
		newRecord(stamp.Add(time.Second), scopeRequest, githubHost, "/user", verdictAllow, sampleAllowed, githubEndpoint),
		newRecord(stamp.Add(2*time.Second), scopeRequest, pypiHost, "/simple/", verdictAllow, sampleAllowed,
			"pypi.org:443/"),
		newRecord(stamp.Add(3*time.Second), scopeRequest, routerHost, "/api/v1/key", verdictDeny, reasonRefused,
			routerEndpoint),
		newRecord(stamp.Add(4*time.Second), proxylog.ScopeDNS, sampleDenied, "", verdictDeny, sampleReason, ""),
	}
}

// sampleTime is when the sample records were made: a fixed time of day,
// so that a screen that cuts a record's time short draws the same each
// time.
func sampleTime() time.Time {
	return time.Date(2026, 10, 9, 17, 31, 44, 0, time.Local) //nolint:gosmopolitan // The screens show local times.
}

// upModel returns a model of a sandbox that is up: the pi demo, the
// agent's pod ready, and the proxy's records of a few requests.
func upModel(t *testing.T) *model {
	t.Helper()
	root := testModel(t)
	settings := root.session.Local().Settings
	settings[sandbox.KeyUserEmail] = sampleUser
	settings[sandbox.KeyZoneURL] = "https://wvqowjom3ctf3eysdimuj4h745.keycard.cloud"
	root.screen = screenDashboard
	root.state.Loaded = piScenario()
	root.state.Reads.Status = sandbox.Status{
		Deployed: true, ClusterExists: true, Tunnel: sandbox.ServiceState{Running: true},
		Mirror: sandbox.ServiceState{Running: true},
	}
	root.state.Agents.Pods = []state.ContainerRow{
		{Pod: samplePod, Container: sandbox.ContainerStatus{
			Name: "egress-netsetup", Init: true, State: state.StateTerminated,
		}},
		{Pod: samplePod, Container: sandbox.ContainerStatus{Name: state.ProxyContainer, Ready: true, State: "running"}},
		{Pod: samplePod, Container: sandbox.ContainerStatus{Name: state.AgentContainer, Ready: true, State: "running"}},
	}
	root.shownPod = samplePod
	root.state.Agents.List = []sandbox.Agent{{Email: sampleUser, Pod: samplePod, Default: true}}
	root.state.Agents.Traffic.Reset(samplePod)
	for _, record := range sampleRecords(sampleTime()) {
		root.state.Agents.Traffic.Add(samplePod, record)
	}
	return root
}

// quietModel is a sandbox that is up, on an existing cluster that does not
// exist: work it starts only reads, and fails.
func quietModel(t *testing.T) *model {
	t.Helper()
	root := upModel(t)
	writeSettings(t, root)
	root.session.Local().Settings[sandbox.KeyKubeContext] = "keycard-test-none"
	root.state.ResetSteps()
	root.state.Demos = sampleDemos()
	return root
}

// TestHomeStates says what the sandbox does in each state, and what to do.
func TestHomeStates(t *testing.T) {
	t.Parallel()
	down := testModel(t)
	down.screen = screenDashboard
	bringingUp := upModel(t)
	bringingUp.state.Run.Job = state.Job{Kind: state.JobBringUp}
	bringingUp.state.Run.Steps[2].State = state.StepRunning
	bringingUp.state.Run.Cursor = 2
	failed := testModel(t)
	failed.screen = screenDashboard
	failed.Update(stepDoneMsg{index: 1, err: sandbox.ErrDocker})
	reading := testModel(t)
	reading.state.Reads.Done = false
	podsErr := upModel(t)
	podsErr.state.Agents.Pods, podsErr.state.Agents.PodsErr = nil, fmt.Errorf(
		"%w: the connection to the server was refused",
		sandbox.ErrPod,
	)
	clusterErr := testModel(t)
	clusterErr.state.Reads.Status.ClusterErr = fmt.Errorf("%w: cannot list k3d clusters", sandbox.ErrDocker)
	dockerDown := upModel(t)
	dockerDown.state.Reads.Status.ClusterExists = false
	dockerDown.state.Reads.Status.ClusterErr = clusterErr.state.Reads.Status.ClusterErr
	const isDown = "The sandbox is down"
	for name, tc := range map[string]struct {
		root *model
		want []string
	}{
		"down":    {down, []string{isDown, "There is no sandbox yet", "Bring it up", "bring up"}},
		"cluster": {clusterErr, []string{isDown, "The cluster might exist", "cannot list k3d clusters"}},
		"docker":  {dockerDown, []string{isDown, "The cluster might exist", "cannot list k3d clusters"}},
		"reading": {reading, []string{"Reading the sandbox"}},
		"pods":    {podsErr, []string{"Cannot read the sandbox's pods", "the connection to the server was refused"}},
		"coming":  {bringingUp, []string{"Bringing up your sandbox", "Start the issuer mirror", "START THE ISSUER MIRROR"}},
		"failed": {failed, []string{
			"The bring-up stopped at: Create or check the cluster", "→ Start Docker",
			"Start again from: " + failed.state.Run.Steps[0].Step.Title,
		}},
		"up": {upModel(t), []string{
			sampleUser + "'s agent is running", "acts as " + sampleUser, "does not hold any secrets", "WHAT KEYCARD DID",
			"GitHub", "1 request, credential added", "No credential: Keycard's policy does not allow it",
			"not used yet", "no credential needed", "Blocked", sampleDenied, "TRY IT", "Open pi-session",
		}},
	} {
		view := ansi.Strip(tc.root.View().Content)
		for _, want := range tc.want {
			if !strings.Contains(view, want) {
				t.Errorf("%s: Home lacks %q:\n%s", name, want, view)
			}
		}
	}
}

// sampleDemos are the checkout's three demos, as their scenario files
// describe them.
func sampleDemos() []*sandbox.Scenario {
	piDemo := piScenario()
	piDemo.Name, piDemo.Title = "pi", samplePi
	piDemo.Inputs = []sandbox.ScenarioInput{
		{ID: inputGitHub, Kind: inputGitHub},
		{ID: "openrouter-api-key", Kind: inputSecret, Prompt: "OpenRouter API key"},
		{ID: "repo", Kind: inputText, Prompt: "GitHub repository for pi to work in, as owner/name"},
		{ID: "model", Kind: inputText, Prompt: "OpenRouter model for pi", Default: "anthropic/claude-sonnet-5"},
	}
	slack := &sandbox.Scenario{
		Name:  sampleSlack,
		Title: "Slack agent for GitHub and Linear",
		Run:   sandbox.ScenarioRun{Mode: sandbox.ModeBackground},
		Inputs: []sandbox.ScenarioInput{
			{ID: "slack-bot-token", Kind: inputSecret, Prompt: "Slack bot token (xoxb-), with the im:write scope"},
			{
				ID:     "slack-user",
				Kind:   inputText,
				Prompt: "Your Slack member ID, such as U012AB3CDE. The agent answers only you.",
			},
			{ID: inputGitHub, Kind: inputGitHub},
			{ID: "linear-api-key", Kind: inputSecret, Prompt: "Linear personal API key"},
		},
	}
	smoke := &sandbox.Scenario{Name: sandbox.DefaultScenario, Title: sampleSmoke}
	return []*sandbox.Scenario{piDemo, slack, smoke}
}

// TestDemoPicker lists every demo with the current one marked, and enter
// starts the chosen demo's questions with the demo already chosen.
func TestDemoPicker(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.state.Demos = sampleDemos()
	view := ansi.Strip(root.View().Content)
	if !strings.Contains(view, "Switch to another demo: pi or slack") || !strings.Contains(view, "switch demo") {
		t.Errorf("Home does not offer the other demos:\n%s", view)
	}
	root.Update(press("p"))
	view = ansi.Strip(root.View().Content)
	if root.overlay.kind != overlayDemo || !strings.Contains(view, "▸ pi") {
		t.Fatalf("p: overlay %v; want the picker on pi:\n%s", root.overlay.kind, view)
	}
	for _, want := range []string{
		"Switch the demo", samplePi, "a opens pi-session", "Slack agent for GitHub and Linear",
		"Smoke checks  ● running now", "Asks for GitHub, OpenRouter API key, GitHub repository for pi to work in.",
		"Asks for Slack bot token, Slack member ID, GitHub, Linear personal API key.", "Asks for nothing.",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("picker lacks %q:\n%s", want, view)
		}
	}
	root.Update(press("j"))
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	defer root.stopSetup()
	if root.overlay.kind == overlayDemo || root.screen != screenWizard || !root.state.Run.Job.Is(state.JobSetup) {
		t.Errorf("enter: overlay %v, screen %v, setup running %v; want the wizard", root.overlay.kind, root.screen,
			root.state.Run.Job.Is(state.JobSetup))
	}
}

// TestDemoPickerWhileDown marks the demo the settings name as chosen, and
// none as running, while the sandbox is down.
func TestDemoPickerWhileDown(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.state.Demos = sampleDemos()
	root.Update(press("p"))
	if view := ansi.Strip(root.View().Content); !strings.Contains(view, "Smoke checks  ● chosen") ||
		strings.Contains(view, "running now") {
		t.Errorf("the picker while the sandbox is down:\n%s", view)
	}
}

// TestReviewLaunchesOnU launches from the review with u as well as enter.
func TestReviewLaunchesOnU(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.screen = screenReview
	root.state.Problems = []error{sandbox.ErrSettings}
	root.Update(press("u"))
	if !strings.Contains(root.ui.Toast.Text, "Fix the settings first") {
		t.Errorf("u on the review: notice %q, want the launch to run", root.ui.Toast.Text)
	}
}

// TestDeadTunnelHint says what happened when Keycard cannot resolve a
// quick tunnel's name, and what u does.
func TestDeadTunnelHint(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	issuer := slices.IndexFunc(root.state.Run.Steps, func(row *state.StepRow) bool {
		return row.Step.ID == sandbox.StepIssuer
	})
	err := fmt.Errorf("%w: discovery refused: dial tcp: lookup given-shareware-tales-beta.trycloudflare.com: "+
		"no such host", sandbox.ErrIssuer)
	if hint := page.StepHint(root.state.Run.Steps[issuer], err); !strings.Contains(hint, "starts a new tunnel") {
		t.Errorf("hint = %q, want it to say u starts a new tunnel", hint)
	}
}

// TestServerErrorAfterCredential says that the credential was added and the
// server still refused the request.
func TestServerErrorAfterCredential(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	record := newRecord(time.Now(), scopeRequest, routerHost, "/api/v1/messages", verdictAllow, sampleAllowed,
		routerEndpoint)
	record.Outcome.Status = http.StatusNotFound
	root.state.Agents.Traffic.Add(samplePod, record)
	view := ansi.Strip(root.View().Content)
	for _, want := range []string{"1 request, credential added", "but the server answered 404 Not Found"} {
		if !strings.Contains(view, want) {
			t.Errorf("Home lacks %q:\n%s", want, view)
		}
	}
}

// TestUnansweredRequests counts the requests the server did not answer:
// on their own for an endpoint with no other request, and after the
// answered ones otherwise.
func TestUnansweredRequests(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	stamp := sampleTime().Add(5 * time.Second)
	root.state.Agents.Traffic.Add(
		samplePod,
		newRecord(stamp, scopeRequest, "github.com", "/org/repo.git/info/refs", verdictDeny,
			reasonUnanswered, "github.com:443/"),
	)
	root.state.Agents.Traffic.Add(
		samplePod,
		newRecord(stamp, scopeRequest, githubHost, "/user", verdictDeny, reasonUnanswered,
			githubEndpoint),
	)
	view := ansi.Strip(root.View().Content)
	for target, wants := range map[string][]string{
		" github.com ":     {"✕ GitHub git", "1 request, the server did not answer"},
		" api.github.com ": {"✓ GitHub", "1 request, credential added, 1 not answered"},
	} {
		line := lineWith(view, target)
		for _, want := range wants {
			if !strings.Contains(line, want) {
				t.Errorf("Home says %q for%s, want %q", line, target, want)
			}
		}
	}
}

// lineWith returns the first line of view that holds text, or "".
func lineWith(view, text string) string {
	for line := range strings.Lines(view) {
		if strings.Contains(line, text) {
			return line
		}
	}
	return ""
}

// TestBlockedReasons names each blocked host with the reason the proxy
// gave for it, not the newest host's reason for all of them.
func TestBlockedReasons(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	stamp := time.Now()
	root.state.Agents.Traffic.Add(samplePod, newRecord(stamp, proxylog.ScopeDNS, sampleDenied, "", verdictDeny,
		sampleReason, ""))
	root.state.Agents.Traffic.Add(samplePod, newRecord(stamp.Add(time.Second), scopeRequest, "pypi.org", "/simple/",
		verdictDeny, "malformed_request", ""))
	view := ansi.Strip(root.View().Content)
	for _, want := range []string{
		"pypi.org · " + proxylog.ReasonText("malformed_request"),
		sampleDenied + " · " + proxylog.ReasonText(sampleReason),
	} {
		if !strings.Contains(view, want) {
			t.Errorf("Home lacks %q:\n%s", want, view)
		}
	}
}
