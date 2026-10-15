package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	// changeQuestion is the question a rerun of Init starts with, and
	// changeDemo its answer for the demo section.
	changeQuestion = "What do you want to change?"
	changeDemo     = "scenario"
	// apiKeyQuestion is the agent scenario's secret question.
	apiKeyQuestion = "API key"
	// The questions for the service account.
	clientIDQuestion     = "Service account client ID"
	clientSecretQuestion = "Service account client secret"
	zoneQuestion         = "Which zone"
	usersQuestion        = "Who does the agent act as?"
	// repoQuestion is the agent scenario's text question.
	repoQuestion = "Repository"
	// openRouterAPI and openRouterName are the OpenRouter resource the
	// demos call.
	openRouterAPI  = "https://openrouter.ai/api/v1"
	openRouterName = "OpenRouter"
	// The secrets setup keeps, by name, and what the tests type for them.
	openRouterSecret = "openrouter-api-key" //nolint:gosec // G101: a secret's name, not a credential.
	ghpToken         = "ghp-token"
	apiSecret        = "api-secret"
	orKey            = "or-key"
)

// scriptedAsker answers a question with the next answer under the key the
// question starts with. With no answer left, it keeps the default, and Secret
// returns errStop. It records every question. Input asks again after a
// refused answer, as the terminal asker does, and records the refusal.
type scriptedAsker struct {
	t       *testing.T
	answers map[string][]string
	asked   []string
	refused []error
	// offered holds the values of the choices each question offered.
	offered map[string][]string
}

func (a *scriptedAsker) Input(_ context.Context, question, value string, check func(string) error) (string, error) {
	for {
		answer, ok := a.next(question)
		if ok {
			value = answer
		}
		err := check(value)
		if err == nil {
			return value, nil
		}
		if !ok {
			a.t.Fatalf("%q: the answer %q is refused: %v", question, value, err)
		}
		a.refused = append(a.refused, err)
	}
}

// errStop is the error Secret returns when it has no answer left, as when the
// operator presses Ctrl-C.
var errStop = errors.New("stopped")

func (a *scriptedAsker) Secret(_ context.Context, question string, check func(string) error) (string, error) {
	answer, ok := a.next(question)
	if !ok {
		return "", errStop
	}
	if check(answer) != nil {
		a.t.Fatalf("%q: the answer %q is refused", question, answer)
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
		"Keycard API URL":     {serverURL},
		"CA certificate file": {caFile},
		clientIDQuestion:      {"sa-1"},
		clientSecretQuestion:  secrets,
		usersQuestion:         {"\x00again", email},
	}
}

