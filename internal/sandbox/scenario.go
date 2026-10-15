package sandbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	yaml "go.yaml.in/yaml/v3"
	k8syaml "sigs.k8s.io/yaml"
)

const (
	// DefaultScenario is the scenario `sandbox run` uses when
	// KEYCARD_LOCAL_SCENARIO is empty.
	DefaultScenario = "smoke"
	scenariosDir    = "scenarios"
	// answersFile holds the answers to the scenario's questions that are
	// not secrets, relative to the repository root.
	answersFile = "local/answers.yaml"
	// The kinds of scenario input.
	inputText   = "text"
	inputSecret = "secret"
	inputGitHub = "github"
	// ModeChecks runs the checks and keeps the sandbox up.
	ModeChecks = "checks"
	// ModeShell runs the checks, then runs run.command in the agent
	// container, attached to the terminal.
	ModeShell = "shell"
	// ModeBackground runs the agent as the container's args. `sandbox up`
	// runs the checks, then follows the agent's log.
	ModeBackground = "background"
)

// ErrScenario means a scenario is missing, or `sandbox` cannot use it as
// written.
var ErrScenario = errors.New("scenario refused")

var (
	scenarioName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	inputID      = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	envName      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// answerRef is a reference to a text input in an agent.env value.
	answerRef = regexp.MustCompile(`\{([a-z][a-z0-9-]*)\}`)
)

// Scenario is one demo in scenarios/<name>: what `sandbox init` asks, the
// Keycard resources zone-setup creates, the proxy's endpoints, and the
// agent. docs/local.md documents the keys of scenario.yaml.
type Scenario struct {
	// Name is the scenario's directory under scenarios.
	Name      string             `yaml:"-"`
	Title     string             `yaml:"title"`
	Inputs    []ScenarioInput    `yaml:"inputs"`
	Resources []ScenarioResource `yaml:"resources"`
	// Endpoints go to the sandbox chart's endpoints as written.
	Endpoints []any         `yaml:"endpoints"`
	Agent     ScenarioAgent `yaml:"agent"`
	Run       ScenarioRun   `yaml:"run"`
	// Image is the agent image `sandbox run` builds from the scenario's
	// Dockerfile, or "" when the scenario has none and runs sandbox-base.
	Image string `yaml:"-"`
}

// ScenarioInput is one question `sandbox init` asks.
type ScenarioInput struct {
	ID   string `yaml:"id"`
	Kind string `yaml:"kind"`
	// Prompt is the question for a text or secret input.
	Prompt string `yaml:"prompt"`
	// Confirm makes a secret input optional. init asks this yes or no
	// question before the secret.
	Confirm string `yaml:"confirm"`
	// Default is the answer init offers for a text input.
	Default string `yaml:"default"`
}

// ScenarioResource is one zone-setup resource.
type ScenarioResource struct {
	Identifier string `yaml:"identifier"`
	Name       string `yaml:"name"`
	// Credential is the id of the secret or github input that gives the
	// resource its credential.
	Credential string `yaml:"credential"`
}

// ScenarioAgent is the agent container.
type ScenarioAgent struct {
	Args []string `yaml:"args"`
	// Env values are literal, except that {id} is the answer to the text
	// input id.
	Env map[string]string `yaml:"env"`
}

// ScenarioRun says what `sandbox up` does once the sandbox is up.
type ScenarioRun struct {
	Mode string `yaml:"mode"`
	// Command is what shell mode runs in the agent container.
	Command []string `yaml:"command"`
}

// LoadScenario reads and checks scenarios/<name>/scenario.yaml under root.
func LoadScenario(root, name string) (*Scenario, error) {
	if !scenarioName.MatchString(name) {
		return nil, fmt.Errorf("%w: %q is not a scenario name of lowercase letters, digits and dashes",
			ErrScenario, name)
	}
	dir := filepath.Join(root, scenariosDir, name)
	path := filepath.Join(dir, "scenario.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is under this checkout's scenarios.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s does not exist", ErrScenario, path)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScenario, err)
	}
	scenario := &Scenario{Name: name}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err = decoder.Decode(scenario); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrScenario, path, err)
	}
	if err = scenario.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "Dockerfile")); err == nil {
		// The tag follows the build context. An edit to the scenario's files
		// changes the sandbox's template, and run then replaces the pod,
		// which otherwise keeps running the old image.
		digest, err := contextDigest(dir)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrScenario, err)
		}
		scenario.Image = imageRegistry + "sandbox-" + name + ":local-" + digest
	}
	return scenario, nil
}

