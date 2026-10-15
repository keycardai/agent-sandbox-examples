package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	// EnvConfig names the config file in place of the default. "-" means
	// none: the settings come from the environment alone.
	EnvConfig = "KEYCARD_SANDBOX_CONFIG"
	// NoConfig is the config path that does not name a file.
	NoConfig = "-"
	// configDirMode and configFileMode keep the config readable by its
	// owner alone.
	configDirMode  = 0o700
	configFileMode = 0o600
	// secretEnvPrefix starts the environment variable that stands for a
	// secret setup keeps. zone-setup reads the secret through it. Setting
	// the variable overrides the kept secret.
	secretEnvPrefix = "KEYCARD_SANDBOX_SECRET_"
)

// Config is what the sandbox needs to come up: the Keycard account, the
// zone and who has an agent, the demo, and the cluster. Setup keeps it for
// the session, and saves it to the config file when the operator agrees. It
// does not hold secrets. Each secret has a name, and the keychain keeps it.
type Config struct {
	Account ConfigAccount `yaml:"account"`
	Zone    ConfigZone    `yaml:"zone"`
	Agents  ConfigAgents  `yaml:"agents"`
	Demo    ConfigDemo    `yaml:"demo"`
	Cluster ConfigCluster `yaml:"cluster"`
}

// ConfigAccount is the organization service account setup signs in with.
type ConfigAccount struct {
	APIURL   string `yaml:"api_url"`
	ClientID string `yaml:"client_id"`
	// Secret names the client secret in the keychain, or is "" when it
	// comes from KEYCARD_CLIENT_SECRET.
	Secret string `yaml:"secret,omitempty"`
	// CAFile is a PEM file of extra roots for an API with a private CA.
	CAFile string `yaml:"ca_file,omitempty"`
}

// ConfigZone is the Keycard zone the sandbox uses.
type ConfigZone struct {
	ID  string `yaml:"id"`
	URL string `yaml:"url"`
	// OnHost is set for a zone on this machine or a private network.
	OnHost bool `yaml:"on_host,omitempty"`
}

// ConfigAgents are the people with an agent.
type ConfigAgents struct {
	// Default is the person setup picks. Every demo has an agent for them,
	// and the commands for one agent, such as `sandbox shell`, act on it.
	Default string `yaml:"default"`
	// Demos holds, for each demo by name, the other people with an agent
	// in that demo.
	Demos map[string][]string `yaml:"demos,omitempty"`
}

// people returns who has an agent in the demo name: the default person,
// then the demo's others.
func (a *ConfigAgents) people(demo string) []string {
	var out []string
	for _, email := range slices.Concat([]string{a.Default}, a.Demos[demo]) {
		if email != "" && !slices.Contains(out, email) {
			out = append(out, email)
		}
	}
	return out
}

// everyone returns who has an agent in any demo.
func (a *ConfigAgents) everyone() []string {
	all := []string{a.Default}
	for _, people := range a.Demos {
		all = append(all, people...)
	}
	all = slices.DeleteFunc(all, func(email string) bool { return email == "" })
	return slices.Compact(slices.Sorted(slices.Values(all)))
}

// ConfigDemo is the demo, its answers and its credentials.
type ConfigDemo struct {
	Name string `yaml:"name"`
	// Answers holds the answers to the demo's questions, by input id, and
	// People each person's own answers, by email.
	Answers     map[string]string            `yaml:"answers,omitempty"`
	People      map[string]map[string]string `yaml:"people,omitempty"`
	Credentials []ConfigCredential           `yaml:"credentials,omitempty"`
	// GitHubLogin is the GitHub user the checks expect, if any.
	GitHubLogin string `yaml:"github_login,omitempty"`
}

// ConfigCredential is where the credential of one of the demo's resources
// comes from: a provider each person signs in with, a secret in the
// keychain, or an environment variable.
type ConfigCredential struct {
	Identifier string `yaml:"identifier"`
	Name       string `yaml:"name"`
	Provider   string `yaml:"provider,omitempty"`
	Secret     string `yaml:"secret,omitempty"`
	Env        string `yaml:"env,omitempty"`
}

