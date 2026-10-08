package sandbox

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/keycardai/keycard-go"
	yaml "go.yaml.in/yaml/v3"

	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	defaultManagementAPI = "https://api.keycard.ai"
	defaultCluster       = "keycard-sandbox-local"
	zoneSetupFile        = "zone-setup.yaml"
	// githubTokenFile is the secret file of a GitHub personal access token.
	githubTokenFile = "github-token"
	// zonePoll and zoneWait bound the wait for a new zone. The policy
	// engine caches a 404 for an unknown zone for about a minute.
	zonePoll = 5 * time.Second
	zoneWait = 2 * time.Minute
	// Choice values that cannot match a zone ID, user email, provider
	// identifier or kubeconfig context.
	newZone    = "\x00new"
	newCluster = "\x00k3d"
	checkAgain = "\x00again"
	// gitHubToken is the GitHub choice for a personal access token.
	gitHubToken = "\x00token"
	// How the issuer is published.
	issuerQuick     = "quick"
	issuerNamed     = "named"
	issuerPublished = "published"
)

// ErrAnswer reports a missing or refused answer.
var ErrAnswer = errors.New("no answer")

// Asker asks the operator one question at a time and returns the answer.
// value is the default. When check returns an error, the Asker asks again.
type Asker interface {
	Input(question, value string, check func(string) error) (string, error)
	Secret(question string, check func(string) error) (string, error)
	Choose(question string, options []Option, value string) (string, error)
	Confirm(question string, value bool) (bool, error)
}

// Option is one choice. Choose shows Label and returns Value.
type Option struct {
	Label, Value string
}

// InitResult holds the answers Init does not write to a file.
type InitResult struct {
	// ReplaceActive reports whether the operator agreed to replace the
	// zone's active policy set.
	ReplaceActive bool
}

// wizard is the state of one Init.
type wizard struct {
	l     *Local
	ask   Asker
	prev  previous
	env   Settings
	roots *x509.CertPool
	out   valuesFile
	org   *zonesetup.Org
	zone  *zonesetup.Session
	// answers holds the text input answers of every scenario, by input id.
	answers map[string]string
}

// previous holds the values file from before Init. exists reports whether
// local/local.env or the values file exists.
type previous struct {
	values zonesetup.Values
	exists bool
}

// valuesFile is the zone-setup values file Init writes. Its keys are a
// subset of zonesetup.Values.
type valuesFile struct {
	ManagementAPIURL string `yaml:"management_api_url"`
	ZoneID           string `yaml:"zone_id"`
	ZoneURL          string `yaml:"zone_url"`
	Account          struct {
		ClientID         string `yaml:"client_id"`
		ClientSecretFile string `yaml:"client_secret_file,omitempty"`
	} `yaml:"management_service_account"`
	Users     []zonesetup.UserValues `yaml:"users"`
	Resources []resourceValues       `yaml:"resources,omitempty"`
}

type resourceValues struct {
	Identifier         string `yaml:"identifier"`
	Name               string `yaml:"name"`
	Prefix             bool   `yaml:"prefix"`
	CredentialProvider string `yaml:"credential_provider,omitempty"`
	SecretFile         string `yaml:"secret_file,omitempty"`
}

// Init asks the operator for the settings and the scenario's inputs, and
// writes local/local.env, the zone-setup values file and local/answers.yaml.
// Secrets the operator types go in 0600 files under local/out/secrets, and
// the values file names them. Each question defaults to the value in the
// file Init replaces.
//
// Init reads zones, users and providers from the management API. Its only
// write creates a zone, when the operator asks for one. It does not change
// policies. It asks whether `sandbox run` may replace another active policy
// set, and returns the answer.
func (l *Local) Init(ctx context.Context, ask Asker) (InitResult, error) {
	w := &wizard{l: l, ask: ask, prev: l.previousValues(), env: Settings{}}
	change, err := w.chooseChanges()
	if err != nil || len(change) == 0 {
		return InitResult{}, err
	}
	if err = w.runSections(ctx, change); err != nil {
		return InitResult{}, err
	}
	replace := false
	if w.zone != nil {
		if replace, err = w.askReplaceActive(ctx); err != nil {
			return InitResult{}, err
		}
	}
	if err = w.write(); err != nil {
		return InitResult{}, err
	}
	next := sandboxCommand + " up"
	if replace {
		next += " --" + zonesetup.ReplaceActiveFlag
	}
	_, _ = fmt.Fprintf(l.Out, "\nTo start the sandbox, run:\n\n  %s\n", next)
	return InitResult{ReplaceActive: replace}, nil
}

// The sections of the settings, in the order Init asks them.
const (
	sectionAccount   = "account"
	sectionZone      = "zone"
	sectionScenario  = "scenario"
	sectionCluster   = "cluster"
	changeEverything = "\x00everything"
	changeNothing    = "\x00nothing"
)

