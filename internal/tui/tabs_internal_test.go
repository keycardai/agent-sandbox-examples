package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
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
	githubUserURL    = "https://api.github.com/user"
	// paneAgent and paneRequests are the Agents tab's panes with the agent
	// and with the requests.
	paneAgent    = "Agent"
	paneRequests = "Try a request"
	// The kinds of a demo's inputs, as its scenario file names them.
	inputGitHub = "github"
	inputSecret = "secret"
	inputText   = "text"
)

// focusedTitle finds the title of the pane with focus, which has the heavy
// border.
var focusedTitle = regexp.MustCompile(`┏━ (.+?) ━`)

// focusedPane names the pane with focus in a view.
func focusedPane(view string) string {
	match := focusedTitle.FindStringSubmatch(view)
	if match == nil {
		return ""
	}
	return match[1]
}

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
		Try: []sandbox.ScenarioTry{
			{Method: http.MethodGet, URL: "https://api.github.com/user"},
			{Method: http.MethodGet, URL: "https://openrouter.ai/api/v1/key"},
			{Method: http.MethodGet, URL: "https://pypi.org/pypi/requests/json"},
		},
		Title: samplePi,
		Run:   sandbox.ScenarioRun{Mode: sandbox.ModeShell, Command: []string{"pi-session"}},
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
	root.state.Demos.Loaded = piScenario()
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

// quietModel is a sandbox that is up with three agents, on an existing
// cluster that does not exist: work it starts only reads, and fails.
func quietModel(t *testing.T) *model {
	t.Helper()
	root := teamModel(t)
	writeSettings(t, root)
	root.session.Local().Settings[sandbox.KeyKubeContext] = "keycard-test-none"
	root.state.ResetSteps()
	root.state.Demos.List = sampleDemos()
	return root
}

// respond answers the request the Agents tab asked for with response,
// after the proxy's record of it, and returns the view.
func respond(root *model, send tea.Cmd, record proxylog.Record, response sandbox.AgentResponse) string {
	asked, _ := send().(page.SendRequestMsg)
	root.state.Agents.Traffic.Add(samplePod, record)
	root.Update(page.RequestDoneMsg{
		Agent: asked.Agent, Seq: asked.Seq, Method: asked.Method, URL: asked.URL, Response: response,
	})
	return ansi.Strip(root.View().Content)
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
			"Send a request from the agent",
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

// TestAgentTab shows the agent and the requests on the left, and the
// agent's log on the right. With the requests in focus, the right shows
// the last response, and the request selected says what it needs.
func TestAgentTab(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(press("2"))
	view := ansi.Strip(root.View().Content)
	for _, want := range []string{
		paneAgent, sampleUser, "running · pod " + samplePod, paneRequests, "GET api.github.com/user",
		"Needs " + sampleUser + "'s GitHub", "Press a to open pi-session", "Container log",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("Agents tab lacks %q:\n%s", want, view)
		}
	}
	down := tea.KeyPressMsg{Code: tea.KeyDown}
	root.Update(down)
	if view = ansi.Strip(root.View().Content); focusedPane(view) != paneRequests ||
		!strings.Contains(view, "Response") || !strings.Contains(view, "Nothing sent yet") {
		t.Errorf("down from the agent: focus %q, want the requests and the response:\n%s", focusedPane(view), view)
	}
	for range 5 {
		root.Update(down)
	}
	if view = ansi.Strip(root.View().Content); !strings.Contains(view, "Not on the allow list") {
		t.Errorf("the last request does not say it is blocked:\n%s", view)
	}
}

