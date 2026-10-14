package sandbox_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

const (
	// agentName is the name of agentScenario in the tests, and agentRepo
	// the answer to its text input.
	agentName = "agent"
	agentRepo = "octo/demo"
)

// agentScenario is a scenario with two text inputs that its agent's
// environment uses, one with a default, and a secret.
const agentScenario = `title: Agent
inputs:
  - {id: repo, kind: text, prompt: Repository}
  - {id: api-key, kind: secret, prompt: API key}
  - {id: branch, kind: text, prompt: Branch, default: main}
resources:
  - {identifier: https://api.example.com, name: Example, credential: api-key}
endpoints:
  - url: https://api.example.com
    credential:
      placement: {kind: header, name: Authorization, template: "Bearer {credential}"}
agent:
  args: [sleep, infinity]
  env: {REPO: "https://github.com/{repo}.git", BRANCH: "{branch}", PLACEHOLDER: "{not a ref}"}
run: {mode: checks}
`

// addScenario writes scenarios/<name>/scenario.yaml under root, and a
// Dockerfile when dockerfile is set.
func addScenario(t *testing.T, root, name, body string, dockerfile bool) {
	t.Helper()
	dir := filepath.Join(root, "scenarios", name)
	writeFile(t, filepath.Join(dir, "scenario.yaml"), body)
	if dockerfile {
		writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM ghcr.io/keycardai/sandbox-base:local\n")
	}
}

// TestLoadScenario checks that LoadScenario refuses each scenario it cannot
// use, including one whose agent.env names a secret input.
func TestLoadScenario(t *testing.T) {
	t.Parallel()
	const head = "title: T\ninputs:\n  - {id: key, kind: secret, prompt: Key}\n"
	tests := []struct {
		name, scenario, body, wantText string
	}{
		{name: "bad name", scenario: "../smoke", wantText: `"../smoke" is not a scenario name`},
		{name: "missing", scenario: "nothing", wantText: "scenario.yaml does not exist"},
		{name: "unknown key", body: head + "run: {mode: checks}\nimage: x\n", wantText: "field image not found"},
		{name: "no title", body: "run: {mode: checks}\n", wantText: "title is required"},
		{
			name:     "input listed twice",
			body:     head + "  - {id: key, kind: text, prompt: Key}\nrun: {mode: checks}\n",
			wantText: "inputs[1].id key is listed twice",
		},
		{
			name:     "unknown kind",
			body:     "title: T\ninputs: [{id: key, kind: password, prompt: Key}]\nrun: {mode: checks}\n",
			wantText: "inputs[0].kind must be text, secret or github",
		},
		{
			name:     "confirm on text",
			body:     "title: T\ninputs: [{id: key, kind: text, prompt: Key, confirm: \"Add?\"}]\nrun: {mode: checks}\n",
			wantText: "inputs[0].confirm is only for a secret",
		},
		{
			name:     "resource without a credential input",
			body:     head + "resources: [{identifier: https://a.example.com, credential: other}]\nrun: {mode: checks}\n",
			wantText: "resources[0].credential must be the id of a secret or github input",
		},
		{
			name:     "secret in the environment",
			body:     head + "agent: {env: {KEY: \"{key}\"}}\nrun: {mode: checks}\n",
			wantText: "agent.env.KEY: {key} must name a text input",
		},
		{
			name:     "bad environment name",
			body:     head + "agent: {env: {\"A-B\": x}}\nrun: {mode: checks}\n",
			wantText: `agent.env: "A-B" is not an environment variable name`,
		},
		{
			name:     "unknown mode",
			body:     head + "run: {mode: daemon}\n",
			wantText: `run.mode "daemon" is not one of checks, shell, background`,
		},
		{
			name:     "background without args",
			body:     head + "run: {mode: background}\n",
			wantText: "agent.args is required in background mode",
		},
		{
			name:     "shell without a command",
			body:     head + "run: {mode: shell}\n",
			wantText: "run.command is required in shell mode and not allowed in other modes",
		},
		{
			name:     "command in checks mode",
			body:     head + "run: {mode: checks, command: [sh]}\n",
			wantText: "run.command is required in shell mode and not allowed in other modes",
		},
		{
			name:     "default on a secret",
			body:     "title: T\ninputs: [{id: key, kind: secret, prompt: Key, default: x}]\nrun: {mode: checks}\n",
			wantText: "inputs[0].default is only for a text input",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			scenario := tc.scenario
			if scenario == "" {
				scenario = "bad"
				addScenario(t, root, scenario, tc.body, false)
			}
			_, err := sandbox.LoadScenario(root, scenario)
			checkErr(t, err, []error{sandbox.ErrScenario}, tc.wantText)
		})
	}
}

// TestScenarioValues checks the values file of a scenario with a
// Dockerfile: its endpoints, the image `sandbox run` builds, and the agent's
// environment with the text answer in place of {repo} and the default in
// place of {branch}. Values refuses a missing answer that has no default.
func TestScenarioValues(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	addScenario(t, root, agentName, agentScenario, true)
	scenario, err := sandbox.LoadScenario(root, agentName)
	if err != nil {
		t.Fatal(err)
	}
	got, err := scenario.Values(map[string]string{"repo": agentRepo})
	if err != nil {
		t.Fatal(err)
	}
	const want = `agent:
  args:
  - sleep
  - infinity
  env:
    BRANCH: main
    PLACEHOLDER: '{not a ref}'
    REPO: https://github.com/octo/demo.git
endpoints:
- credential:
    placement:
      kind: header
      name: Authorization
      template: Bearer {credential}
  url: https://api.example.com
images:
  agent: ghcr.io/keycardai/sandbox-agent:local
`
	if string(got) != want {
		t.Errorf("values = %q, want %q", got, want)
	}
	_, err = scenario.Values(map[string]string{})
	checkErr(
		t,
		err,
		[]error{sandbox.ErrScenario},
		"local/answers.yaml has no answer for {repo}. Run go run ./cmd/sandbox init",
	)
}

// TestPiSession checks that pi-session gives pi a models.json that sends
// OpenRouter the placeholder as a bearer token, which the proxy replaces,
// and leaves a models.json of the operator's own as it is.
func TestPiSession(t *testing.T) {
	t.Parallel()
	const own = "{}\n"
	for _, tc := range []struct {
		name, existing, want string
	}{
		{name: "first run", want: `"x-api-key": "", "Authorization": "Bearer placeholder"`},
		{name: "operator's own", existing: own, want: own},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, bin := t.TempDir(), t.TempDir()
			// The clone exists, so pi-session does not run git.
			if err := os.MkdirAll(filepath.Join(home, "work", "demo", ".git"), 0o750); err != nil {
				t.Fatal(err)
			}
			//nolint:gosec // G306: the fake pi must be executable.
			if err := os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			models := filepath.Join(home, ".pi", "agent", "models.json")
			if tc.existing != "" {
				writeFile(t, models, tc.existing)
			}
			script, err := filepath.Abs(filepath.Join("..", "..", "scenarios", "pi", "pi-session"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), script) //nolint:gosec // G204: the script is this repository's.
			cmd.Env = []string{
				"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin", "SANDBOX_REPO=" + agentRepo, "SANDBOX_MODEL=model",
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("pi-session: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(models) //nolint:gosec // G304: the path is under the test's own directory.
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), tc.want) {
				t.Errorf("models.json = %q, want it to hold %q", raw, tc.want)
			}
		})
	}
}