// chooseChanges returns the sections to ask. On a first run that is every
// section. When the settings files exist, it shows the current settings and
// asks what to change. If the answer is nothing, it returns an empty map and
// Init does not write the files.
func (w *wizard) chooseChanges() (map[string]bool, error) {
	all := map[string]bool{sectionAccount: true, sectionZone: true, sectionScenario: true, sectionCluster: true}
	if !w.prev.exists {
		return all, nil
	}
	_, _ = fmt.Fprintf(w.l.Out, "Your current settings, from local/local.env and %s:\n\n%s\n",
		w.l.valuesName(), w.summary())
	choice, err := w.choose("What do you want to change?", []Option{
		{Label: "Nothing. Keep these settings.", Value: changeNothing},
		{Label: "Everything. Answer each question again, starting from these answers.", Value: changeEverything},
		{Label: "The Keycard account", Value: sectionAccount},
		{Label: "The zone and the user", Value: sectionZone},
		{Label: "The demo, its credentials and its settings", Value: sectionScenario},
		{Label: "The cluster and the issuer", Value: sectionCluster},
	}, changeNothing)
	switch {
	case err != nil:
		return nil, err
	case choice == changeNothing:
		_, _ = fmt.Fprintf(w.l.Out, "Kept local/local.env and %s as they are.\n", w.l.valuesName())
		return map[string]bool{}, nil
	case choice == changeEverything:
		return all, nil
	}
	return map[string]bool{choice: true}, nil
}

// summary describes the current settings, one line per section.
func (w *wizard) summary() string {
	values := w.prev.values
	user := "no user"
	if len(values.Users) > 0 {
		user = values.Users[0].Email
	}
	return fmt.Sprintf("  Keycard account          %s at %s\n"+
		"  Zone and user            %s in %s\n"+
		"  Demo and credentials     %s\n"+
		"  Cluster and issuer       %s\n",
		cmp.Or(values.ManagementServiceAccount.ClientID, "none"), cmp.Or(values.ManagementAPIURL, "none"),
		user, cmp.Or(values.ZoneURL, values.ZoneID, "no zone"),
		w.scenarioSummary(), w.clusterSummary())
}

// scenarioSummary names the scenario and where each resource's credential
// comes from.
func (w *wizard) scenarioSummary() string {
	credentials := make([]string, 0, len(w.prev.values.Resources))
	for _, resource := range w.prev.values.Resources {
		source := "secret file"
		if resource.CredentialProvider != "" {
			source = "provider " + resource.CredentialProvider
		}
		credentials = append(credentials, resource.Name+" ("+source+")")
	}
	return cmp.Or(w.l.Settings[KeyScenario], DefaultScenario) + ": " +
		cmp.Or(strings.Join(credentials, ", "), "no credentials")
}

func (w *wizard) clusterSummary() string {
	settings := w.l.Settings
	k3d := "a local k3d cluster, " + cmp.Or(settings[KeyCluster], defaultCluster) + ", with "
	switch {
	case settings[KeyKubeContext] != "":
		return "the existing cluster in the kubeconfig context " + settings[KeyKubeContext]
	case settings[KeyTunnel] != "":
		return k3d + "the named tunnel " + settings[KeyTunnel] + " at " + settings[KeyIssuer]
	case settings[KeyIssuer] != "":
		return k3d + "the issuer " + settings[KeyIssuer]
	default:
		return k3d + "a quick tunnel"
	}
}

// runSections asks the chosen sections and keeps the others from the
// files.
func (w *wizard) runSections(ctx context.Context, change map[string]bool) error {
	if err := w.account(ctx, change); err != nil {
		return err
	}
	for _, section := range []struct {
		name      string
		ask, keep func(context.Context) error
	}{
		{sectionZone, w.askZone, w.keepZone},
		{sectionScenario, w.chooseScenario, w.keepScenario},
		{sectionCluster, w.chooseCluster, w.keepCluster},
	} {
		run := section.keep
		if change[section.name] {
			run = section.ask
		}
		if err := run(ctx); err != nil {
			return err
		}
	}
	return nil
}

// account asks for the Keycard account when that section is chosen. When
// another chosen section needs Keycard, it signs in with the saved account.
// Otherwise it keeps the account from the files.
func (w *wizard) account(ctx context.Context, change map[string]bool) error {
	switch {
	case change[sectionAccount]:
		return w.login(ctx)
	case change[sectionZone] || change[sectionScenario]:
		return w.signInAgain(ctx)
	default:
		w.keepAccount()
		return nil
	}
}