// fakeManagement serves a management API with one zone at zoneURL, and
// returns a PEM file that trusts it. The service account secret is
// goodSecret. The first user listing is empty and later ones list
// ada@example.com and grace@example.com. The zone has the providers and the active policy sets
// given.
func fakeManagement(t *testing.T, zoneURL, providers, activeSets string) (*httptest.Server, string) {
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
	zone := `{"id":"zone-a","name":"Acme","protocols":{"oauth2":{"issuer":"` + zoneURL + `/"}}}`
	mux.HandleFunc("GET /zones", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(page(zone)))
	})
	mux.HandleFunc("GET /zones/zone-a", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(zone))
	})
	mux.HandleFunc("GET /zones/zone-a/users", func(w http.ResponseWriter, _ *http.Request) {
		if userLists.Add(1) == 1 {
			_, _ = w.Write([]byte(page("")))
			return
		}
		_, _ = w.Write([]byte(page(`{"id":"user-1","email":"ada@example.com"},` +
			`{"id":"user-2","email":"` + otherEmail + `"}`)))
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
	return server, trustFile(t, server)
}

// initLocal returns a Local rooted in a new directory that holds the
// scenarios, as the repository does. Its store saves to a config file in
// another new directory, and keeps the secrets in a test keychain. Its
// tools run body.
func initLocal(t *testing.T, body string) (*sandbox.Local, *testKeychain) {
	t.Helper()
	local, _ := newLocal(t, sandbox.Settings{}, body)
	linkCharts(t, local.Root)
	keychain := newTestKeychain()
	local.Store = sandbox.NewStore(filepath.Join(t.TempDir(), "keycard", "sandbox.yaml"), nil, keychain)
	return local, keychain
}

// testKeychain keeps secrets in memory, as the OS keychain does between
// runs.
type testKeychain struct {
	mu      sync.Mutex
	secrets map[string]string
	// refuse is a secret Set refuses to save, and getRefuse one Get refuses
	// to read, as a locked keychain does.
	refuse, getRefuse string
	// broken is what Check returns.
	broken error
}

// errRefused is the error testKeychain gives for the secret it refuses.
var errRefused = errors.New("the keychain is locked")

func newTestKeychain() *testKeychain { return &testKeychain{secrets: map[string]string{}} }

func (k *testKeychain) Get(name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if name == k.getRefuse {
		return "", fmt.Errorf("%w: %w", sandbox.ErrKeychain, errRefused)
	}
	secret, ok := k.secrets[name]
	if !ok {
		return "", sandbox.ErrNoSecret
	}
	return secret, nil
}

func (k *testKeychain) Set(name, secret string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if name == k.refuse {
		return fmt.Errorf("%w: %w", sandbox.ErrKeychain, errRefused)
	}
	k.secrets[name] = secret
	return nil
}

func (k *testKeychain) Delete(name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.secrets[name]; !ok {
		return sandbox.ErrNoSecret
	}
	delete(k.secrets, name)
	return nil
}

func (*testKeychain) Where() string { return "the test keychain" }

func (k *testKeychain) Check() error { return k.broken }

// The names of the account's secret and the GitHub token in the keychain.
const managementName, githubTokenName = "management-client-secret", "github-token"

// zoneID is the zone the fake management API serves.
const zoneID = "zone-a"

// otherSecret is a secret in the keychain that setup does not write.
const otherSecret = "other"

// TestForget deletes the saved settings and every secret setup can save,
// whichever demo the settings name, and leaves a secret setup does not
// write. The session keeps its settings. A demo that does not load does not
// stop it, and Forget says that demo's secrets can be left.
func TestForget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, broken, wantErr string
	}{
		{name: "every demo loads"},
		{name: "a demo does not load", broken: "title: [", wantErr: "reading the demos to find their secrets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := firstRun(t)
			if tc.broken != "" {
				addScenario(t, run.local.Root, "broken", tc.broken, false)
			}
			for _, name := range []string{githubTokenName, "slack-bot-token", "linear-api-key", otherSecret} {
				if err := run.keychain.Set(name, "secret"); err != nil {
					t.Fatal(err)
				}
			}
			deleted, err := run.local.Forget()
			if tc.wantErr == "" {
				checkErr(t, err, nil, "")
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Forget error = %v, want %q", err, tc.wantErr)
			}
			checkForgot(t, run, deleted)
		})
	}
}

// checkForgot requires Forget to have deleted every secret but other, and
// the settings file, and the session to keep its settings.
func checkForgot(t *testing.T, run initRun, deleted int) {
	t.Helper()
	left := slices.Collect(maps.Keys(run.keychain.secrets))
	if !slices.Equal(left, []string{otherSecret}) || deleted != 5 {
		t.Errorf("Forget deleted %d, keychain = %v; want 5 deleted and only other left", deleted, left)
	}
	if _, err := os.Stat(run.local.Store.Path()); !os.IsNotExist(err) {
		t.Errorf("stat %s: %v, want the settings file gone", run.local.Store.Path(), err)
	}
	if run.local.Store.Saved() || !run.local.Initialized() {
		t.Errorf("saved %v, initialized %v; want the session's settings kept and nothing saved",
			run.local.Store.Saved(), run.local.Initialized())
	}
}

// checkKeychain requires local's keychain to hold each secret of want.
func checkKeychain(t *testing.T, keychain *testKeychain, want map[string]string) {
	t.Helper()
	for name, secret := range want {
		if got, err := keychain.Get(name); err != nil || got != secret {
			t.Errorf("keychain %s = %q, %v; want %q", name, got, err, secret)
		}
	}
}

// savedConfig returns the config file local's store saved, and requires it
// to be readable by its owner alone.
func savedConfig(t *testing.T, local *sandbox.Local) string {
	t.Helper()
	path := local.Store.Path()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("%s mode = %v, want no access for group or others", path, info.Mode().Perm())
	}
	return readFile(t, path)
}

// reload loads local's settings from its saved config file, as the next
// command does.
func reload(t *testing.T, local *sandbox.Local, keychain *testKeychain) {
	t.Helper()
	config, err := sandbox.LoadConfig(local.Store.Path())
	if err != nil || config == nil {
		t.Fatalf("LoadConfig = %v, %v; want the saved settings", config, err)
	}
	local.Store = sandbox.NewStore(local.Store.Path(), config, keychain)
	local.UseConfig()
}