// contextDigest returns the first 12 hex digits of a SHA-256 over the path,
// permissions and contents of each file under dir. It follows links.
func contextDigest(dir string) (string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("reading the build context %s: %w", dir, err)
	}
	sum := sha256.New()
	//nolint:wrapcheck // The error after the walk names the build context.
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is a file of the scenario.
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(sum, "%s\x00%o\x00%d\x00", filepath.ToSlash(rel), info.Mode().Perm(), len(raw))
		_, _ = sum.Write(raw)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("reading the build context %s: %w", dir, err)
	}
	return hex.EncodeToString(sum.Sum(nil))[:12], nil
}

// Scenarios loads every scenario under root, sorted by name.
func Scenarios(root string) ([]*Scenario, error) {
	paths, err := filepath.Glob(filepath.Join(root, scenariosDir, "*", "scenario.yaml"))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrScenario, err)
	}
	scenarios := make([]*Scenario, 0, len(paths))
	for _, path := range paths {
		scenario, err := LoadScenario(root, filepath.Base(filepath.Dir(path)))
		if err != nil {
			return nil, err
		}
		scenarios = append(scenarios, scenario)
	}
	if len(scenarios) == 0 {
		return nil, fmt.Errorf("%w: %s has no scenario", ErrScenario, filepath.Join(root, scenariosDir))
	}
	return scenarios, nil
}

// Values returns the values file for both charts that runs the scenario's
// agent with its endpoints. answers holds the text inputs' answers. A text
// input without an answer uses its default. The install chart does not
// read endpoints.
func (s *Scenario) Values(answers map[string]string) ([]byte, error) {
	type agent struct {
		Args []string          `json:"args,omitempty"`
		Env  map[string]string `json:"env,omitempty"`
	}
	type images struct {
		Agent string `json:"agent"`
	}
	type values struct {
		Endpoints []any   `json:"endpoints"`
		Images    *images `json:"images,omitempty"`
		Agent     agent   `json:"agent"`
	}
	out := values{Endpoints: s.Endpoints, Agent: agent{Args: s.Agent.Args}}
	if s.Image != "" {
		out.Images = &images{Agent: s.Image}
	}
	withDefaults := map[string]string{}
	for _, input := range s.Inputs {
		if input.Default != "" {
			withDefaults[input.ID] = input.Default
		}
	}
	maps.Copy(withDefaults, answers)
	var missing error
	for name, value := range s.Agent.Env {
		if out.Agent.Env == nil {
			out.Agent.Env = map[string]string{}
		}
		out.Agent.Env[name] = answerRef.ReplaceAllStringFunc(value, func(ref string) string {
			answer, ok := withDefaults[ref[1:len(ref)-1]]
			if !ok {
				missing = fmt.Errorf("%w: %s has no answer for %s. Run %s init", ErrScenario, answersFile, ref, sandboxCommand)
			}
			return answer
		})
	}
	if missing != nil {
		return nil, missing
	}
	raw, err := k8syaml.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encoding the scenario values: %w", err)
	}
	return raw, nil
}

// scenario loads the scenario KEYCARD_LOCAL_SCENARIO names, or
// DefaultScenario.
func (l *Local) scenario() (*Scenario, error) {
	name := l.Settings[KeyScenario]
	if name == "" {
		name = DefaultScenario
	}
	return LoadScenario(l.Root, name)
}

// modes returns the run modes a scenario can name.
func modes() []string {
	return []string{ModeChecks, ModeShell, ModeBackground}
}