func (w *wizard) askZone(ctx context.Context) error {
	if err := w.chooseZone(ctx); err != nil {
		return err
	}
	return w.chooseUser(ctx)
}

// keepAccount keeps the management API, the service account and the extra
// roots from the files.
func (w *wizard) keepAccount() {
	w.out.ManagementAPIURL = w.prev.values.ManagementAPIURL
	w.out.Account.ClientID = w.prev.values.ManagementServiceAccount.ClientID
	w.out.Account.ClientSecretFile = w.prev.values.ManagementServiceAccount.ClientSecretFile
	w.env[KeyCAFile] = w.l.Settings[KeyCAFile]
}

// signInAgain signs in with the saved account. If there is no saved secret
// or the sign-in fails, it calls login.
func (w *wizard) signInAgain(ctx context.Context) error {
	w.keepAccount()
	secret := cmp.Or(w.getenv(zonesetup.DefaultManagementSecretEnv), readSecret(w.out.Account.ClientSecretFile))
	roots, err := rootsFrom(w.env[KeyCAFile])
	if err != nil || secret == "" || w.out.ManagementAPIURL == "" {
		return w.login(ctx)
	}
	w.roots = roots
	if w.org, err = zonesetup.Login(ctx, w.out.ManagementAPIURL, w.out.Account.ClientID, secret, roots); err != nil {
		_, _ = fmt.Fprintf(w.l.Out, "Signing in with the saved account failed: %v\n", err)
		return w.login(ctx)
	}
	return nil
}

// keepZone keeps the zone and the user from the files. After a sign-in, it
// also opens a session on the zone for the sections that follow.
func (w *wizard) keepZone(ctx context.Context) error {
	w.out.ZoneID, w.out.ZoneURL, w.out.Users = w.prev.values.ZoneID, w.prev.values.ZoneURL, w.prev.values.Users
	for _, key := range []string{KeyZoneURL, KeyZoneOnHost, KeyUserEmail} {
		w.env[key] = w.l.Settings[key]
	}
	if w.org == nil {
		return nil
	}
	zones, err := w.org.Zones(ctx)
	if err != nil {
		return fmt.Errorf("listing the zones: %w", err)
	}
	for i := range zones {
		if zones[i].ID == w.out.ZoneID {
			w.zone = w.org.Session(&zones[i])
			return nil
		}
	}
	return fmt.Errorf("%w: the service account cannot see the saved zone %s. Change the zone",
		ErrAnswer, cmp.Or(w.out.ZoneID, "(none)"))
}

// keepScenario keeps the scenario from local.env and the resources from the
// values file. It does not change local/answers.yaml.
func (w *wizard) keepScenario(context.Context) error {
	w.env[KeyScenario] = w.l.Settings[KeyScenario]
	for _, resource := range w.prev.values.Resources {
		w.out.Resources = append(w.out.Resources, resourceValues{
			Identifier: resource.Identifier, Name: resource.Name, Prefix: resource.Prefix,
			CredentialProvider: resource.CredentialProvider, SecretFile: resource.SecretFile,
		})
	}
	return nil
}

// keepCluster keeps the cluster and the issuer settings from local.env.
func (w *wizard) keepCluster(context.Context) error {
	for _, key := range []string{KeyIssuer, KeyTunnel, KeyKubeContext, KeyImageTag, KeyClusterCIDRs} {
		w.env[key] = w.l.Settings[key]
	}
	return nil
}

func (w *wizard) getenv(key string) string {
	if w.l.Getenv == nil {
		return ""
	}
	return w.l.Getenv(key)
}

func (w *wizard) input(question, value string, check func(string) error) (string, error) {
	answer, err := w.ask.Input(question, value, check)
	if err != nil {
		return "", fmt.Errorf("answering: %w", err)
	}
	return answer, nil
}

func (w *wizard) secret(question string, check func(string) error) (string, error) {
	answer, err := w.ask.Secret(question, check)
	if err != nil {
		return "", fmt.Errorf("answering: %w", err)
	}
	return answer, nil
}

func (w *wizard) choose(question string, options []Option, value string) (string, error) {
	answer, err := w.ask.Choose(question, options, value)
	if err != nil {
		return "", fmt.Errorf("answering: %w", err)
	}
	return answer, nil
}

func (w *wizard) confirm(question string, value bool) (bool, error) {
	answer, err := w.ask.Confirm(question, value)
	if err != nil {
		return false, fmt.Errorf("answering: %w", err)
	}
	return answer, nil
}