// TestUpToTheAgent goes from the requests back to the agent and its log,
// up past the first request.
func TestUpToTheAgent(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(press("2"))
	for range 5 {
		root.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	for range 4 {
		act(root, tea.KeyPressMsg{Code: tea.KeyUp})
	}
	if view := ansi.Strip(root.View().Content); focusedPane(view) != paneAgent ||
		!strings.Contains(view, "▸ GET api.github.com/user") || !strings.Contains(view, "Container log") {
		t.Errorf("up past the first request: focus %q, want the agent and its log, the first request selected:\n%s",
			focusedPane(view), view)
	}
}

// TestAgentPanes goes round the panes with tab, and esc on the right goes
// back to the pane it shows for.
func TestAgentPanes(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(press("2"))
	tab := tea.KeyPressMsg{Code: tea.KeyTab}
	for _, want := range []string{paneRequests, "Response", paneAgent} {
		root.Update(tab)
		if got := focusedPane(ansi.Strip(root.View().Content)); got != want {
			t.Fatalf("focus = %q after tab, want %q", got, want)
		}
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	root.Update(tab)
	if got := focusedPane(ansi.Strip(root.View().Content)); got != "Response" ||
		root.ui.Logs.Source() != state.SourceResponse {
		t.Fatalf("the right pane after the requests: focus %q, source %q; want the response", got,
			root.ui.Logs.Source())
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := focusedPane(ansi.Strip(root.View().Content)); got != paneRequests {
		t.Errorf("esc on the response: focus %q, want the requests", got)
	}
}

// TestSendTrial sends a request from the agent, and says what came back,
// what Keycard did, and the command the agent ran.
func TestSendTrial(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(press("2"))
	_, send := root.Update(press("s"))
	if send == nil || !strings.Contains(ansi.Strip(root.View().Content), "Waiting for the response") {
		t.Fatal("s did not send")
	}
	view := respond(
		root, send,
		newRecord(time.Now(), scopeRequest, githubHost, "/user", verdictAllow, sampleAllowed, githubEndpoint),
		sandbox.AgentResponse{Status: http.StatusOK, Body: `{"login":"ada"}`, Took: 412 * time.Millisecond},
	)
	for _, want := range []string{
		"200 OK", "412ms", "The proxy got " + sampleUser + "'s GitHub credential",
		`"login": "ada"`, "In the agent's container", "curl -sS -m 20 -X GET",
		"-H 'Authorization: Bearer placeholder'", githubUserURL,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("response lacks %q:\n%s", want, view)
		}
	}
	activity, _ := root.ui.Logs.Log(state.SourceActivity).Snapshot()
	if last := activity[len(activity)-1]; !strings.Contains(last,
		"✓ "+sampleUser+"'s agent: GET https://api.github.com/user: 200 OK") {
		t.Errorf("activity ends with %q, want the request", last)
	}
}

// wideCaption is wide enough for a response's caption to show whole.
const wideCaption = 200

// TestAgentVerdicts says what Keycard did with a request it refused, and
// with one to a host off the allow list.
func TestAgentVerdicts(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(tea.WindowSizeMsg{Width: wideCaption, Height: 40})
	root.Update(press("2"))
	down := tea.KeyPressMsg{Code: tea.KeyDown}
	root.Update(down)
	for _, tc := range []struct {
		moves    int
		record   proxylog.Record
		url      string
		response sandbox.AgentResponse
		want     string
	}{
		{
			1, newRecord(time.Now(), scopeRequest, routerHost, "/api/v1/key", verdictDeny, reasonRefused, routerEndpoint),
			"https://openrouter.ai/api/v1/key",
			sandbox.AgentResponse{Status: http.StatusForbidden},
			"No credential: Keycard's policy does not allow it",
		},
		{
			2, newRecord(time.Now(), proxylog.ScopeDNS, sampleDenied, "", verdictDeny, sampleReason, ""),
			"https://example.com/",
			sandbox.AgentResponse{Failure: "curl: (6) Could not resolve host: example.com"},
			"The proxy refused it: not on the sandbox's allow list",
		},
	} {
		for range tc.moves {
			root.Update(down)
		}
		_, send := root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if view := respond(root, send, tc.record, tc.response); !strings.Contains(view, tc.want) {
			t.Errorf("%s: response lacks %q:\n%s", tc.url, tc.want, view)
		}
	}
}

// TestSendNeedsTheAgent refuses to send before the agent runs.
func TestSendNeedsTheAgent(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.state.Agents.Pods = root.state.Agents.Pods[:2]
	if _, cmd := root.Update(press("s")); cmd != nil {
		t.Error("s sent without an agent")
	}
	if !strings.Contains(root.ui.Toast.Text, "not running") {
		t.Errorf("notice = %q, want it to say the agent is not running", root.ui.Toast.Text)
	}
}

// TestTrafficTab sums up what Keycard did, and shows the feed with the
// DNS lookups on d.
func TestTrafficTab(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.ui.Logs.Log(state.SourceTraffic).Add(sampleRecords(time.Now())[2].Row())
	root.Update(press("3"))
	view := ansi.Strip(root.View().Content)
	for _, want := range []string{
		"WHAT KEYCARD DID", "Every request through the egress proxy", sampleUserPath,
		"show DNS",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("Traffic tab lacks %q:\n%s", want, view)
		}
	}
	root.Update(press("d"))
	if view = ansi.Strip(root.View().Content); !strings.Contains(view, "with DNS lookups") ||
		!strings.Contains(view, "hide DNS") {
		t.Errorf("after d, the Traffic tab does not show the lookups:\n%s", view)
	}
}

// TestTabSizes draws every tab, down and up, at a laptop size and the
// smallest size, inside the window.
func TestTabSizes(t *testing.T) {
	t.Parallel()
	for _, build := range []func(*testing.T) *model{testModel, upModel} {
		root := build(t)
		root.screen = screenDashboard
		for _, size := range []tea.WindowSizeMsg{{Width: 140, Height: 40}, {Width: 80, Height: 22}} {
			root.Update(size)
			for _, key := range []string{"1", "2", "3", "4"} {
				root.Update(press(key))
				lines := strings.Split(ansi.Strip(root.View().Content), "\n")
				if len(lines) != size.Height {
					t.Errorf("%dx%d tab %s: %d lines, want %d", size.Width, size.Height, key, len(lines), size.Height)
				}
				for _, line := range lines {
					if ansi.StringWidth(line) > size.Width {
						t.Errorf("%dx%d tab %s: line wider than the window: %q", size.Width, size.Height, key, line)
						break
					}
				}
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
	root.state.Demos.List = sampleDemos()
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
	root.state.Demos.List = sampleDemos()
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
	root.state.Setup.Problems = []error{sandbox.ErrSettings}
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

// TestHomeTroubles says on Home what keeps a sandbox that runs from
// working, with what to do: nothing when all is well, then the mirror that
// stopped, the proxy that stopped, and its log that cannot be read.
func TestHomeTroubles(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(tea.WindowSizeMsg{Width: 150, Height: 42})
	if view := ansi.Strip(root.View().Content); strings.Contains(view, "NEEDS A LOOK") {
		t.Errorf("all well, and Home says something needs a look:\n%s", view)
	}
	root.state.Reads.Status.Mirror.Running = false
	for i, row := range root.state.Agents.Pods {
		if row.Container.Name == state.ProxyContainer {
			ctr := &root.state.Agents.Pods[i].Container
			ctr.Ready, ctr.State, ctr.ExitCode = false, state.StateTerminated, 1
		}
	}
	root.state.Agents.Traffic.SetErr(errRefused)
	view := ansi.Strip(root.View().Content)
	for _, want := range []string{
		"NEEDS A LOOK", "The mirror is down, so Keycard cannot verify the agent.", "Press u to start it again.",
		"The egress proxy stopped: terminated (exit 1).", "Press l for its log, or R to restart the pod.",
		"Cannot read the proxy's log: kubectl get pods: connection refused.",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("Home lacks %q:\n%s", want, view)
		}
	}
}

// errRefused is a read of the pods that the cluster refused.
var errRefused = errors.New("kubectl get pods: connection refused")

// TestPodsError says why there is no pod when the pods cannot be read, on
// Home and on the Agents tab.
func TestPodsError(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(podsMsg{agents: root.state.Agents.List, err: errRefused})
	for _, tab := range []string{"1", "2"} {
		root.Update(press(tab))
		if view := ansi.Strip(root.View().Content); !strings.Contains(view, "connection refused") {
			t.Errorf("tab %s does not say why there is no pod:\n%s", tab, view)
		}
	}
}

// TestLogsListAfterFewerSteps keeps the list of every log's cursor on a
// log when the list grows shorter, as when the settings move to an
// existing cluster, which has fewer steps.
func TestLogsListAfterFewerSteps(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.screen = screenDashboard
	root.Update(press("l"))
	for range len(root.logs.Entries()) {
		root.Update(press("j"))
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	root.session.Local().Settings[sandbox.KeyKubeContext] = "keycard-test-none"
	root.state.ResetSteps()
	root.Update(press("l"))
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if source := root.ui.Logs.Source(); source != state.SourceActivity {
		t.Errorf("the last log of a shorter list shows %q, want the activity", source)
	}
}

// TestOldResponseStaysOut shows a response only for the request it answers.
// One that comes back after a newer request does not take its place.
func TestOldResponseStaysOut(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(press("2"))
	_, first := root.Update(press("s"))
	root.agents.Reset()
	_, second := root.Update(press("s"))
	record := newRecord(time.Now(), scopeRequest, githubHost, "/user", verdictAllow, sampleAllowed, githubEndpoint)
	view := respond(root, first, record, sandbox.AgentResponse{Status: http.StatusOK, Body: `{"login":"old"}`})
	if strings.Contains(view, `"login": "old"`) || !strings.Contains(view, "Waiting for the response") {
		t.Errorf("the first request's response shows for the second:\n%s", view)
	}
	view = respond(root, second, record, sandbox.AgentResponse{Status: http.StatusOK, Body: `{"login":"ada"}`})
	if !strings.Contains(view, `"login": "ada"`) {
		t.Errorf("the second request's response does not show:\n%s", view)
	}
}

// TestLogsShowTheStep opens the list of every log on the step that stopped
// the bring-up, which Home's hint points at.
func TestLogsShowTheStep(t *testing.T) {
	t.Parallel()
	root := testModel(t)
	root.screen = screenDashboard
	root.Update(stepDoneMsg{index: 1, err: sandbox.ErrDocker})
	root.Update(press("l"))
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if want := state.StepSource(root.state.Run.Steps[1].Step.ID); root.ui.Logs.Source() != want {
		t.Errorf("l then enter after a failed step shows %q, want %q", root.ui.Logs.Source(), want)
	}
}

// TestStoppedChecksAreNoTrouble keeps checks the operator stopped off what
// needs a look on Home: they did not fail.
func TestStoppedChecksAreNoTrouble(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	root.Update(tea.WindowSizeMsg{Width: 150, Height: 42})
	checks := slices.IndexFunc(root.state.Run.Steps, func(row *state.StepRow) bool {
		return row.Step.ID == sandbox.StepChecks
	})
	root.Update(stepDoneMsg{index: checks, err: context.Canceled})
	if view := ansi.Strip(root.View().Content); strings.Contains(view, "NEEDS A LOOK") ||
		strings.Contains(view, "context canceled") {
		t.Errorf("Home shows the stopped checks as a failure:\n%s", view)
	}
}

// TestHelpFits draws every key's help within the window, the smallest one
// too.
func TestHelpFits(t *testing.T) {
	t.Parallel()
	for _, size := range [][2]int{{component.MinWidth, component.MinHeight}, {140, 40}} {
		root := upModel(t)
		root.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		root.Update(press("?"))
		modal := ansi.Strip(root.helpModal())
		lines := strings.Split(modal, "\n")
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Errorf("at %dx%d, a line of the help is %d wide:\n%s", size[0], size[1], ansi.StringWidth(line), modal)
				break
			}
		}
		if len(lines) > size[1] || !strings.Contains(modal, "X remove the agent") {
			t.Errorf("at %dx%d, the help takes %d rows, or lacks X:\n%s", size[0], size[1], len(lines), modal)
		}
	}
}

// TestHomeFirewallFailed says on Home that the agent does not run when its
// pod's firewall did not start, as the Agents tab does, instead of saying
// the pod is starting.
func TestHomeFirewallFailed(t *testing.T) {
	t.Parallel()
	root := upModel(t)
	for i, row := range root.state.Agents.Pods {
		ctr := &root.state.Agents.Pods[i].Container
		switch row.Container.Name {
		case "egress-netsetup":
			ctr.State, ctr.ExitCode = state.StateTerminated, 1
		case state.AgentContainer:
			ctr.Ready, ctr.State = false, "waiting"
		}
	}
	root.syncAgents()
	view := ansi.Strip(root.View().Content)
	if !strings.Contains(view, "the firewall did not start") || strings.Contains(view, "pod is starting") {
		t.Errorf("Home does not say the firewall did not start:\n%s", view)
	}
}

// TestTabsAgreeOnThePhase says on Home and the Agents tab that the sandbox
// is being read, or where the bring-up stopped.
func TestTabsAgreeOnThePhase(t *testing.T) {
	t.Parallel()
	for want, setup := range map[string]func(root *model){
		"Reading the sandbox":             func(root *model) { root.state.Reads.Done = false },
		"The bring-up stopped at: Create": func(root *model) { root.Update(stepDoneMsg{index: 1, err: sandbox.ErrDocker}) },
	} {
		root := testModel(t)
		root.Update(tea.WindowSizeMsg{Width: 150, Height: 42})
		setup(root)
		for _, tab := range []string{"1", "2"} {
			root.Update(press(tab))
			if view := ansi.Strip(root.View().Content); !strings.Contains(view, want) {
				t.Errorf("tab %s lacks %q:\n%s", tab, want, view)
			}
		}
	}
}

// TestPollKeepsTheLogInFront leaves the log in front to the screen that
// shows it: a poll follows the Agents tab's agent only while that tab's
// pane shows the agent's log, and not under the log chosen from the list.
func TestPollKeepsTheLogInFront(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	root.openTab(tabAgent)
	root.stopStream()
	entries := root.logs.Entries()
	root.showLog(entries[len(entries)-1])
	root.Update(podsMsg{rows: root.state.Agents.Pods, agents: root.state.Agents.List, mark: root.state.Reads.Mark()})
	if source := root.ui.Logs.Source(); source != state.SourceActivity || root.stream != "" {
		t.Errorf("under the activity log, a poll showed %q and followed %q; want the activity log", source,
			root.stream)
	}
	root.closeOverlay()
	root.follow(root.agents.FocusPane(page.AgentPaneRequests))
	root.Update(podsMsg{rows: root.state.Agents.Pods, agents: root.state.Agents.List, mark: root.state.Reads.Mark()})
	if source := root.ui.Logs.Source(); source != state.SourceResponse {
		t.Errorf("with the response in the pane, a poll showed %q; want the response", source)
	}
}

// TestLogFollowedOnceItsPodRuns follows the container of the log chosen
// from the list once a read finds its pod, though it had none when chosen.
func TestLogFollowedOnceItsPodRuns(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	pods := root.state.Agents.Pods
	root.state.Agents.Pods = nil
	root.showLog(root.logs.Entries()[1])
	if root.logs.Shown.Container != state.ProxyContainer || root.stream != "" {
		t.Fatalf("shown %+v, stream %q; want the proxy's log, not followed yet", root.logs.Shown, root.stream)
	}
	root.Update(podsMsg{rows: pods, agents: root.state.Agents.List, mark: root.state.Reads.Mark()})
	if want := state.ContainerSource(samplePod, state.ProxyContainer); root.stream != want ||
		root.ui.Logs.Source() != want {
		t.Errorf("after the pod came: stream %q, showing %q; want %q", root.stream, root.ui.Logs.Source(), want)
	}
}

// TestFinishedLogFollowedAgain follows an agent's log again once its
// container runs again, after a log that was read once because the
// container had finished, as in a pod made anew under the same name.
func TestFinishedLogFollowedAgain(t *testing.T) {
	t.Parallel()
	root := quietModel(t)
	finished := slices.Clone(root.state.Agents.Pods)
	for i := range finished {
		if finished[i].Container.Name == state.AgentContainer {
			finished[i].Container.State, finished[i].Container.Ready = state.StateTerminated, false
		}
	}
	running := root.state.Agents.Pods
	root.Update(podsMsg{rows: finished, agents: root.state.Agents.List, mark: root.state.Reads.Mark()})
	root.openTab(tabAgent)
	if !root.streamFinished {
		t.Fatalf("stream %q is not read once, want it read once for a container that finished", root.stream)
	}
	root.Update(podsMsg{rows: running, agents: root.state.Agents.List, mark: root.state.Reads.Mark()})
	if root.streamFinished || root.stream != state.ContainerSource(samplePod, state.AgentContainer) {
		t.Errorf("stream %q, read once %v; want the agent's log followed again", root.stream, root.streamFinished)
	}
}

// TestPodStatesAgree says the same about the agent's pod on Home and on
// the Agents tab: a pod an action makes comes up, a read that failed
// cannot read the pods, and a pod the read did not find is gone.
func TestPodStatesAgree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		setup     func(root *model)
		home, tab string
	}{
		{"coming up", func(root *model) {
			root.state.Agents.Pods = nil
			root.state.Run.Job = state.Job{Kind: state.JobAction, Name: "Restarting", Agent: sampleUser}
		}, "is coming up", "coming up"},
		{
			"unread", func(root *model) { root.state.Agents.Pods, root.state.Agents.PodsErr = nil, errRefused },
			"Cannot read the sandbox's pods", "cannot read the pods",
		},
		{"missing", func(root *model) { root.state.Agents.Pods = nil }, "has no pod", "no pod. u brings it up"},
	} {
		root := quietModel(t)
		tc.setup(root)
		home := ansi.Strip(root.View().Content)
		root.openTab(tabAgent)
		agents := ansi.Strip(root.View().Content)
		if !strings.Contains(home, tc.home) || !strings.Contains(agents, tc.tab) {
			t.Errorf("%s: Home lacks %q, or the Agents tab lacks %q:\n%s\n%s", tc.name, tc.home, tc.tab, home, agents)
		}
	}
}