// ConfigCluster is where the sandbox runs, and how Keycard reaches it.
type ConfigCluster struct {
	// Name is the local k3d cluster's, and KubeContext an existing
	// cluster's.
	Name        string `yaml:"name,omitempty"`
	KubeContext string `yaml:"kube_context,omitempty"`
	ImageTag    string `yaml:"image_tag,omitempty"`
	CIDRs       string `yaml:"cidrs,omitempty"`
	Issuer      string `yaml:"issuer,omitempty"`
	Tunnel      string `yaml:"tunnel,omitempty"`
}

// DefaultConfigPath returns $XDG_CONFIG_HOME/keycard/sandbox.yaml, or
// ~/.config/keycard/sandbox.yaml. It uses ~/.config on macOS too, as most
// CLIs do.
func DefaultConfigPath(getenv func(string) string) (string, error) {
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: finding your home directory: %w", ErrSettings, err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "keycard", "sandbox.yaml"), nil
}

// Command returns the command that runs this program with args. It adds
// --config when the settings come from another file than the default, or
// from the environment alone, so the command reads the same settings.
func (l *Local) Command(args ...string) string {
	words := []string{sandboxCommand}
	if l.Store != nil {
		if path, err := DefaultConfigPath(l.getenv); err != nil || l.Store.Path() != path {
			words = append(words, "--config", HomeRelative(l.Store.Path()))
		}
	}
	return strings.Join(append(words, args...), " ")
}

// LoadConfig reads the config file at path. For a missing file, or NoConfig,
// it returns a nil config and a nil error.
func LoadConfig(path string) (*Config, error) {
	if path == NoConfig || path == "" {
		return nil, nil //nolint:nilnil // No file: setup has not saved any settings.
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the config file of this program.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // No file: setup has not saved any settings.
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSettings, err)
	}
	config := &Config{}
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err = decoder.Decode(config); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrSettings, path, err)
	}
	return config, nil
}

// configHeader opens the config file.
const configHeader = "# Settings of the Keycard sandbox, saved by its setup. Secrets are not\n" +
	"# in this file: the OS keychain keeps them, under the service \"keycard\".\n"

// save writes the config to path, readable by its owner alone.
func (c *Config) save(path string) error {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encoding the settings: %w", err)
	}
	return writePrivate(path, append([]byte(configHeader), raw...))
}

// writePrivate writes raw to path, readable by its owner alone, even when
// the file exists with a wider mode, as one written by hand can.
func writePrivate(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), configDirMode); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, raw, configFileMode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(path, configFileMode); err != nil {
		return fmt.Errorf("making %s readable by its owner alone: %w", path, err)
	}
	return nil
}

// zoneSetupValues returns zone-setup's values: the config's, or the
// values file KEYCARD_LOCAL_ZONE_SETUP_VALUES names. A non-empty issuerURL
// replaces the issuer, and an error about it names source.
func (l *Local) zoneSetupValues(issuerURL, source string) (*zonesetup.Values, error) {
	if file := l.Settings[KeyZoneSetupValues]; file != "" {
		if !filepath.IsAbs(file) {
			file = filepath.Join(l.Root, file)
		}
		values, err := zonesetup.ReadValuesFile(file, issuerURL, source)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}
		return values, nil
	}
	config := l.config()
	if config == nil {
		return nil, fmt.Errorf("%w: setup has not run yet. Run %s", ErrSettings, l.Command("init"))
	}
	if url := l.Settings[KeyZoneURL]; url != config.Zone.URL {
		return nil, fmt.Errorf("%w: %s is %s, but the settings set the zone up at %s. Unset it, or run %s "+
			"to change the zone", ErrSettings, KeyZoneURL, url, config.Zone.URL, l.Command("init"))
	}
	raw, err := yaml.Marshal(config.valuesFile(l.demo()))
	if err != nil {
		return nil, fmt.Errorf("encoding the zone-setup values: %w", err)
	}
	values, err := zonesetup.ParseValuesWithIssuer(raw, issuerURL, source)
	if err != nil {
		return nil, fmt.Errorf("the settings: %w", err)
	}
	return values, nil
}

// config returns what setup asked, or nil before setup.
func (l *Local) config() *Config {
	return l.Store.Config()
}