// login asks for the management API URL, the extra trusted roots if the API
// is not production, and the service account, then signs in. It asks for the
// account again until the API accepts it.
func (w *wizard) login(ctx context.Context) error {
	var err error
	w.out.ManagementAPIURL, err = w.input("Keycard management API URL",
		cmp.Or(w.prev.values.ManagementAPIURL, defaultManagementAPI), checkHTTPS)
	if err != nil {
		return err
	}
	// A non-production management API, such as localdev, may use a private
	// CA. For production, keep the current setting without asking.
	caFile := w.l.Settings[KeyCAFile]
	if w.out.ManagementAPIURL != defaultManagementAPI {
		caFile, err = w.input("PEM file of extra trusted roots, as an absolute path. "+
			"Leave empty if this API's certificate comes from a public CA.",
			caFile, checkCAFile)
		if err != nil {
			return err
		}
	}
	w.env[KeyCAFile] = caFile
	if w.roots, err = rootsFrom(caFile); err != nil {
		return err
	}
	for {
		clientID, secret, secretFile, err := w.askAccount()
		if err != nil {
			return err
		}
		w.org, err = zonesetup.Login(ctx, w.out.ManagementAPIURL, clientID, secret, w.roots)
		if err == nil {
			w.out.Account.ClientID, w.out.Account.ClientSecretFile = clientID, secretFile
			return nil
		}
		if !errors.Is(err, zonesetup.ErrAuth) {
			return fmt.Errorf("signing in: %w", err)
		}
		_, _ = fmt.Fprintf(w.l.Out, "%v\nTry again.\n", err)
		w.prev.values.ManagementServiceAccount = zonesetup.ManagementServiceAccountValues{ClientID: clientID}
	}
}

// askAccount asks for the service account. The secret comes from
// KEYCARD_MANAGEMENT_CLIENT_SECRET, from the file an earlier Init wrote, or
// from the operator. A typed secret goes in a 0600 file.
func (w *wizard) askAccount() (string, string, string, error) {
	account := w.prev.values.ManagementServiceAccount
	clientID, err := w.input("Organization service account client ID", account.ClientID, required)
	if err != nil {
		return "", "", "", err
	}
	if secret := w.getenv(zonesetup.DefaultManagementSecretEnv); secret != "" {
		_, _ = fmt.Fprintf(w.l.Out, "Using the client secret from %s.\n", zonesetup.DefaultManagementSecretEnv)
		return clientID, secret, "", nil
	}
	if secret := readSecret(account.ClientSecretFile); secret != "" && clientID == account.ClientID {
		keep, keepErr := w.confirm("Keep the saved client secret?", true)
		if keepErr != nil {
			return "", "", "", keepErr
		}
		if keep {
			return clientID, secret, account.ClientSecretFile, nil
		}
	}
	secret, err := w.secret("Service account client secret", required)
	if err != nil {
		return "", "", "", err
	}
	path, err := w.l.writeSecret("management-client-secret", secret)
	return clientID, secret, path, err
}

// chooseZone asks for one of the zones the service account can see, or a
// new one.
func (w *wizard) chooseZone(ctx context.Context) error {
	zones, err := w.org.Zones(ctx)
	if err != nil {
		return fmt.Errorf("listing the zones: %w", err)
	}
	options, value := w.zoneOptions(zones)
	choice, err := w.choose("Which zone does the sandbox use?", options, value)
	if err != nil {
		return err
	}
	var zone *keycard.Zone
	if choice == newZone {
		if zone, err = w.createZone(ctx); err != nil {
			return err
		}
	} else {
		zone = &zones[slices.IndexFunc(zones, func(z keycard.Zone) bool { return z.ID == choice })]
	}
	w.zone = w.org.Session(zone)
	w.out.ZoneID = zone.ID
	w.out.ZoneURL = strings.TrimRight(zone.Protocols.Oauth2.Issuer, "/")
	w.env[KeyZoneURL] = w.out.ZoneURL
	if onHost(ctx, w.out.ZoneURL) {
		w.env[KeyZoneOnHost] = settingTrue
		_, _ = fmt.Fprintln(w.l.Out, "The zone is on this machine or a private network. "+
			"The sandbox connects to it through the Docker host.")
	}
	return nil
}

// zoneOptions returns an option per zone and one to create a zone. The
// default is the zone the previous values file names, or else the first zone.
func (w *wizard) zoneOptions(zones []keycard.Zone) ([]Option, string) {
	options := make([]Option, 0, len(zones)+1)
	value := newZone
	if len(zones) > 0 {
		value = zones[0].ID
	}
	for _, zone := range zones {
		issuer := zone.Protocols.Oauth2.Issuer
		options = append(options, Option{Label: zone.Name + "  " + issuer, Value: zone.ID})
		if zone.ID == w.prev.values.ZoneID || (w.prev.values.ZoneURL != "" && issuer == w.prev.values.ZoneURL) {
			value = zone.ID
		}
	}
	return append(options, Option{Label: "Create a new zone", Value: newZone}), value
}

