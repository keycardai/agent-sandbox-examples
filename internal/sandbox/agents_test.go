package sandbox_test

import (
	"bytes"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

const (
	// otherEmail is the zone's second user.
	otherEmail = "grace@example.com"
	// repoID and chatID are the team scenario's inputs, and adaChat and
	// graceChat each person's answer to the second.
	repoID, chatID     = "repo", "chat"
	adaChat, graceChat = "C-ada", "C-grace"
	// teamName is a scenario with an input each person answers.
	teamName     = "team"
	teamScenario = `title: Team agent
inputs:
  - {id: repo, kind: text, prompt: Repository}
  - {id: chat, kind: text, per_user: true, prompt: "Chat ID of {user}"}
agent:
  args: [sleep, infinity]
  env: {REPO: "{repo}", CHAT: "{chat}"}
run: {mode: checks}
`
)

// TestInitAgents gives the person setup picks an agent in every demo, and
// another person an agent in the team demo alone, as the settings file can.
// Setup asks the per-user input for each person in the demo it asks about,
// by email, and a rerun that changes nothing asks only the answers the
// settings lack. Another demo has only the picked person's agent, and the
// team demo keeps its people and their answers for when it comes back. Back
// in the first demo, setup offers to keep its saved secret.
func TestInitAgents(t *testing.T) {
	t.Parallel()
	server, caFile := fakeManagement(t, "https://192.0.2.10", noGitHubProviders, "")
	local, keychain := initLocal(t, "")
	addScenario(t, local.Root, teamName, teamScenario, false)
	local.Out = &bytes.Buffer{}
	answers := signInAnswers(server.URL, caFile, goodSecret)
	maps.Copy(answers, map[string][]string{
		sandbox.QuestionDemo:  {teamName},
		repoQuestion:          {agentRepo},
		"Chat ID of " + email: {adaChat},
	})
	local.Ask = &scriptedAsker{t: t, answers: answers}
	if _, err := local.Init(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkUsers(t, local, teamName)

	path := local.Store.Path()
	writeFile(t, path, strings.Replace(readFile(t, path), "    default: "+email+"\n",
		"    default: "+email+"\n    demos:\n        team:\n            - "+otherEmail+"\n", 1))
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: a file written by hand.
		t.Fatal(err)
	}
	reload(t, local, keychain)
	rerun := func(change string, more map[string][]string) *scriptedAsker {
		t.Helper()
		ask := &scriptedAsker{t: t, answers: map[string][]string{}}
		if change != "" {
			ask.answers[changeQuestion] = []string{change}
		}
		maps.Copy(ask.answers, more)
		local.Ask = ask
		if _, err := local.Init(t.Context()); err != nil {
			t.Fatal(err)
		}
		return ask
	}
	rerun("", map[string][]string{"Chat ID of " + otherEmail: {graceChat}})
	savedConfig(t, local)
	both := map[string]map[string]string{email: {chatID: adaChat}, otherEmail: {chatID: graceChat}}
	checkAnswers(t, local, both)
	checkUsers(t, local, teamName, otherEmail)

	ask := rerun(changeDemo, map[string][]string{
		sandbox.QuestionDemo: {sandbox.DefaultScenario}, "GitHub personal access token": {ghpToken},
	})
	checkNotAsked(t, ask, []string{"Chat ID"})
	checkAnswers(t, local, both)
	checkUsers(t, local, sandbox.DefaultScenario)
	checkUsers(t, local, teamName, otherEmail)

	rerun(changeDemo, map[string][]string{sandbox.QuestionDemo: {teamName}})
	ask = rerun(changeDemo, map[string][]string{sandbox.QuestionDemo: {sandbox.DefaultScenario}})
	if !slices.ContainsFunc(ask.asked, func(q string) bool { return strings.HasPrefix(q, "Keep the saved GitHub") }) {
		t.Errorf("back in %s, setup asked %q; want it to offer the saved GitHub token", sandbox.DefaultScenario, ask.asked)
	}
}

// editConfig changes the config run saved, and loads it again.
func editConfig(t *testing.T, run initRun, change func(config *sandbox.Config)) {
	t.Helper()
	config, err := sandbox.LoadConfig(run.local.Store.Path())
	if err != nil || config == nil {
		t.Fatalf("LoadConfig = %v, %v; want the saved settings", config, err)
	}
	change(config)
	raw, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, run.local.Store.Path(), string(raw))
	reload(t, run.local, run.keychain)
}

// checkAnswers requires the saved settings to hold the answer repo:
// octo/demo for everyone, and want for each person.
func checkAnswers(t *testing.T, local *sandbox.Local, want map[string]map[string]string) {
	t.Helper()
	config, err := sandbox.LoadConfig(local.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(config.Demo.Answers, map[string]string{repoID: agentRepo}) {
		t.Errorf("answers = %q, want repo: octo/demo alone", config.Demo.Answers)
	}
	if !maps.EqualFunc(config.Demo.People, want, maps.Equal) {
		t.Errorf("each person's answers = %q, want %q", config.Demo.People, want)
	}
}

// withAnswers gives local a config, kept for the session, that holds the
// demo's answers.
func withAnswers(local *sandbox.Local, answers map[string]string) {
	local.Store = sandbox.NewStore(sandbox.NoConfig, &sandbox.Config{Demo: sandbox.ConfigDemo{Answers: answers}}, nil)
}

// readFile returns the content of path.
func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// checkUsers requires the saved settings to give email an agent in every
// demo, and others an agent in demo, in order. It then uses the settings
// for the session, as the next command does.
func checkUsers(t *testing.T, local *sandbox.Local, demo string, others ...string) {
	t.Helper()
	config, err := sandbox.LoadConfig(local.Store.Path())
	if err != nil || config == nil {
		t.Fatalf("LoadConfig = %v, %v; want the saved settings", config, err)
	}
	local.UseConfig()
	agents := config.Agents
	if agents.Default != email || !slices.Equal(agents.Demos[demo], others) {
		t.Errorf("agents = %q, and %q in %s; want %q, and %q", agents.Default, agents.Demos[demo], demo, email, others)
	}
	if local.Settings[sandbox.KeyUserEmail] != email {
		t.Errorf("%s = %q, want %q", sandbox.KeyUserEmail, local.Settings[sandbox.KeyUserEmail], email)
	}
}
