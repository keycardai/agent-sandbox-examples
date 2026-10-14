package sandbox_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// scriptedAsker answers a question with the next answer under the key the
// question starts with. With no answer left, it keeps the default. It
// records every question.
type scriptedAsker struct {
	t       *testing.T
	answers map[string][]string
	asked   []string
	// offered holds the values of the choices each question offered.
	offered map[string][]string
}

func (a *scriptedAsker) Input(_ context.Context, question, value string, check func(string) error) (string, error) {
	if answer, ok := a.next(question); ok {
		value = answer
	}
	if err := check(value); err != nil {
		a.t.Fatalf("%q: the answer %q is refused: %v", question, value, err)
	}
	return value, nil
}

func (a *scriptedAsker) Secret(_ context.Context, question string, check func(string) error) (string, error) {
	answer, ok := a.next(question)
	if !ok || check(answer) != nil {
		a.t.Fatalf("%q: no usable answer", question)
	}
	return answer, nil
}

func (a *scriptedAsker) Choose(
	_ context.Context,
	question string,
	options []sandbox.Option,
	value string,
) (string, error) {
	if a.offered == nil {
		a.offered = map[string][]string{}
	}
	for _, option := range options {
		a.offered[question] = append(a.offered[question], option.Value)
	}
	if answer, ok := a.next(question); ok {
		value = answer
	}
	if !slices.ContainsFunc(options, func(o sandbox.Option) bool { return o.Value == value }) {
		a.t.Fatalf("%q: %q is not one of %v", question, value, options)
	}
	return value, nil
}

func (a *scriptedAsker) Confirm(_ context.Context, question string, value bool) (bool, error) {
	if answer, ok := a.next(question); ok {
		return answer == "y", nil
	}
	return value, nil
}

func (a *scriptedAsker) next(question string) (string, bool) {
	a.asked = append(a.asked, question)
	for prefix, answers := range a.answers {
		if strings.HasPrefix(question, prefix) && len(answers) > 0 {
			a.answers[prefix] = answers[1:]
			return answers[0], true
		}
	}
	return "", false
}

// goodSecret is the service account secret the fake management API takes.
const goodSecret = "good"

// The providers a zone can have. Only github signs users in to GitHub.
const (
	keycardIDProvider = `{"id":"p0","identifier":"keycard-id","name":"Keycard ID",` +
		`"protocols":{"oauth2":{"issuer":"https://id.keycard.ai"}}}`
	clusterProvider = `{"id":"p2","identifier":"https://issuer.example.com","name":"Kubernetes cluster",` +
		`"protocols":{"oauth2":{"issuer":"https://issuer.example.com"}}}`
	gitHubProviders = keycardIDProvider + `,{"id":"p1","identifier":"github","name":"GitHub","protocols":{"oauth2":{` +
		`"issuer":"https://github.com","authorization_endpoint":"https://github.com/login/oauth/authorize"}}},` +
		clusterProvider
	noGitHubProviders = keycardIDProvider + "," + clusterProvider
)

// signInAnswers returns the answers that sign in to the fake management
// API with each secret in turn, and choose its user after one more listing.
func signInAnswers(serverURL, caFile string, secrets ...string) map[string][]string {
	return map[string][]string{
		"Keycard management API URL":             {serverURL},
		"PEM file of extra trusted roots":        {caFile},
		"Organization service account client ID": {"sa-1"},
		"Service account client secret":          secrets,
		"Which user":                             {"\x00again", email},
	}
}