// TestInit answers every question on a first run, saves the settings, and
// checks the config file and the two secrets in the keychain. Init asks
// again for a rejected secret, and lists the users again while the zone has
// none.
func TestInit(t *testing.T) {
	t.Parallel()
	run := firstRun(t)
	gitHub := run.ask.offered["How does the agent get into GitHub?"]
	if want := []string{"github", "\x00token"}; !slices.Equal(gitHub, want) {
		t.Errorf("GitHub choices = %q, want %q", gitHub, want)
	}
	if run.config != wantFirstConfig(run) {
		t.Errorf("config = %q, want %q", run.config, wantFirstConfig(run))
	}
	checkKeychain(t, run.keychain, map[string]string{
		managementName: goodSecret, openRouterSecret: orKey,
	})
	checkSettings(t, run.local, sandbox.Settings{
		sandbox.KeyCluster: "keycard-sandbox-local", sandbox.KeyIssuer: "", sandbox.KeyTunnel: "",
		sandbox.KeyZoneURL: "https://localhost", sandbox.KeyZoneOnHost: "true", sandbox.KeyUserEmail: email,
		sandbox.KeyGitHubLogin: "", sandbox.KeyCAFile: run.caFile,
		sandbox.KeyKubeContext: "", sandbox.KeyImageTag: "", sandbox.KeyClusterCIDRs: "",
		sandbox.KeyScenario: "smoke",
	})
	checkContains(t, "output", run.out.String(), []string{
		"Saved the settings in " + run.local.Store.Path() + ", and the secrets in the test keychain.\n",
	})
	want := "  go run ./cmd/sandbox --config " + sandbox.HomeRelative(
		run.local.Store.Path(),
	) + " up --replace-active-policy-set\n"
	if !strings.HasSuffix(run.out.String(), want) {
		t.Errorf("output = %q, want it to end with %q", run.out, want)
	}
}

// wantFirstConfig is the config file the first run saves.
func wantFirstConfig(run initRun) string {
	return "# Settings of the Keycard sandbox, saved by its setup. Secrets are not\n" +
		"# in this file: the OS keychain keeps them, under the service \"keycard\".\n" +
		"account:\n" +
		"    api_url: " + run.server.URL + "\n" +
		"    client_id: sa-1\n" +
		"    secret: management-client-secret\n" +
		"    ca_file: " + run.caFile + "\n" +
		"zone:\n    id: zone-a\n    url: https://localhost\n    on_host: true\n" +
		"agents:\n    default: ada@example.com\n" +
		"demo:\n    name: smoke\n    credentials:\n" +
		"        - identifier: https://api.github.com\n          name: GitHub\n          provider: github\n" +
		"        - identifier: https://github.com\n          name: GitHub git\n          provider: github\n" +
		"        - identifier: https://openrouter.ai/api/v1\n          name: OpenRouter\n" +
		"          secret: openrouter-api-key\n" +
		"cluster:\n    name: keycard-sandbox-local\n"
}

// TestInitWithoutSaving keeps the settings for the session when the
// operator does not save them: nothing goes in the config file or the
// keychain.
func TestInitWithoutSaving(t *testing.T) {
	t.Parallel()
	server, caFile := fakeManagement(t, "https://192.0.2.10", noGitHubProviders, "")
	local, keychain := initLocal(t, "")
	out := &bytes.Buffer{}
	local.Out = out
	answers := signInAnswers(server.URL, caFile, goodSecret)
	answers["GitHub personal access token"] = []string{ghpToken}
	answers[sandbox.QuestionSave] = []string{"n"}
	local.Ask = &scriptedAsker{t: t, answers: answers}
	result, err := local.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Saved || local.Store.Saved() || !local.Initialized() {
		t.Errorf("saved %v %v, initialized %v; want the settings for the session alone", result.Saved,
			local.Store.Saved(), local.Initialized())
	}
	if _, err = os.Stat(local.Store.Path()); !os.IsNotExist(err) {
		t.Errorf("stat %s: %v, want no config file", local.Store.Path(), err)
	}
	if secrets := keychain.secrets; len(secrets) != 0 {
		t.Errorf("keychain = %v, want nothing saved", slices.Collect(maps.Keys(secrets)))
	}
	checkContains(t, "output", out.String(), []string{"Kept the settings for this session. Nothing was saved.\n"})
	local.UseConfig()
	if problems := local.NewSession("127.0.0.1:8480", time.Second).Problems(); len(problems) != 0 {
		t.Errorf("problems = %v, want none: the session has the token", problems)
	}
}

// TestInitWithoutAFile runs Init with --config -: it does not offer to save
// the settings, and writes nothing.
func TestInitWithoutAFile(t *testing.T) {
	t.Parallel()
	server, caFile := fakeManagement(t, "https://192.0.2.10", noGitHubProviders, "")
	local, _ := newLocal(t, sandbox.Settings{}, "")
	linkCharts(t, local.Root)
	local.Store = sandbox.NewStore(sandbox.NoConfig, nil, nil)
	local.Out = &bytes.Buffer{}
	answers := signInAnswers(server.URL, caFile, goodSecret)
	answers["GitHub personal access token"] = []string{ghpToken}
	ask := &scriptedAsker{t: t, answers: answers}
	local.Ask = ask
	result, err := local.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkNotAsked(t, ask, []string{sandbox.QuestionSave})
	if result.Saved || local.Store.Saved() || !local.Initialized() {
		t.Errorf("saved %v %v, initialized %v; want the settings for the session alone", result.Saved,
			local.Store.Saved(), local.Initialized())
	}
}