func (w *wizard) createZone(ctx context.Context) (*keycard.Zone, error) {
	name, err := w.input("Name of the new zone", "sandbox-demo", required)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(w.l.Out, "Creating the zone %s. It takes up to %s to be ready.\n", name, zoneWait)
	zone, err := w.org.CreateZone(ctx, name, zonePoll, zoneWait)
	if err != nil {
		return nil, fmt.Errorf("creating the zone: %w", err)
	}
	_, _ = fmt.Fprintf(w.l.Out, "Created the zone %s (%s) at %s.\n", name, zone.ID, zone.Protocols.Oauth2.Issuer)
	return zone, nil
}

// chooseUser asks for the user the sandbox acts for. The management API
// cannot create users, so chooseUser lists the users again until the operator
// picks one.
func (w *wizard) chooseUser(ctx context.Context) error {
	for {
		users, err := w.zone.Users(ctx)
		if err != nil {
			return fmt.Errorf("listing the zone's users: %w", err)
		}
		options := make([]Option, 0, len(users)+1)
		for _, user := range users {
			options = append(options, Option{Label: user.Email, Value: user.Email})
		}
		options = append(options, Option{Label: "Not listed: check again", Value: checkAgain})
		if len(users) == 0 {
			_, _ = fmt.Fprintln(w.l.Out, "The zone has no users, and init cannot create one. "+
				"Add the user in the Keycard console, or have them sign in to the zone. Then check again.")
		}
		value := cmp.Or(w.l.Settings[KeyUserEmail], checkAgain)
		choice, err := w.choose("Which user does the sandbox act for?", options, value)
		if err != nil {
			return err
		}
		if choice != checkAgain {
			w.out.Users = []zonesetup.UserValues{{Email: choice}}
			w.env[KeyUserEmail] = choice
			return nil
		}
	}
}

// chooseScenario asks which scenario to run, then asks its inputs.
func (w *wizard) chooseScenario(ctx context.Context) error {
	scenarios, err := Scenarios(w.l.Root)
	if err != nil {
		return err
	}
	options := make([]Option, 0, len(scenarios))
	for _, scenario := range scenarios {
		options = append(options, Option{Label: scenario.Title + " (" + scenario.Name + ")", Value: scenario.Name})
	}
	value := cmp.Or(w.l.Settings[KeyScenario], DefaultScenario)
	if !slices.ContainsFunc(scenarios, func(s *Scenario) bool { return s.Name == value }) {
		value = scenarios[0].Name
	}
	choice, err := w.choose("Which demo?", options, value)
	if err != nil {
		return err
	}
	w.env[KeyScenario] = choice
	if w.answers, err = w.l.answers(); err != nil {
		return err
	}
	return w.askInputs(ctx, scenarios[slices.IndexFunc(scenarios, func(s *Scenario) bool { return s.Name == choice })])
}

// askInputs asks the scenario's inputs in order, and adds the scenario's
// resources whose credential the operator gave.
func (w *wizard) askInputs(ctx context.Context, scenario *Scenario) error {
	var err error
	credentials := map[string]resourceValues{}
	for _, input := range scenario.Inputs {
		var credential resourceValues
		switch input.Kind {
		case inputGitHub:
			credential, err = w.chooseGitHub(ctx, w.prevCredential(scenario, input.ID))
		case inputSecret:
			credential, err = w.askSecret(input, w.prevCredential(scenario, input.ID).SecretFile)
		default:
			w.answers[input.ID], err = w.input(input.Prompt, cmp.Or(w.answers[input.ID], input.Default), required)
		}
		if err != nil {
			return err
		}
		if credential != (resourceValues{}) {
			credentials[input.ID] = credential
		}
	}
	for _, resource := range scenario.Resources {
		if credential, ok := credentials[resource.Credential]; ok {
			credential.Identifier, credential.Name, credential.Prefix = resource.Identifier, resource.Name, true
			w.out.Resources = append(w.out.Resources, credential)
		}
	}
	return nil
}

// prevCredential finds the first of the scenario's resources whose
// credential is input id, and returns that resource's provider and secret
// file from the previous values file.
func (w *wizard) prevCredential(scenario *Scenario, id string) resourceValues {
	for _, resource := range scenario.Resources {
		if resource.Credential != id {
			continue
		}
		for _, prev := range w.prev.values.Resources {
			if prev.Identifier == resource.Identifier {
				return resourceValues{CredentialProvider: prev.CredentialProvider, SecretFile: prev.SecretFile}
			}
		}
	}
	return resourceValues{}
}