// fakeManagement is a management API with one zone at https://localhost.
// The service account's secret is goodSecret. The zone has no user until
// the second listing, and has the providers and the active policy sets
// given.
func fakeManagement(t *testing.T, providers, activeSets string) (*httptest.Server, string) {
	t.Helper()
	var userLists atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /service-account-token", func(w http.ResponseWriter, r *http.Request) {
		if _, secret, _ := r.BasicAuth(); secret != goodSecret {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"t","token_type":"Bearer"}`))
	})
	page := func(items string) string { return `{"items":[` + items + `],"pagination":{}}` }
	mux.HandleFunc("GET /zones", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(page(`{"id":"zone-a","name":"Acme","protocols":{"oauth2":{"issuer":"https://localhost/"}}}`)))
	})
	mux.HandleFunc("GET /zones/zone-a/users", func(w http.ResponseWriter, _ *http.Request) {
		if userLists.Add(1) == 1 {
			_, _ = w.Write([]byte(page("")))
			return
		}
		_, _ = w.Write([]byte(page(`{"id":"user-1","email":"ada@example.com"}`)))
	})
	mux.HandleFunc("GET /zones/zone-a/providers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(page(providers)))
	})
	mux.HandleFunc("GET /zones/zone-a/policy-sets", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(page(activeSets)))
	})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	caFile := filepath.Join(t.TempDir(), "roots.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return server, caFile
}

// initLocal returns a Local rooted in a new directory that holds a copy of
// local/local.env.example.
func initLocal(t *testing.T) *sandbox.Local {
	t.Helper()
	example, err := os.ReadFile("../../local/local.env.example")
	if err != nil {
		t.Fatal(err)
	}
	local, _ := newLocal(t, sandbox.Settings{}, "")
	writeFile(t, filepath.Join(local.Root, "local", "local.env.example"), string(example))
	return local
}

// TestInit answers every question on a first run and checks the two files
// and two secrets Init writes. Init asks again for a rejected secret, and
// lists the users again while the zone has none.
func TestInit(t *testing.T) {
	t.Parallel()
	run := firstRun(t)
	gitHub := run.ask.offered[gitHubSourceQuestion+"?"]
	if want := []string{"github", gitHubTokenChoice}; !slices.Equal(gitHub, want) {
		t.Errorf("GitHub choices = %q, want %q", gitHub, want)
	}
	secretDir := filepath.Join(run.local.Root, "local", "out", "secrets")
	wantValues := "# Written by `go run ./cmd/sandbox init`. zone-setup.example.yaml documents\n" +
		"# each key. `sandbox run` sets issuer_url to the cluster's issuer.\n" +
		"management_api_url: " + run.server.URL + "\n" +
		"zone_url: https://localhost\n" +
		"zone_id: zone-a\n" +
		"management_service_account:\n" +
		"    client_id: sa-1\n" +
		"    client_secret_file: " + filepath.Join(secretDir, "management-client-secret") + "\n" +
		"users:\n" +
		"    - email: ada@example.com\n" +
		"resources:\n" +
		"    - identifier: https://api.github.com\n      name: GitHub\n      prefix: true\n" +
		"      credential_provider: github\n" +
		"    - identifier: https://github.com\n      name: GitHub git\n      prefix: true\n" +
		"      credential_provider: github\n" +
		"    - identifier: https://openrouter.ai/api/v1\n      name: OpenRouter\n      prefix: true\n" +
		"      secret_file: " + filepath.Join(secretDir, "openrouter-api-key") + "\n"
	checkFile(t, filepath.Join(run.local.Root, "zone-setup.yaml"), wantValues)
	checkFile(t, filepath.Join(secretDir, "management-client-secret"), "good\n")
	checkFile(t, filepath.Join(secretDir, "openrouter-api-key"), "or-key\n")
	checkSettings(t, run.local.Root, sandbox.Settings{
		sandbox.KeyCluster: "keycard-sandbox-local", sandbox.KeyIssuer: "", sandbox.KeyTunnel: "",
		sandbox.KeyZoneURL: "https://localhost", sandbox.KeyZoneOnHost: "true",
		sandbox.KeyZoneSetupValues: "zone-setup.yaml", sandbox.KeyUserEmail: email,
		sandbox.KeyGitHubLogin: "", sandbox.KeyCAFile: run.caFile,
	})
	for _, line := range []string{
		"Wrote local/out/secrets/management-client-secret, readable only by you.\n",
		"Wrote local/out/secrets/openrouter-api-key, readable only by you.\n",
		"Wrote zone-setup.yaml.\n",
		"Wrote local/local.env.\n",
	} {
		if !strings.Contains(run.out.String(), line) {
			t.Errorf("output lacks %q", line)
		}
	}
	if want := "  go run ./cmd/sandbox up --replace-active-policy-set\n"; !strings.HasSuffix(run.out.String(), want) {
		t.Errorf("output = %q, want it to end with %q", run.out, want)
	}
}

// The questions and answers that more than one test uses.
const (
	openRouterKeyQuestion = "OpenRouter API key"
	// gitHubSourceQuestion asks where the GitHub token comes from.
	// gitHubTokenChoice is the answer for a personal access token.
	gitHubSourceQuestion = "How does the sandbox get a GitHub token"
	gitHubTokenChoice    = "\x00token"
	// changeQuestion asks which section of the settings to change.
	// credentialsSection is the answer for the GitHub token and the
	// OpenRouter key.
	changeQuestion     = "What do you want to change?"
	credentialsSection = "credentials"
)

// checkNotAsked fails the test for each question asked that starts with one
// of notAsked.
func checkNotAsked(t *testing.T, ask *scriptedAsker, notAsked []string) {
	t.Helper()
	for _, question := range ask.asked {
		for _, not := range notAsked {
			if strings.HasPrefix(question, not) {
				t.Errorf("asked %q", question)
			}
		}
	}
}

// initRun is a first Init run and what it wrote.
type initRun struct {
	local  *sandbox.Local
	server *httptest.Server
	caFile string
	ask    *scriptedAsker
	out    *bytes.Buffer
	// values is the values file the run wrote.
	values string
}

// firstRun runs Init on a new checkout, answering every question, and
// loads the settings it wrote.
func firstRun(t *testing.T) initRun {
	t.Helper()
	server, caFile := fakeManagement(t, gitHubProviders,
		`{"id":"s1","name":"default-zone-policies","active":true,"target_type":"zone"}`)
	local := initLocal(t)
	out := &bytes.Buffer{}
	local.Out = out
	answers := signInAnswers(server.URL, caFile, "bad", goodSecret)
	maps.Copy(answers, map[string][]string{
		"Which zone":                                   {"zone-a"},
		gitHubSourceQuestion:                           {"github"},
		"Add an OpenRouter API key?":                   {"y"},
		openRouterKeyQuestion:                          {"or-key"},
		"How does the zone reach the cluster's issuer": {"quick"},
		"The zone's active policy set":                 {"y"},
	})
	ask := &scriptedAsker{t: t, answers: answers}
	local.Ask = ask
	result, err := local.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !result.ReplaceActive {
		t.Error("ReplaceActive = false, want true")
	}
	if local.Settings, err = sandbox.LoadSettings(local.Root, "", func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	values, err := os.ReadFile(filepath.Join(local.Root, "zone-setup.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return initRun{local: local, server: server, caFile: caFile, ask: ask, out: out, values: string(values)}
}

// checkSettings requires local/local.env under root to hold want, and
// returns what it holds.
func checkSettings(t *testing.T, root string, want sandbox.Settings) sandbox.Settings {
	t.Helper()
	settings, err := sandbox.LoadSettings(root, "", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(settings, want) {
		t.Errorf("local.env = %q, want %q", settings, want)
	}
	return settings
}

// TestInitGitHubToken runs init against a zone without a GitHub provider.
// The personal access token is the default, and both GitHub resources read
// it from one secret file.
func TestInitGitHubToken(t *testing.T) {
	t.Parallel()
	server, caFile := fakeManagement(t, noGitHubProviders, "")
	local := initLocal(t)
	out := &bytes.Buffer{}
	local.Out = out
	answers := signInAnswers(server.URL, caFile, goodSecret)
	answers["GitHub personal access token"] = []string{"ghp-token"}
	ask := &scriptedAsker{t: t, answers: answers}
	local.Ask = ask
	if _, err := local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "The zone has no GitHub provider.") {
		t.Errorf("output = %q, want it to say the zone has no GitHub provider", out)
	}
	token := filepath.Join(local.Root, "local", "out", "secrets", "github-token")
	checkFile(t, token, "ghp-token\n")
	raw, err := os.ReadFile(filepath.Join(local.Root, "zone-setup.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"- identifier: https://api.github.com\n      name: GitHub\n      prefix: true\n      secret_file: " + token,
		"- identifier: https://github.com\n      name: GitHub git\n      prefix: true\n      secret_file: " + token,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("zone-setup.yaml lacks %q\n%s", want, raw)
		}
	}
}

// TestInitRerun runs Init a second time. It shows the current settings and
// asks only the sections chosen. Sections it keeps come from the files, and
// it signs in with the saved account only when a chosen section needs
// Keycard. Saved secrets stay unless the answer is no. When a row sets
// prepare, the test runs it on the first run's files before the second run.
func TestInitRerun(t *testing.T) {
	t.Parallel()
	const changeCluster = "cluster"
	tests := []struct {
		name      string
		prepare   func(t *testing.T, run initRun)
		answers   map[string][]string
		notAsked  []string
		wantFiles map[string]string // secret file name: content
		check     func(t *testing.T, run initRun)
	}{
		{name: "nothing", notAsked: signInQuestions(), check: checkKept},
		{
			name:      "everything, keeping the secrets",
			answers:   map[string][]string{changeQuestion: {"\x00everything"}},
			notAsked:  []string{"Service account client secret", openRouterKeyQuestion},
			wantFiles: map[string]string{"management-client-secret": "good\n", "openrouter-api-key": "or-key\n"},
			check:     checkValuesKept,
		},
		{
			name: "credentials",
			answers: map[string][]string{
				changeQuestion:                       {credentialsSection},
				gitHubSourceQuestion:                 {gitHubTokenChoice},
				"GitHub personal access token":       {"ghp-new"},
				"Keep the saved OpenRouter API key?": {"n"},
				openRouterKeyQuestion:                {"or-new"},
			},
			notAsked:  signInQuestions(),
			wantFiles: map[string]string{"github-token": "ghp-new\n", "openrouter-api-key": "or-new\n"},
			check:     checkTokenValues,
		},
		{
			name: "cluster",
			answers: map[string][]string{
				changeQuestion: {changeCluster},
				"How does the zone reach the cluster's issuer": {"named"},
				"Tunnel name":     {"demo"},
				"Tunnel hostname": {"https://issuer.example.org"},
			},
			notAsked: append(signInQuestions(), "The zone's active policy set"),
			check:    checkNamedTunnel,
		},
		{
			name:     "cluster, in a hand-written values file",
			prepare:  handWritten,
			answers:  map[string][]string{changeQuestion: {changeCluster}},
			notAsked: signInQuestions(),
			check:    checkHandWrittenKept,
		},
		{
			name:     "credentials, in a hand-written values file",
			prepare:  handWritten,
			answers:  map[string][]string{changeQuestion: {credentialsSection}},
			notAsked: append(signInQuestions(), "Service account client secret", openRouterKeyQuestion),
			check:    checkHandWrittenKept,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := firstRun(t)
			if tc.prepare != nil {
				tc.prepare(t, run)
			}
			run.out.Reset()
			ask := &scriptedAsker{t: t, answers: tc.answers}
			run.local.Ask = ask
			if _, err := run.local.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			checkNotAsked(t, ask, tc.notAsked)
			for name, want := range tc.wantFiles {
				checkFile(t, filepath.Join(run.local.Root, "local", "out", "secrets", name), want)
			}
			tc.check(t, run)
		})
	}
}

// handWrittenValues is a values file with the keys Init does not ask about:
// client_secret_env, issuer_url, namespace, policy_set, secret_env and a
// resource of its own. It is in the form yaml.Marshal writes.
const handWrittenValues = `zone_url: https://localhost
zone_id: zone-a
management_service_account:
    client_id: sa-1
    client_secret_env: SANDBOX_SECRET
issuer_url: https://issuer.example.com
namespace: team-a
policy_set: Team A
users:
    - email: ada@example.com
resources:
    - identifier: https://api.github.com
      name: GitHub
      prefix: true
      credential_provider: github
    - identifier: https://github.com
      name: GitHub git
      prefix: true
      credential_provider: github
    - identifier: https://openrouter.ai/api/v1
      name: OpenRouter
      prefix: true
      secret_env: OPENROUTER_API_KEY
    - identifier: https://api.example.com
      name: Example
      prefix: false
      secret_file: /run/secrets/example
`

// handWritten replaces the values file with handWrittenValues, and puts the
// service account secret in SANDBOX_SECRET.
func handWritten(t *testing.T, run initRun) {
	t.Helper()
	writeFile(t, filepath.Join(run.local.Root, "zone-setup.yaml"),
		"management_api_url: "+run.server.URL+"\n"+handWrittenValues)
	run.local.Getenv = func(key string) string {
		if key == "SANDBOX_SECRET" {
			return goodSecret
		}
		return ""
	}
}

// checkHandWrittenKept requires the values file to keep every key of
// handWrittenValues.
func checkHandWrittenKept(t *testing.T, run initRun) {
	t.Helper()
	checkFile(t, filepath.Join(run.local.Root, "zone-setup.yaml"),
		"# Written by `go run ./cmd/sandbox init`. zone-setup.example.yaml documents\n"+
			"# each key. `sandbox run` sets issuer_url to the cluster's issuer.\n"+
			"management_api_url: "+run.server.URL+"\n"+handWrittenValues)
}

// signInQuestions returns the questions that sign in and choose the zone.
func signInQuestions() []string {
	return []string{"Keycard management API URL", "Organization service account client ID", "Which zone"}
}

// checkKept requires the summary of the first run's settings, and the
// values file as the first run wrote it.
func checkKept(t *testing.T, run initRun) {
	t.Helper()
	for _, want := range []string{
		"  Keycard account          sa-1 at " + run.server.URL + "\n",
		"  GitHub and OpenRouter    GitHub token from the provider github, OpenRouter API key\n",
		"  Cluster and issuer       a local k3d cluster, keycard-sandbox-local, with a quick tunnel\n",
		"Kept local/local.env and zone-setup.yaml as they are.\n",
	} {
		if !strings.Contains(run.out.String(), want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(run.out.String(), "Wrote") {
		t.Errorf("output = %q, want nothing written", run.out)
	}
	checkValuesKept(t, run)
}

// checkValuesKept requires the values file as the first run wrote it.
func checkValuesKept(t *testing.T, run initRun) {
	t.Helper()
	checkFile(t, filepath.Join(run.local.Root, "zone-setup.yaml"), run.values)
}

// checkTokenValues requires the values file to keep the zone and the user,
// and to read the GitHub token from its secret file.
func checkTokenValues(t *testing.T, run initRun) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(run.local.Root, "zone-setup.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(run.local.Root, "local", "out", "secrets", "github-token")
	for _, want := range []string{"zone_id: zone-a\n", "- email: ada@example.com\n", "secret_file: " + token} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("zone-setup.yaml lacks %q\n%s", want, raw)
		}
	}
}

// TestInitRelativeKey reruns Init from the repository root, where a saved
// key's relative path resolves. Init must not offer to keep that key. It asks
// again and writes the key's absolute path.
//
//nolint:paralleltest // t.Chdir changes the process's directory, so the test cannot run in parallel.
func TestInitRelativeKey(t *testing.T) {
	run := firstRun(t)
	key := filepath.Join("local", "out", "secrets", "openrouter-api-key")
	absKey := filepath.Join(run.local.Root, key)
	values := filepath.Join(run.local.Root, "zone-setup.yaml")
	raw, err := os.ReadFile(values) //nolint:gosec // G304: the path is the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, values, strings.ReplaceAll(string(raw), absKey, key))
	t.Chdir(run.local.Root)
	ask := &scriptedAsker{t: t, answers: map[string][]string{
		changeQuestion:                 {credentialsSection},
		gitHubSourceQuestion:           {gitHubTokenChoice},
		"GitHub personal access token": {"ghp-new"},
		openRouterKeyQuestion:          {"or-new"},
	}}
	run.local.Ask = ask
	if _, err = run.local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkNotAsked(t, ask, []string{"Keep the saved OpenRouter API key?"})
	checkFile(t, absKey, "or-new\n")
	if raw, err = os.ReadFile(values); err != nil { //nolint:gosec // G304: the path is the test's own file.
		t.Fatal(err)
	}
	if want := "secret_file: " + absKey + "\n"; !strings.Contains(string(raw), want) {
		t.Errorf("zone-setup.yaml lacks %q\n%s", want, raw)
	}
}

// checkNamedTunnel requires the values file as the first run wrote it, and
// local.env to name the tunnel and keep the user.
func checkNamedTunnel(t *testing.T, run initRun) {
	t.Helper()
	checkValuesKept(t, run)
	settings, err := sandbox.LoadSettings(run.local.Root, "", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if settings[sandbox.KeyTunnel] != "demo" || settings[sandbox.KeyIssuer] != "https://issuer.example.org" ||
		settings[sandbox.KeyUserEmail] != email {
		t.Errorf("local.env = %q, want the named tunnel and the kept user", settings)
	}
}

// checkFile requires path to hold want and to be readable only by its
// owner.
func checkFile(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Errorf("%s = %q, want %q", path, raw, want)
	}
	info, err := os.Stat(path)
	if err == nil && info.Mode().Perm()&0o077 != 0 {
		t.Errorf("%s mode = %v, want no access for group or others", path, info.Mode().Perm())
	}
}