// openRouterKeyQuestion is the question for the OpenRouter API key.
const openRouterKeyQuestion = "OpenRouter API key"

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

// initRun is a first Init run and what it saved.
type initRun struct {
	local    *sandbox.Local
	keychain *testKeychain
	server   *httptest.Server
	caFile   string
	ask      *scriptedAsker
	out      *bytes.Buffer
	// config is the config file the run saved.
	config string
}

// firstRun runs Init on a new checkout, answering every question, and
// loads the settings it saved, as the next command does.
func firstRun(t *testing.T) initRun {
	t.Helper()
	server, caFile := fakeManagement(t, "https://localhost", gitHubProviders,
		`{"id":"s1","name":"default-zone-policies","active":true,"target_type":"zone"}`)
	local, keychain := initLocal(t, "")
	addScenario(t, local.Root, agentName, agentScenario, true)
	out := &bytes.Buffer{}
	local.Out = out
	answers := signInAnswers(server.URL, caFile, "bad", goodSecret)
	maps.Copy(answers, map[string][]string{
		zoneQuestion:                         {zoneID},
		"How does the agent get into GitHub": {"github"},
		"Add an OpenRouter API key?":         {"y"},
		openRouterKeyQuestion:                {orKey},
		"How does Keycard reach the cluster": {"quick"},
		"Turn on the sandbox's access rules": {"y"},
	})
	ask := &scriptedAsker{t: t, answers: answers}
	local.Ask = ask
	result, err := local.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !result.ReplaceActive || !result.Saved {
		t.Errorf("result = %+v, want the policy set replaced and the settings saved", result)
	}
	reload(t, local, keychain)
	return initRun{
		local: local, keychain: keychain, server: server, caFile: caFile, ask: ask, out: out, config: savedConfig(t, local),
	}
}

// checkSettings requires the settings local saved to be want, and returns
// them.
func checkSettings(t *testing.T, local *sandbox.Local, want sandbox.Settings) sandbox.Settings {
	t.Helper()
	config, err := sandbox.LoadConfig(local.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	settings := sandbox.SettingsFrom(config, func(string) string { return "" })
	if !maps.Equal(settings, want) {
		t.Errorf("settings = %q, want %q", settings, want)
	}
	return settings
}

// TestInitGitHubToken runs init against a zone without a GitHub provider.
// The personal access token is the default, and both GitHub resources read
// it from one secret.
func TestInitGitHubToken(t *testing.T) {
	t.Parallel()
	server, caFile := fakeManagement(t, "https://192.0.2.10", noGitHubProviders, "")
	local, keychain := initLocal(t, "")
	out := &bytes.Buffer{}
	local.Out = out
	answers := signInAnswers(server.URL, caFile, goodSecret)
	answers["GitHub personal access token"] = []string{ghpToken}
	ask := &scriptedAsker{t: t, answers: answers}
	local.Ask = ask
	if _, err := local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "The zone has no GitHub provider.") {
		t.Errorf("output = %q, want it to say the zone has no GitHub provider", out)
	}
	// A token is the only way without a provider, so Init does not ask.
	checkNotAsked(t, ask, []string{"How does the agent get into GitHub?"})
	checkKeychain(t, keychain, map[string]string{githubTokenName: ghpToken})
	checkContains(t, "config", savedConfig(t, local), []string{
		"        - identifier: https://api.github.com\n          name: GitHub\n          secret: github-token\n",
		"        - identifier: https://github.com\n          name: GitHub git\n          secret: github-token\n",
	})
}

// accountEnv returns a Getenv that holds KEYCARD_CLIENT_ID and
// KEYCARD_CLIENT_SECRET.
func accountEnv(clientID, secret string) func(string) string {
	env := map[string]string{sandbox.EnvClientID: clientID, sandbox.EnvClientSecret: secret}
	return func(key string) string { return env[key] }
}