// askSecret asks for a secret input and writes it to
// local/out/secrets/<id>. It keeps prevFile when that still holds a secret.
// For an input with a confirm question, a no returns an empty credential.
func (w *wizard) askSecret(input ScenarioInput, prevFile string) (resourceValues, error) {
	if input.Confirm != "" {
		add, err := w.confirm(input.Confirm, prevFile != "")
		if err != nil || !add {
			return resourceValues{}, err
		}
	}
	file, err := w.secretFile(prevFile, input.Prompt, input.ID)
	return resourceValues{SecretFile: file}, err
}

// chooseGitHub asks where the GitHub token comes from: a GitHub provider in
// the zone, which gives each user their own token, or a personal access
// token, which every user shares. prev is the previous answer.
func (w *wizard) chooseGitHub(ctx context.Context, prev resourceValues) (resourceValues, error) {
	providers, err := w.zone.Providers(ctx)
	if err != nil {
		return resourceValues{}, fmt.Errorf("listing the zone's providers: %w", err)
	}
	options := w.gitHubOptions(providers)
	value := options[0].Value
	switch {
	case slices.ContainsFunc(options, func(o Option) bool { return o.Value == prev.CredentialProvider }):
		value = prev.CredentialProvider
	case prev.SecretFile != "":
		value = gitHubToken
	}
	choice, err := w.choose("How does the sandbox get a GitHub token?", options, value)
	if err != nil || choice != gitHubToken {
		return resourceValues{CredentialProvider: choice}, err
	}
	file, err := w.secretFile(prev.SecretFile, "GitHub personal access token", githubTokenFile)
	return resourceValues{SecretFile: file}, err
}

// gitHubOptions returns a choice for each GitHub provider in the zone, then
// one for a personal access token.
func (w *wizard) gitHubOptions(providers []keycard.Provider) []Option {
	var options []Option
	for _, provider := range providers {
		if gitHubProvider(provider) {
			options = append(options, Option{
				Label: "GitHub provider " + provider.Name + " (" + provider.Identifier + "). Each user signs in to GitHub.",
				Value: provider.Identifier,
			})
		}
	}
	if len(options) == 0 {
		_, _ = fmt.Fprintln(w.l.Out, "The zone has no GitHub provider. To give each user their own token, "+
			"add one in the Keycard console and run init again.")
	}
	return append(options, Option{
		Label: "A personal access token. Every user acts as the token's owner.",
		Value: gitHubToken,
	})
}

// gitHubProvider reports whether one of the provider's OAuth endpoints is on
// github.com.
func gitHubProvider(provider keycard.Provider) bool {
	oauth := provider.Protocols.Oauth2
	for _, endpoint := range []string{oauth.AuthorizationEndpoint, oauth.TokenEndpoint, oauth.Issuer} {
		if parsed, err := url.Parse(endpoint); err == nil && parsed.Hostname() == "github.com" {
			return true
		}
	}
	return false
}

// secretFile asks whether to keep the secret in prevFile, when it holds
// one. Otherwise, or when the answer is no, it asks for the secret and
// writes it to local/out/secrets/name.
func (w *wizard) secretFile(prevFile, question, name string) (string, error) {
	if readSecret(prevFile) != "" {
		keep, err := w.confirm("Keep the saved "+question+"?", true)
		if err != nil || keep {
			return prevFile, err
		}
	}
	secret, err := w.secret(question, required)
	if err != nil {
		return "", err
	}
	return w.l.writeSecret(name, secret)
}

// chooseCluster asks whether to create a local k3d cluster or use an
// existing one from the kubeconfig. It keeps the saved image tag.
func (w *wizard) chooseCluster(ctx context.Context) error {
	contexts, err := w.l.kubeContexts(ctx)
	if err != nil {
		return err
	}
	options := []Option{{Label: "A new local k3d cluster", Value: newCluster}}
	for _, name := range contexts {
		options = append(options, Option{Label: "Existing cluster in kubeconfig context " + name, Value: name})
	}
	value := cmp.Or(w.l.Settings[KeyKubeContext], newCluster)
	if !slices.Contains(contexts, value) {
		value = newCluster
	}
	choice, err := w.choose("Which cluster runs the sandbox?", options, value)
	if err != nil {
		return err
	}
	w.env[KeyImageTag] = w.l.Settings[KeyImageTag]
	if choice == newCluster {
		return w.chooseIssuer(ctx)
	}
	return w.existingCluster(ctx, choice)
}