// check refuses a scenario `sandbox` cannot use. Every error wraps
// ErrScenario.
func (s *Scenario) check() error {
	if s.Title == "" {
		return fmt.Errorf("%w: title is required", ErrScenario)
	}
	kinds, err := s.checkInputs()
	if err != nil {
		return err
	}
	for i, resource := range s.Resources {
		if resource.Identifier == "" {
			return fmt.Errorf("%w: resources[%d].identifier is required", ErrScenario, i)
		}
		if kind := kinds[resource.Credential]; kind != inputSecret && kind != inputGitHub {
			return fmt.Errorf("%w: resources[%d].credential must be the id of a secret or github input", ErrScenario, i)
		}
	}
	if err = s.checkEnv(kinds); err != nil {
		return err
	}
	return s.checkRun()
}

// checkRun checks the run mode and the keys it needs.
func (s *Scenario) checkRun() error {
	if !slices.Contains(modes(), s.Run.Mode) {
		return fmt.Errorf("%w: run.mode %q is not one of %s", ErrScenario, s.Run.Mode, strings.Join(modes(), ", "))
	}
	if (s.Run.Mode == ModeShell) != (len(s.Run.Command) > 0) {
		return fmt.Errorf("%w: run.command is required in shell mode and not allowed in other modes", ErrScenario)
	}
	if s.Run.Mode == ModeBackground && len(s.Agent.Args) == 0 {
		return fmt.Errorf("%w: agent.args is required in background mode", ErrScenario)
	}
	return nil
}

// checkEnv checks agent.env against the kinds of the inputs, by id.
func (s *Scenario) checkEnv(kinds map[string]string) error {
	for name, value := range s.Agent.Env {
		if !envName.MatchString(name) {
			return fmt.Errorf("%w: agent.env: %q is not an environment variable name", ErrScenario, name)
		}
		// A secret never goes in the agent's environment. The proxy adds it
		// to the request.
		for _, ref := range answerRef.FindAllStringSubmatch(value, -1) {
			if kinds[ref[1]] != inputText {
				return fmt.Errorf("%w: agent.env.%s: {%s} must name a text input", ErrScenario, name, ref[1])
			}
		}
	}
	return nil
}

// checkInputs checks the inputs and returns the kind of each, by id.
func (s *Scenario) checkInputs() (map[string]string, error) {
	kinds := map[string]string{}
	for i, input := range s.Inputs {
		if problem := inputProblem(input, kinds); problem != "" {
			return nil, fmt.Errorf("%w: inputs[%d].%s", ErrScenario, i, problem)
		}
		kinds[input.ID] = input.Kind
	}
	return kinds, nil
}

// inputProblem describes what is wrong with input, or returns "". kinds
// holds the kinds of the inputs listed before it.
func inputProblem(input ScenarioInput, kinds map[string]string) string {
	switch {
	case !inputID.MatchString(input.ID):
		return "id must be lowercase letters, digits and dashes"
	case kinds[input.ID] != "":
		return "id " + input.ID + " is listed twice"
	case !slices.Contains([]string{inputText, inputSecret, inputGitHub}, input.Kind):
		return "kind must be text, secret or github"
	case input.Kind != inputGitHub && input.Prompt == "":
		return "prompt is required"
	case input.Confirm != "" && input.Kind != inputSecret:
		return "confirm is only for a secret"
	case input.Default != "" && input.Kind != inputText:
		return "default is only for a text input"
	}
	return ""
}

// scenarioValues loads the scenario and returns its values file with the
// answers in local/answers.yaml.
func (l *Local) scenarioValues() (*Scenario, []byte, error) {
	scenario, err := l.scenario()
	if err != nil {
		return nil, nil, err
	}
	answers, err := l.answers()
	if err != nil {
		return nil, nil, err
	}
	values, err := scenario.Values(answers)
	return scenario, values, err
}

// answers reads local/answers.yaml. A missing file gives an empty map.
func (l *Local) answers() (map[string]string, error) {
	answers := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(l.Root, answersFile))
	if errors.Is(err, fs.ErrNotExist) {
		return answers, nil
	}
	if err == nil {
		err = yaml.Unmarshal(raw, &answers)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrScenario, answersFile, err)
	}
	return answers, nil
}