// TestInitAccountFromEnv signs in with KEYCARD_CLIENT_ID and
// KEYCARD_CLIENT_SECRET. Init does not ask for the account or save the
// secret.
func TestInitAccountFromEnv(t *testing.T) {
	t.Parallel()
	server, caFile := fakeManagement(t, "https://192.0.2.10", noGitHubProviders, "")
	local, keychain := initLocal(t, "")
	local.Out = &bytes.Buffer{}
	local.Getenv = accountEnv("sa-env", goodSecret)
	answers := signInAnswers(server.URL, caFile)
	answers["GitHub personal access token"] = []string{ghpToken}
	ask := &scriptedAsker{t: t, answers: answers}
	local.Ask = ask
	if _, err := local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkNotAsked(t, ask, []string{clientIDQuestion, clientSecretQuestion})
	checkContains(t, "config", savedConfig(t, local), []string{"    client_id: sa-env\n    ca_file: "})
	if _, err := keychain.Get(managementName); err == nil {
		t.Error("the keychain holds the client secret, want it left in the environment")
	}
}

// TestInitAccountFromEnvRejected stops when Keycard rejects the account in
// the environment, since there is nothing to ask again.
func TestInitAccountFromEnvRejected(t *testing.T) {
	t.Parallel()
	server, caFile := fakeManagement(t, "https://192.0.2.10", noGitHubProviders, "")
	local, _ := initLocal(t, "")
	local.Out = &bytes.Buffer{}
	local.Getenv = accountEnv("sa-env", "bad")
	ask := &scriptedAsker{t: t, answers: signInAnswers(server.URL, caFile)}
	local.Ask = ask
	_, err := local.Init(t.Context())
	checkErr(t, err, []error{zonesetup.ErrAuth}, "signing in with "+sandbox.EnvClientID+" and "+sandbox.EnvClientSecret)
}

// TestInitRerunWithAccountEnv reruns Init with the account in the
// environment and changes only the cluster. It signs in with the
// environment's account, and keeps the saved one in the settings.
func TestInitRerunWithAccountEnv(t *testing.T) {
	t.Parallel()
	run := firstRun(t)
	run.local.Getenv = accountEnv("sa-env", goodSecret)
	ask := &scriptedAsker{t: t, answers: map[string][]string{changeQuestion: {sandbox.SectionCluster}}}
	run.local.Ask = ask
	if _, err := run.local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkConfigKept(t, run)
}

// TestInitSaveFails returns ErrNotSaved when the keychain refuses a secret.
// It puts back the secrets it saved before that one, and writes no settings
// file, so the keychain and the file still agree. The store keeps the
// settings for the session.
func TestInitSaveFails(t *testing.T) {
	t.Parallel()
	server, caFile := fakeManagement(t, "https://192.0.2.10", noGitHubProviders, "")
	local, keychain := initLocal(t, "")
	local.Out = &bytes.Buffer{}
	keychain.refuse = managementName
	answers := signInAnswers(server.URL, caFile, goodSecret)
	answers["GitHub personal access token"] = []string{ghpToken}
	local.Ask = &scriptedAsker{t: t, answers: answers}
	_, err := local.Init(t.Context())
	checkErr(t, err, []error{sandbox.ErrNotSaved, sandbox.ErrKeychain, errRefused}, "")
	if local.Store.Config() == nil || local.Store.Saved() {
		t.Errorf("config = %v, saved = %t; want the settings for this session only",
			local.Store.Config(), local.Store.Saved())
	}
	if len(keychain.secrets) != 0 {
		t.Errorf(
			"keychain = %v, want the secrets saved before the refusal put back",
			slices.Collect(maps.Keys(keychain.secrets)),
		)
	}
	if _, err = os.Stat(local.Store.Path()); !os.IsNotExist(err) {
		t.Errorf("stat %s: %v, want no config file", local.Store.Path(), err)
	}
}