// Forget deletes the saved settings, and every secret setup can put in the
// keychain: the account's, the GitHub token, and each demo's. It deletes
// them even when the settings name another demo, or none. It returns how
// many secrets it deleted. When a demo does not load, Forget deletes the
// rest and returns the error, since that demo's secrets can be left.
func (l *Local) Forget() (int, error) {
	known := []string{managementSecret, githubTokenSecret}
	scenarios, scenariosErr := Scenarios(l.Root)
	for _, scenario := range scenarios {
		for _, input := range scenario.Inputs {
			if input.Kind == inputSecret {
				known = append(known, input.ID)
			}
		}
	}
	deleted, err := l.Store.Forget(known...)
	if scenariosErr != nil {
		err = errors.Join(err, fmt.Errorf("reading the demos to find their secrets: %w", scenariosErr))
	}
	return deleted, err
}

// loadSecrets reads the secrets values names. Each comes from its
// environment variable when that is set, and otherwise from the secret it
// stands for, typed this session or kept in the keychain. A keychain that
// refuses fails the read.
func (l *Local) loadSecrets(values *zonesetup.Values) (*zonesetup.Secrets, error) {
	var refused error
	lookup := func(name string) (string, bool) {
		if value := l.getenv(name); value != "" {
			return value, true
		}
		secret, err := l.Store.secret(secretName(name))
		if err != nil && !errors.Is(err, ErrNoSecret) && refused == nil {
			refused = err
		}
		return secret, secret != ""
	}
	secrets, err := zonesetup.LoadSecrets(values, lookup, os.ReadFile)
	if refused != nil {
		return nil, refused
	}
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	return secrets, nil
}

// keptSecret returns the secret the environment variable name stands for,
// from the environment, this session or the keychain, or "". It is for the
// defaults of setup's questions: a keychain that refuses counts as no
// secret here, and saving reports the refusal.
func (l *Local) keptSecret(name string) string {
	if name == "" {
		return ""
	}
	if value := l.getenv(name); value != "" {
		return value
	}
	secret, _ := l.Store.secret(secretName(name))
	return secret
}

// getenv reads the environment through Getenv, if it is set.
func (l *Local) getenv(key string) string {
	if l.Getenv == nil {
		return ""
	}
	return l.Getenv(key)
}

// envSecret returns the service account's client secret from the
// environment, and the variable that holds it.
func (l *Local) envSecret() (string, string) {
	for _, key := range []string{EnvClientSecret, zonesetup.DefaultManagementSecretEnv} {
		if secret := l.getenv(key); secret != "" {
			return secret, key
		}
	}
	return "", ""
}

// useAccountEnv puts the service account from KEYCARD_CLIENT_ID and the
// client secret variables over the one the settings name.
func (l *Local) useAccountEnv(account *zonesetup.ManagementServiceAccountValues) {
	if id := l.getenv(EnvClientID); id != "" {
		account.ClientID = id
	}
	if _, key := l.envSecret(); key != "" {
		account.ClientSecretEnv, account.ClientSecretFile = key, ""
	}
}

// SettingsFrom returns the settings of config, with each non-empty setting
// from getenv over them. A nil config gives the environment's alone.
func SettingsFrom(config *Config, getenv func(string) string) Settings {
	settings := Settings{}
	if config != nil {
		maps.Copy(settings, config.settings())
	}
	for _, key := range settingKeys() {
		if value := getenv(key); value != "" {
			settings[key] = value
		}
	}
	return settings
}

// settings returns the config as the settings the steps read.
func (c *Config) settings() Settings {
	onHost := ""
	if c.Zone.OnHost {
		onHost = settingTrue
	}
	return Settings{
		KeyCluster: c.Cluster.Name, KeyIssuer: c.Cluster.Issuer, KeyTunnel: c.Cluster.Tunnel,
		KeyZoneURL: c.Zone.URL, KeyZoneOnHost: onHost, KeyUserEmail: c.Agents.Default,
		KeyGitHubLogin: c.Demo.GitHubLogin, KeyCAFile: c.Account.CAFile, KeyKubeContext: c.Cluster.KubeContext,
		KeyImageTag: c.Cluster.ImageTag, KeyClusterCIDRs: c.Cluster.CIDRs, KeyScenario: c.Demo.Name,
	}
}