// existingCluster reads the cluster's issuer and asks for the cluster's
// ranges.
func (w *wizard) existingCluster(ctx context.Context, kubeContext string) error {
	existing := &Local{Root: w.l.Root, Settings: Settings{KeyKubeContext: kubeContext}, Tool: w.l.Tool}
	issuerURL, err := existing.clusterIssuer(ctx, kubeContext, "")
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w.l.Out, "The cluster's issuer is %s. The zone fetches the cluster's keys from it. "+
		"It must be reachable from the internet.\n", issuerURL)
	if w.env[KeyZoneOnHost] == settingTrue {
		return fmt.Errorf("%w: the zone is on this machine or a private network, "+
			"and an existing cluster cannot connect to it", ErrExisting)
	}
	ranges, err := w.input("The cluster's pod and service ranges in CIDR form, comma-separated. "+
		"For example: 10.0.0.0/16,172.20.0.0/16",
		w.l.Settings[KeyClusterCIDRs], checkCIDRs)
	if err != nil {
		return err
	}
	w.env[KeyKubeContext], w.env[KeyClusterCIDRs] = kubeContext, ranges
	return nil
}

// kubeContexts lists the contexts in the kubeconfig.
func (l *Local) kubeContexts(ctx context.Context) ([]string, error) {
	cmd, err := l.Tool(ctx, "kubectl", "config", "get-contexts", "--output=name")
	if err != nil {
		return nil, err
	}
	out, err := pinned.Run(cmd, nil)
	if err != nil {
		return nil, fmt.Errorf("listing the kubeconfig contexts: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// chooseIssuer asks how to publish the cluster's issuer.
func (w *wizard) chooseIssuer(context.Context) error {
	value := issuerQuick
	switch {
	case w.l.Settings[KeyTunnel] != "":
		value = issuerNamed
	case w.l.Settings[KeyIssuer] != "":
		value = issuerPublished
	}
	choice, err := w.choose("How does the zone reach the cluster's issuer?", []Option{
		{
			Label: "Cloudflare quick tunnel. No account needed. Each start gets a new URL and needs a new cluster.",
			Value: issuerQuick,
		},
		{Label: "Named Cloudflare tunnel in your account, with a fixed URL", Value: issuerNamed},
		{Label: "Your own HTTPS URL that forwards to sandbox mirror", Value: issuerPublished},
	}, value)
	if err != nil {
		return err
	}
	switch choice {
	case issuerNamed:
		name, err := w.input("Tunnel name", w.l.Settings[KeyTunnel], checkTunnelName)
		if err != nil {
			return err
		}
		issuerURL, err := w.input("Tunnel hostname, as https://<hostname>", w.l.Settings[KeyIssuer], checkHostIssuer)
		if err != nil {
			return err
		}
		w.env[KeyTunnel], w.env[KeyIssuer] = name, issuerURL
	case issuerPublished:
		issuerURL, err := w.input("Issuer URL", w.l.Settings[KeyIssuer], checkClusterIssuer)
		if err != nil {
			return err
		}
		w.env[KeyIssuer] = issuerURL
	}
	return nil
}

// askReplaceActive asks whether `sandbox run` may replace another active
// policy set. A zone has one active policy set.
func (w *wizard) askReplaceActive(ctx context.Context) (bool, error) {
	active, err := w.zone.ActivePolicySets(ctx)
	if err != nil {
		return false, fmt.Errorf("listing the zone's active policy set: %w", err)
	}
	var others []string
	for _, set := range active {
		if set.Name != zonesetup.DefaultPolicySetName {
			others = append(others, fmt.Sprintf("%q", set.Name))
		}
	}
	if len(others) == 0 {
		return false, nil
	}
	return w.confirm(fmt.Sprintf("The zone's active policy set is %s. Only one set can be active. "+
		"Replace it with %q when the sandbox runs? Its policies will stop applying to everyone in the zone.",
		strings.Join(others, ", "), zonesetup.DefaultPolicySetName), false)
}

// write writes the values file, local/answers.yaml when there are answers,
// and local/local.env.
func (w *wizard) write() error {
	raw, err := yaml.Marshal(w.out)
	if err != nil {
		return fmt.Errorf("encoding the values: %w", err)
	}
	header := "# Written by `go run ./cmd/sandbox init`. zone-setup.example.yaml documents\n" +
		"# each key. `sandbox run` sets issuer_url to the cluster's issuer.\n"
	if err = os.WriteFile(w.l.valuesPath(), append([]byte(header), raw...), fileMode); err != nil {
		return fmt.Errorf("writing %s: %w", w.l.valuesPath(), err)
	}
	_, _ = fmt.Fprintf(w.l.Out, "Wrote %s.\n", w.l.valuesName())
	if len(w.answers) > 0 {
		if raw, err = yaml.Marshal(w.answers); err != nil {
			return fmt.Errorf("encoding the answers: %w", err)
		}
		header = "# Written by `go run ./cmd/sandbox init`. The answers to the scenarios'\n" +
			"# questions that are not secrets, by input id.\n"
		path := filepath.Join(w.l.Root, answersFile)
		if err = os.WriteFile(path, append([]byte(header), raw...), fileMode); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		_, _ = fmt.Fprintf(w.l.Out, "Wrote %s.\n", answersFile)
	}
	w.env[KeyCluster] = cmp.Or(w.l.Settings[KeyCluster], defaultCluster)
	w.env[KeyZoneSetupValues] = zoneSetupFile
	w.env[KeyGitHubLogin] = w.l.Settings[KeyGitHubLogin]
	return w.l.writeSettings(w.env)
}

// Initialized reports whether local/local.env and the values file both
// exist.
func (l *Local) Initialized() bool {
	for _, path := range []string{filepath.Join(l.Root, "local", "local.env"), l.valuesPath()} {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}

// writeSettings writes local/local.env from local/local.env.example, with
// each key set to its value in settings.
func (l *Local) writeSettings(settings Settings) error {
	example, err := os.ReadFile(filepath.Join(l.Root, "local", "local.env.example"))
	if err != nil {
		return fmt.Errorf("reading local.env.example: %w", err)
	}
	var out bytes.Buffer
	lines := bufio.NewScanner(bytes.NewReader(example))
	for lines.Scan() {
		line := lines.Text()
		if key, _, ok := strings.Cut(line, "="); ok && slices.Contains(settingKeys(), key) {
			line = key + "=" + settings[key]
		}
		out.WriteString(line + "\n")
	}
	path := filepath.Join(l.Root, "local", "local.env")
	if err = os.WriteFile(path, out.Bytes(), fileMode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	_, _ = fmt.Fprintln(l.Out, "Wrote local/local.env.")
	return nil
}

// writeSecret writes secret to local/out/secrets/name, which only the owner
// can read, and returns its path.
func (l *Local) writeSecret(name, secret string) (string, error) {
	dir := filepath.Join(l.outDir(), "secrets")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(secret+"\n"), fileMode); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(l.Out, "Wrote local/out/secrets/%s, readable only by you.\n", name)
	return path, nil
}

// valuesName returns the values file's path relative to the repository root.
func (l *Local) valuesName() string {
	return cmp.Or(l.Settings[KeyZoneSetupValues], zoneSetupFile)
}

func (l *Local) valuesPath() string {
	return filepath.Join(l.Root, l.valuesName())
}

// previousValues reads the values file Init replaces. It skips the
// ParseValues checks, because Init only takes defaults from it.
func (l *Local) previousValues() previous {
	_, envErr := os.Stat(filepath.Join(l.Root, "local", "local.env"))
	raw, err := os.ReadFile(l.valuesPath())
	prev := previous{exists: envErr == nil || err == nil}
	if err == nil {
		_ = yaml.Unmarshal(raw, &prev.values)
	}
	return prev
}

// onHost reports whether every address of the zone's host is a loopback or
// private address. The sandbox connects to such a zone through the Docker
// host.
func onHost(ctx context.Context, zoneURL string) bool {
	parsed, err := url.Parse(zoneURL)
	if err != nil {
		return false
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, parsed.Hostname())
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, addr := range addrs {
		if !addr.IP.IsLoopback() && !addr.IP.IsPrivate() {
			return false
		}
	}
	return true
}

func rootsFrom(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil //nolint:nilnil // No file: use the system roots.
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("%w: reading the system roots: %w", ErrCAFile, err)
	}
	pem, err := os.ReadFile(caFile) //nolint:gosec // G304: the path is the CA file the operator named.
	if err != nil || !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%w: %s does not hold a PEM certificate", ErrCAFile, caFile)
	}
	return roots, nil
}

// readSecret returns the trimmed content of path, or "" when it is empty or
// cannot be read.
func readSecret(path string) string {
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is one an earlier Init wrote.
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func required(answer string) error {
	if strings.TrimSpace(answer) == "" {
		return fmt.Errorf("%w: required", ErrAnswer)
	}
	return nil
}

func checkHTTPS(answer string) error {
	parsed, err := url.Parse(answer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("%w: enter an https URL", ErrAnswer)
	}
	return nil
}

func checkCAFile(answer string) error {
	_, err := (&Local{Settings: Settings{KeyCAFile: answer}}).caFile()
	return err
}

func checkTunnelName(answer string) error {
	if !tunnelName.MatchString(answer) {
		return fmt.Errorf("%w: use only letters, digits, and . _ -", ErrAnswer)
	}
	return nil
}

func checkCIDRs(answer string) error {
	ranges, err := ParseCIDRs(answer)
	if err != nil {
		return err
	}
	if len(ranges) == 0 {
		return fmt.Errorf("%w: required", ErrAnswer)
	}
	return nil
}

func checkHostIssuer(answer string) error {
	if !hostIssuer.MatchString(answer) {
		return fmt.Errorf("%w: enter https://<hostname>, with no port or path", ErrAnswer)
	}
	return nil
}