// TestInitKeychainBroken does not offer to save when the keychain does not
// work, and says why. The settings and the secrets last for the session.
func TestInitKeychainBroken(t *testing.T) {
	t.Parallel()
	server, caFile := fakeManagement(t, "https://192.0.2.10", noGitHubProviders, "")
	local, keychain := initLocal(t, "")
	out := &bytes.Buffer{}
	local.Out = out
	keychain.broken = errRefused
	answers := signInAnswers(server.URL, caFile, goodSecret)
	answers["GitHub personal access token"] = []string{ghpToken}
	asker := &scriptedAsker{t: t, answers: answers}
	local.Ask = asker
	result, err := local.Init(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, question := range asker.asked {
		if strings.HasPrefix(question, sandbox.QuestionSave) {
			t.Errorf("asked %q, want no offer to save", question)
		}
	}
	checkContains(t, "output", out.String(), []string{
		"Setup cannot save the settings, because the keychain is locked.\n",
		"Kept the settings for this session. Nothing was saved.\n",
	})
	if result.Saved || local.Store.Config() == nil || len(keychain.secrets) != 0 {
		t.Errorf("saved = %t, config = %v, keychain = %v; want the settings for this session only",
			result.Saved, local.Store.Config(), slices.Collect(maps.Keys(keychain.secrets)))
	}
	if _, err = os.Stat(local.Store.Path()); !os.IsNotExist(err) {
		t.Errorf("stat %s: %v, want no config file", local.Store.Path(), err)
	}
}

// rewriteConfig replaces old with replacement in the config file run saved,
// and loads it again.
func rewriteConfig(t *testing.T, run initRun, old, replacement string) {
	t.Helper()
	path := run.local.Store.Path()
	raw := readFile(t, path)
	if !strings.Contains(raw, old) {
		t.Fatalf("the config lacks %q\n%s", old, raw)
	}
	writeFile(t, path, strings.Replace(raw, old, replacement, 1))
	reload(t, run.local, run.keychain)
}

// TestInitRerunNewZone changes only the zone. The demo's credentials can
// name the old zone's providers, so Init asks the demo's questions again.
func TestInitRerunNewZone(t *testing.T) {
	t.Parallel()
	run := firstRun(t)
	rewriteConfig(t, run, "id: zone-a", "id: zone-old")
	run.out.Reset()
	ask := &scriptedAsker{t: t, answers: map[string][]string{
		changeQuestion: {sandbox.SectionZone}, zoneQuestion: {zoneID},
	}}
	run.local.Ask = ask
	if _, err := run.local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ask.asked, sandbox.QuestionDemo) {
		t.Errorf("asked %q, want the demo question too", ask.asked)
	}
	if !strings.Contains(run.out.String(), "The zone changed.") {
		t.Errorf("output = %q, want it to say the zone changed", run.out)
	}
}

// TestInitRerunKeepsSecretEnv keeps a credential that comes from an
// environment variable, and a credential of a resource that no demo lists.
// When Init asks the demo again, it offers to keep reading the variable.
func TestInitRerunKeepsSecretEnv(t *testing.T) {
	t.Parallel()
	const handWritten = "        - identifier: https://api.example.org\n          name: Mine\n          env: MY_TOKEN\n"
	for _, tc := range []struct {
		name, section string
		wantAsked     []string
	}{
		{name: "the cluster changes", section: sandbox.SectionCluster},
		{
			name: "the demo changes", section: changeDemo,
			wantAsked: []string{"Keep reading the OpenRouter API key from OPENROUTER_API_KEY?"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := firstRun(t)
			rewriteConfig(t, run, "          secret: openrouter-api-key\n",
				"          env: OPENROUTER_API_KEY\n"+handWritten)
			ask := &scriptedAsker{t: t, answers: map[string][]string{changeQuestion: {tc.section}}}
			run.local.Ask = ask
			if _, err := run.local.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, question := range tc.wantAsked {
				if !slices.Contains(ask.asked, question) {
					t.Errorf("asked %q, want %q among them", ask.asked, question)
				}
			}
			checkContains(
				t,
				"config",
				savedConfig(t, run.local),
				[]string{"          env: OPENROUTER_API_KEY\n" + handWritten},
			)
		})
	}
}

// TestInitRerunRefusedSecret asks for the client secret when Keycard refuses
// the saved one, and does not offer the saved one again.
func TestInitRerunRefusedSecret(t *testing.T) {
	t.Parallel()
	run := firstRun(t)
	if err := run.keychain.Set(managementName, "bad"); err != nil {
		t.Fatal(err)
	}
	const keepQuestion = "Keep the saved client secret?"
	ask := &scriptedAsker{t: t, answers: map[string][]string{
		changeQuestion:       {sandbox.SectionAccount},
		keepQuestion:         {"y", "n"},
		clientSecretQuestion: {goodSecret},
	}}
	run.local.Ask = ask
	if _, err := run.local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	if kept := slices.DeleteFunc(
		slices.Clone(ask.asked),
		func(q string) bool { return q != keepQuestion },
	); len(
		kept,
	) != 1 {
		t.Errorf("asked %q, want %q once", ask.asked, keepQuestion)
	}
	checkKeychain(t, run.keychain, map[string]string{managementName: goodSecret})
}

// sectionAsker records the parts of the settings Init says it asks about.
type sectionAsker struct {
	*scriptedAsker

	sections []string
}

func (a *sectionAsker) Section(id string) { a.sections = append(a.sections, id) }