// valuesFile returns the config as zone-setup's values file, with an agent
// for each person the demo has. A secret the keychain keeps is named by the
// environment variable secretEnv makes.
func (c *Config) valuesFile(demo string) valuesFile {
	out := valuesFile{ManagementAPIURL: c.Account.APIURL, ZoneID: c.Zone.ID, ZoneURL: c.Zone.URL}
	out.Account.ClientID = c.Account.ClientID
	if c.Account.Secret != "" {
		out.Account.ClientSecretEnv = secretEnv(c.Account.Secret)
	}
	for _, email := range c.Agents.people(demo) {
		out.Users = append(out.Users, zonesetup.UserValues{Email: email})
	}
	for _, credential := range c.Demo.Credentials {
		resource := resourceValues{
			Identifier: credential.Identifier, Name: credential.Name, Prefix: true,
			CredentialProvider: credential.Provider, SecretEnv: credential.Env,
		}
		if credential.Secret != "" {
			resource.SecretEnv = secretEnv(credential.Secret)
		}
		out.Resources = append(out.Resources, resource)
	}
	return out
}

// zoneSetupValues returns the config as zone-setup's values, with no
// checks, for the defaults of setup's questions.
func (c *Config) zoneSetupValues(demo string) zonesetup.Values {
	var values zonesetup.Values
	if raw, err := yaml.Marshal(c.valuesFile(demo)); err == nil {
		_ = yaml.Unmarshal(raw, &values)
	}
	return values
}

// answers returns the demo's answers.
func (c *Config) answers() scenarioAnswers {
	answers := scenarioAnswers{Shared: maps.Clone(c.Demo.Answers), Users: map[string]map[string]string{}}
	if answers.Shared == nil {
		answers.Shared = map[string]string{}
	}
	for email, own := range c.Demo.People {
		answers.Users[email] = maps.Clone(own)
	}
	return answers
}

// configFrom makes a config of what setup asked: the settings, zone-setup's
// values, the demo's answers, and each demo's other people with an agent.
func configFrom(env Settings, out *zonesetup.Values, answers scenarioAnswers, demos map[string][]string) *Config {
	account := out.ManagementServiceAccount
	config := &Config{
		Account: ConfigAccount{
			APIURL: out.ManagementAPIURL, ClientID: account.ClientID,
			Secret: secretName(account.ClientSecretEnv), CAFile: env[KeyCAFile],
		},
		Zone:   ConfigZone{ID: out.ZoneID, URL: out.ZoneURL, OnHost: env[KeyZoneOnHost] == settingTrue},
		Agents: ConfigAgents{Default: env[KeyUserEmail], Demos: demos},
		Demo: ConfigDemo{
			Name: env[KeyScenario], Answers: answers.Shared, People: answers.Users, GitHubLogin: env[KeyGitHubLogin],
		},
		Cluster: ConfigCluster{
			Name: env[KeyCluster], KubeContext: env[KeyKubeContext], ImageTag: env[KeyImageTag],
			CIDRs: env[KeyClusterCIDRs], Issuer: env[KeyIssuer], Tunnel: env[KeyTunnel],
		},
	}
	if len(config.Demo.Answers) == 0 {
		config.Demo.Answers = nil
	}
	if len(config.Demo.People) == 0 {
		config.Demo.People = nil
	}
	for _, resource := range out.Resources {
		credential := ConfigCredential{
			Identifier: resource.Identifier, Name: resource.Name, Provider: resource.CredentialProvider,
		}
		if name := secretName(resource.SecretEnv); name != "" {
			credential.Secret = name
		} else {
			credential.Env = resource.SecretEnv
		}
		config.Demo.Credentials = append(config.Demo.Credentials, credential)
	}
	return config
}

// secretEnv is the environment variable that stands for the secret name:
// KEYCARD_SANDBOX_SECRET_ and the name in capitals, with _ for -.
func secretEnv(name string) string {
	return secretEnvPrefix + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// secretName is the name of the secret env stands for, or "" when env is not
// one secretEnv makes.
func secretName(env string) string {
	name, ok := strings.CutPrefix(env, secretEnvPrefix)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

// secretNames lists the names of the secrets the config uses.
func (c *Config) secretNames() []string {
	var names []string
	if c.Account.Secret != "" {
		names = append(names, c.Account.Secret)
	}
	for _, credential := range c.Demo.Credentials {
		if credential.Secret != "" {
			names = append(names, credential.Secret)
		}
	}
	return names
}

// HomeRelative writes path with ~ for the home directory, as people type
// it.
func HomeRelative(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return filepath.Join("~", rest)
	}
	return path
}