// TestInitSections asks only the section named, without asking what to
// change, and tells a SectionAsker each part it asks about. Saved settings
// stay saved without the question.
func TestInitSections(t *testing.T) {
	t.Parallel()
	run := firstRun(t)
	ask := &sectionAsker{scriptedAsker: &scriptedAsker{t: t}}
	run.local.Ask = ask
	if _, err := run.local.InitSections(t.Context(), sandbox.SectionScenario); err != nil {
		t.Fatal(err)
	}
	checkNotAsked(t, ask.scriptedAsker, []string{changeQuestion, sandbox.QuestionSave})
	want := []string{sandbox.SectionScenario, sandbox.SectionCredentials, sandbox.SectionPolicy}
	if !slices.Equal(ask.sections, want) {
		t.Errorf("sections = %q, want %q", ask.sections, want)
	}
	run.local.Ask = nil
	_, err := run.local.InitSections(t.Context(), sandbox.SectionScenario)
	checkErr(t, err, []error{sandbox.ErrNoTerminal}, "")
}

// TestInitWithoutATerminal refuses to ask without a terminal, and names the
// command to run in one.
func TestInitWithoutATerminal(t *testing.T) {
	t.Parallel()
	local, _ := initLocal(t, "")
	_, err := local.Init(t.Context())
	checkErr(t, err, []error{sandbox.ErrNoTerminal}, "Run go run ./cmd/sandbox --config ")
}

// TestInitRerun runs Init a second time. It shows the current settings and
// asks only the sections chosen. Sections it keeps come from the files, and
// it signs in with the saved account only when a chosen section needs
// Keycard. Saved secrets stay unless the answer is no. Choosing k3d again
// keeps the saved image tag. When a row sets prepare, the test runs it on the
// first run's files before the second run.
func TestInitRerun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		answers     map[string][]string
		notAsked    []string
		wantSecrets map[string]string
		check       func(t *testing.T, run initRun)
	}{
		{name: "nothing", notAsked: signInQuestions(), check: checkKept},
		{
			name:        "everything, keeping the secrets",
			answers:     map[string][]string{changeQuestion: {"\x00everything"}},
			notAsked:    []string{clientSecretQuestion, openRouterKeyQuestion, sandbox.QuestionSave},
			wantSecrets: map[string]string{managementName: goodSecret, openRouterSecret: orKey},
			check:       checkConfigKept,
		},
		{
			name:     "demo, keeping its answers",
			answers:  map[string][]string{changeQuestion: {changeDemo}},
			notAsked: append(signInQuestions(), "GitHub personal access token", openRouterKeyQuestion),
			check:    checkConfigKept,
		},
		{
			name: "another demo",
			answers: map[string][]string{
				changeQuestion:       {changeDemo},
				sandbox.QuestionDemo: {agentName},
				repoQuestion:         {agentRepo},
				apiKeyQuestion:       {apiSecret},
			},
			notAsked:    signInQuestions(),
			wantSecrets: map[string]string{"api-key": apiSecret},
			check:       checkAgentConfig,
		},
		{
			name: "demo credentials",
			answers: map[string][]string{
				changeQuestion:                       {changeDemo},
				"How does the agent get into GitHub": {"\x00token"},
				"GitHub personal access token":       {"ghp-new"},
				"Keep the saved OpenRouter API key?": {"n"},
				openRouterKeyQuestion:                {"or-new"},
			},
			notAsked:    signInQuestions(),
			wantSecrets: map[string]string{githubTokenName: "ghp-new", openRouterSecret: "or-new"},
			check:       checkTokenConfig,
		},
		{
			name: "cluster",
			answers: map[string][]string{
				changeQuestion:                       {"cluster"},
				sandbox.QuestionClusterName:          {"my-sandbox"},
				"How does Keycard reach the cluster": {"named"},
				"Name of your Cloudflare tunnel":     {"demo"},
				"Your tunnel's URL":                  {"https://issuer.example.org"},
			},
			notAsked: append(signInQuestions(), "Turn on the sandbox's access rules"),
			check:    checkNamedTunnel,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := firstRun(t)
			rewriteConfig(t, run, "    name: keycard-sandbox-local\n",
				"    name: keycard-sandbox-local\n    image_tag: "+devTag+"\n")
			run.config = savedConfig(t, run.local)
			run.out.Reset()
			ask := &scriptedAsker{t: t, answers: tc.answers}
			run.local.Ask = ask
			if _, err := run.local.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			checkNotAsked(t, ask, tc.notAsked)
			checkKeychain(t, run.keychain, tc.wantSecrets)
			tc.check(t, run)
		})
	}
}

// signInQuestions returns the questions that sign in and choose the zone.
func signInQuestions() []string {
	return []string{"Keycard API URL", clientIDQuestion, zoneQuestion}
}

// checkKept requires the summary of the first run's settings, and the
// config as the first run saved it.
func checkKept(t *testing.T, run initRun) {
	t.Helper()
	checkContains(t, "output", run.out.String(), []string{
		"  Keycard account          " + strings.TrimPrefix(run.server.URL, "https://") +
			", with the service account sa-1\n",
		"  Demo and credentials     Check the proxy with GitHub and OpenRouter (smoke). GitHub: each person " +
			"signs in (provider github), GitHub git: each person signs in (provider github), OpenRouter: your key\n",
		"  Cluster                  The local cluster keycard-sandbox-local, reached through a Cloudflare quick tunnel\n",
		"Kept your settings as they are.\n",
	})
	if strings.Contains(run.out.String(), "Saved") {
		t.Errorf("output = %q, want nothing saved", run.out)
	}
	checkConfigKept(t, run)
}

// checkConfigKept requires the config as the first run saved it.
func checkConfigKept(t *testing.T, run initRun) {
	t.Helper()
	if got := savedConfig(t, run.local); got != run.config {
		t.Errorf("config = %q, want it as the first run saved it, %q", got, run.config)
	}
}

// checkTokenConfig requires the config to keep the zone and the user, and
// the GitHub credentials to come from the saved token.
func checkTokenConfig(t *testing.T, run initRun) {
	t.Helper()
	checkContains(t, "config", savedConfig(t, run.local), []string{
		"    id: zone-a\n", "    default: ada@example.com\n", "          secret: github-token\n",
	})
}

// checkAgentConfig requires the config to keep the zone and the user, and
// to hold the agent scenario with its answers and its credential alone.
func checkAgentConfig(t *testing.T, run initRun) {
	t.Helper()
	config := savedConfig(t, run.local)
	checkContains(t, "config", config, []string{"    id: zone-a\n", "    default: ada@example.com\n", wantAgentDemo})
	if strings.Contains(config, "github") {
		t.Errorf("config = %s\nwant only the agent scenario's credential", config)
	}
}

// wantAgentDemo is the demo in the config after the agent scenario's
// questions.
const wantAgentDemo = "demo:\n    name: agent\n" +
	"    answers:\n        branch: main\n        repo: octo/demo\n" +
	"    credentials:\n" +
	"        - identifier: https://api.example.com\n          name: Example\n          secret: api-key\n"

// checkNamedTunnel requires the config to name the cluster and the tunnel,
// and keep the user and the demo.
func checkNamedTunnel(t *testing.T, run initRun) {
	t.Helper()
	config, err := sandbox.LoadConfig(run.local.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	settings := sandbox.SettingsFrom(config, func(string) string { return "" })
	if settings[sandbox.KeyTunnel] != "demo" || settings[sandbox.KeyIssuer] != "https://issuer.example.org" ||
		settings[sandbox.KeyCluster] != "my-sandbox" || settings[sandbox.KeyUserEmail] != email ||
		settings[sandbox.KeyScenario] != sandbox.DefaultScenario || settings[sandbox.KeyImageTag] != devTag {
		t.Errorf("settings = %q, want the named cluster and tunnel, and the kept user, demo and image tag", settings)
	}
}

// TestInitScenario chooses a scenario with a text input and a secret
// input on a first run. A rerun that changes the demo and accepts every
// default keeps the saved answer and secret, and does not ask for the secret.
func TestInitScenario(t *testing.T) {
	t.Parallel()
	server, caFile := fakeManagement(t, "https://192.0.2.10", gitHubProviders, "")
	local, keychain := initLocal(t, "")
	addScenario(t, local.Root, agentName, agentScenario, true)
	out := &bytes.Buffer{}
	local.Out = out
	answers := signInAnswers(server.URL, caFile, goodSecret)
	maps.Copy(answers, map[string][]string{
		sandbox.QuestionDemo: {agentName},
		repoQuestion:         {agentRepo},
		apiKeyQuestion:       {apiSecret},
	})
	ask := &scriptedAsker{t: t, answers: answers}
	local.Ask = ask
	if _, err := local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	if demos := ask.offered[sandbox.QuestionDemo]; !slices.Contains(demos, agentName) ||
		!slices.Contains(demos, "smoke") {
		t.Errorf("demos offered = %q, want agent and smoke among them", demos)
	}
	checkKeychain(t, keychain, map[string]string{"api-key": apiSecret})
	checkContains(t, "config", savedConfig(t, local), []string{wantAgentDemo})
	first := savedConfig(t, local)

	reload(t, local, keychain)
	again := &scriptedAsker{t: t, answers: map[string][]string{changeQuestion: {changeDemo}}}
	local.Ask = again
	if _, err := local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkNotAsked(t, again, []string{apiKeyQuestion})
	if !slices.Contains(again.asked, "Keep the saved API key?") {
		t.Errorf("second run asked %q, want it to offer to keep the API key", again.asked)
	}
	if got := savedConfig(t, local); got != first {
		t.Errorf("config = %q, want it as the first run saved it, %q", got, first)
	}
}
