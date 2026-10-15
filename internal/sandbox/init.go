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
	// zonePoll and zoneWait bound the wait for a new zone to become ready.
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
	// out starts as the previous values file. Each chosen section replaces
	// only the keys it asks about.
	out  zonesetup.Values
	org  *zonesetup.Org
	zone *zonesetup.Session
	// answers holds the text input answers of every scenario, by input id.
	answers map[string]string
}

// previous holds the values file from before Init. exists reports whether
// the settings file or the values file exists. err is the values file's parse
// error. When err is set, values is empty and exists is false.
type previous struct {
	values zonesetup.Values
	exists bool
	err    error
}

// Init asks the operator for the settings and the scenario's inputs, and
// writes the settings file, the zone-setup values file and
// local/answers.yaml. Secrets the operator types go in 0600 files under
// local/out/secrets, and the values file names them. Each question defaults
// to the value in the existing files. Init keeps the keys it does not ask
// about, such as namespace and policy_set.
//
// Init reads zones, users and providers from the management API. It writes
// to Keycard only to create a zone, when the operator asks for one. It does
// not change policies. It asks whether `sandbox run` may replace another active policy
// set, and returns the answer.
//
// Init asks with l.Ask, and returns ErrNoTerminal when l.Ask is nil. It
// writes the files after the last question, so an error before then leaves
// them unchanged.
func (l *Local) Init(ctx context.Context) (InitResult, error) {
	if l.Ask == nil {
		return InitResult{}, fmt.Errorf("%w. Run %s init in a terminal", ErrNoTerminal, sandboxCommand)
	}
	w := &wizard{l: l, ask: l.Ask, prev: l.previousValues(), env: Settings{}}
	w.out = w.prev.values
	change, err := w.chooseChanges(ctx)
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

// chooseChanges returns the sections to ask. On a first run, or when the
// values file does not parse, that is every section. When the settings files
// exist, it shows the current settings and asks what to change. If the answer
// is nothing, it returns an empty map and Init does not write the files.
func (w *wizard) chooseChanges(ctx context.Context) (map[string]bool, error) {
	all := map[string]bool{sectionAccount: true, sectionZone: true, sectionScenario: true, sectionCluster: true}
	if w.prev.err != nil {
		_, _ = fmt.Fprintf(w.l.Out, "%s does not parse: %v\nInit asks every question and replaces it.\n",
			w.l.valuesName(), w.prev.err)
	}
	if !w.prev.exists {
		return all, nil
	}
	_, _ = fmt.Fprintf(w.l.Out, "Your current settings, from %s and %s:\n\n%s\n",
		w.l.settingsName(), w.l.valuesName(), w.summary())
	choice, err := w.choose(ctx, "What do you want to change?", []Option{
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
		_, _ = fmt.Fprintf(w.l.Out, "Kept %s and %s as they are.\n", w.l.settingsName(), w.l.valuesName())
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
		switch {
		case resource.CredentialProvider != "":
			source = "provider " + resource.CredentialProvider
		case resource.SecretEnv != "":
			source = "environment variable " + resource.SecretEnv
		}
		credentials = append(credentials, cmp.Or(resource.Name, resource.Identifier)+" ("+source+")")
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

// keepAccount keeps the extra roots from local.env.
func (w *wizard) keepAccount() {
	w.env[KeyCAFile] = w.l.Settings[KeyCAFile]
}

// signInAgain signs in with the saved account. If there is no saved secret
// or the sign-in fails, it calls login.
func (w *wizard) signInAgain(ctx context.Context) error {
	w.keepAccount()
	account := w.out.ManagementServiceAccount
	secret := w.savedSecret(account)
	roots, err := rootsFrom(w.env[KeyCAFile])
	if err != nil || secret == "" || w.out.ManagementAPIURL == "" {
		return w.login(ctx)
	}
	w.roots = roots
	if w.org, err = zonesetup.Login(ctx, w.out.ManagementAPIURL, account.ClientID, secret, roots); err != nil {
		_, _ = fmt.Fprintf(w.l.Out, "Signing in with the saved account failed: %v\n", err)
		return w.login(ctx)
	}
	return nil
}

// savedSecret returns the service account secret from the file the values
// file names. With no file, it reads the environment variable the values
// file names, or KEYCARD_MANAGEMENT_CLIENT_SECRET.
func (w *wizard) savedSecret(account zonesetup.ManagementServiceAccountValues) string {
	if account.ClientSecretFile != "" {
		return readSecret(account.ClientSecretFile)
	}
	return w.getenv(cmp.Or(account.ClientSecretEnv, zonesetup.DefaultManagementSecretEnv))
}

// keepZone keeps the zone and the user settings from local.env. After a
// sign-in, it also opens a session on the zone for the sections that follow.
func (w *wizard) keepZone(ctx context.Context) error {
	for _, key := range []string{KeyZoneURL, KeyZoneOnHost, KeyUserEmail} {
		w.env[key] = w.l.Settings[key]
	}
	if w.org == nil {
		return nil
	}
	zone, err := w.org.Zone(ctx, &w.out)
	if err != nil {
		return fmt.Errorf("finding the saved zone: %w. Change the zone", err)
	}
	w.zone = w.org.Session(zone)
	return nil
}

// keepScenario keeps the scenario from local.env. The resources stay as the
// values file has them, and local/answers.yaml stays as it is.
func (w *wizard) keepScenario(context.Context) error {
	w.env[KeyScenario] = w.l.Settings[KeyScenario]
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

func (w *wizard) input(ctx context.Context, question, value string, check func(string) error) (string, error) {
	answer, err := w.ask.Input(ctx, question, value, check)
	if err != nil {
		return "", fmt.Errorf("answering: %w", err)
	}
	return answer, nil
}

func (w *wizard) secret(ctx context.Context, question string, check func(string) error) (string, error) {
	answer, err := w.ask.Secret(ctx, question, check)
	if err != nil {
		return "", fmt.Errorf("answering: %w", err)
	}
	return answer, nil
}

func (w *wizard) choose(ctx context.Context, question string, options []Option, value string) (string, error) {
	answer, err := w.ask.Choose(ctx, question, options, value)
	if err != nil {
		return "", fmt.Errorf("answering: %w", err)
	}
	return answer, nil
}

func (w *wizard) confirm(ctx context.Context, question string, value bool) (bool, error) {
	answer, err := w.ask.Confirm(ctx, question, value)
	if err != nil {
		return false, fmt.Errorf("answering: %w", err)
	}
	return answer, nil
}

// login asks for the management API URL, the extra trusted roots if the API
// is not production, and the service account, then signs in. It asks for the
// account again while the token endpoint refuses it. Any other error stops
// Init. A typed secret goes in a 0600 file once the sign-in succeeds.
func (w *wizard) login(ctx context.Context) error {
	var err error
	w.out.ManagementAPIURL, err = w.input(ctx, "Keycard management API URL",
		cmp.Or(w.prev.values.ManagementAPIURL, defaultManagementAPI), checkHTTPS)
	if err != nil {
		return err
	}
	if err = w.askRoots(ctx); err != nil {
		return err
	}
	clientID := w.prev.values.ManagementServiceAccount.ClientID
	useSaved := true
	for {
		account, secret, typed, err := w.askAccount(ctx, clientID, useSaved)
		if err != nil {
			return err
		}
		w.org, err = zonesetup.Login(ctx, w.out.ManagementAPIURL, account.ClientID, secret, w.roots)
		if errors.Is(err, zonesetup.ErrRefused) {
			_, _ = fmt.Fprintf(w.l.Out, "%v\nTry again.\n", err)
			clientID, useSaved = account.ClientID, false
			continue
		}
		if err != nil {
			return fmt.Errorf("signing in: %w", err)
		}
		if typed {
			if account.ClientSecretFile, err = w.l.writeSecret("management-client-secret", secret); err != nil {
				return err
			}
		}
		w.out.ManagementServiceAccount = account
		return nil
	}
}

// askRoots asks for the extra trusted roots. A non-production management
// API, such as localdev, may use a private CA. For production, it keeps the
// current setting without asking.
func (w *wizard) askRoots(ctx context.Context) error {
	caFile := w.l.Settings[KeyCAFile]
	if w.out.ManagementAPIURL != defaultManagementAPI {
		var err error
		caFile, err = w.input(ctx, "PEM file of extra trusted roots, as an absolute path. "+
			"Leave empty if this API's certificate comes from a public CA.",
			caFile, checkCAFile)
		if err != nil {
			return err
		}
	}
	w.env[KeyCAFile] = caFile
	roots, err := rootsFrom(caFile)
	w.roots = roots
	return err
}

// askAccount asks for the service account and returns it, its secret, and
// whether the operator typed the secret. When useSaved is true and the
// environment variable or the file the values file names holds a secret, it
// offers that secret.
func (w *wizard) askAccount(
	ctx context.Context,
	clientID string,
	useSaved bool,
) (zonesetup.ManagementServiceAccountValues, string, bool, error) {
	var account zonesetup.ManagementServiceAccountValues
	saved := w.prev.values.ManagementServiceAccount
	clientID, err := w.input(ctx, "Organization service account client ID", clientID, required)
	if err != nil {
		return account, "", false, err
	}
	account.ClientID = clientID
	secret := ""
	if useSaved {
		secret = w.savedSecret(saved)
	}
	switch {
	case secret != "" && saved.ClientSecretFile == "":
		_, _ = fmt.Fprintf(w.l.Out, "Using the client secret from %s.\n",
			cmp.Or(saved.ClientSecretEnv, zonesetup.DefaultManagementSecretEnv))
		account.ClientSecretEnv = saved.ClientSecretEnv
		return account, secret, false, nil
	case secret != "" && clientID == saved.ClientID:
		keep, keepErr := w.confirm(ctx, "Keep the saved client secret?", true)
		if keepErr != nil {
			return account, "", false, keepErr
		}
		if keep {
			account.ClientSecretFile = saved.ClientSecretFile
			return account, secret, false, nil
		}
	}
	secret, err = w.secret(ctx, "Service account client secret", required)
	return account, secret, true, err
}

// chooseZone asks for one of the zones the service account can see, or a
// new one.
func (w *wizard) chooseZone(ctx context.Context) error {
	zones, err := w.org.Zones(ctx)
	if err != nil {
		return fmt.Errorf("listing the zones: %w", err)
	}
	options, value := w.zoneOptions(zones)
	choice, err := w.choose(ctx, "Which zone does the sandbox use?", options, value)
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
	return w.askOnHost(ctx)
}

// askOnHost sets KEYCARD_LOCAL_ZONE_ON_HOST when the zone answers on this
// machine. A zone at a loopback address does. For a zone at a private
// address, it asks, because the address can also be on another machine.
func (w *wizard) askOnHost(ctx context.Context) error {
	loopback, private := zoneAddresses(ctx, w.out.ZoneURL)
	onHost := loopback
	if private && !loopback {
		var err error
		onHost, err = w.confirm(ctx, "The zone has a private address. Does it answer on this machine?",
			w.l.Settings[KeyZoneOnHost] != "")
		if err != nil {
			return err
		}
	}
	if onHost {
		w.env[KeyZoneOnHost] = settingTrue
		_, _ = fmt.Fprintln(w.l.Out, "The sandbox connects to the zone through the Docker host.")
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
	name, err := w.input(ctx, "Name of the new zone", "sandbox-demo", required)
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
		choice, err := w.choose(ctx, "Which user does the sandbox act for?", options, value)
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

// chooseScenario asks which scenario to run, then asks its inputs. It
// replaces the resources any scenario lists, and keeps the other resources
// in the values file.
func (w *wizard) chooseScenario(ctx context.Context) error {
	scenarios, err := Scenarios(w.l.Root)
	if err != nil {
		return err
	}
	options := make([]Option, 0, len(scenarios))
	listed := map[string]bool{}
	for _, scenario := range scenarios {
		options = append(options, Option{Label: scenario.Title + " (" + scenario.Name + ")", Value: scenario.Name})
		for _, resource := range scenario.Resources {
			listed[resource.Identifier] = true
		}
	}
	value := cmp.Or(w.l.Settings[KeyScenario], DefaultScenario)
	if !slices.ContainsFunc(scenarios, func(s *Scenario) bool { return s.Name == value }) {
		value = scenarios[0].Name
	}
	choice, err := w.choose(ctx, "Which demo?", options, value)
	if err != nil {
		return err
	}
	w.env[KeyScenario] = choice
	if w.answers, err = w.l.answers(); err != nil {
		return err
	}
	w.out.Resources = nil
	scenario := scenarios[slices.IndexFunc(scenarios, func(s *Scenario) bool { return s.Name == choice })]
	if err = w.askInputs(ctx, scenario); err != nil {
		return err
	}
	for _, resource := range w.prev.values.Resources {
		if !listed[resource.Identifier] {
			w.out.Resources = append(w.out.Resources, resource)
		}
	}
	return nil
}

// askInputs asks the scenario's inputs in order, and adds the scenario's
// resources whose credential the operator gave.
func (w *wizard) askInputs(ctx context.Context, scenario *Scenario) error {
	var err error
	credentials := map[string]zonesetup.ResourceValues{}
	for _, input := range scenario.Inputs {
		var credential zonesetup.ResourceValues
		switch input.Kind {
		case inputGitHub:
			credential, err = w.chooseGitHub(ctx, w.prevCredential(scenario, input.ID))
		case inputSecret:
			credential, err = w.askSecret(ctx, input, w.prevCredential(scenario, input.ID))
		default:
			w.answers[input.ID], err = w.input(ctx, input.Prompt, cmp.Or(w.answers[input.ID], input.Default), required)
		}
		if err != nil {
			return err
		}
		if credential != (zonesetup.ResourceValues{}) {
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

// prevCredential returns the previous values file's resource for the first
// of the scenario's resources whose credential is input id, or the zero
// value.
func (w *wizard) prevCredential(scenario *Scenario, id string) zonesetup.ResourceValues {
	for _, resource := range scenario.Resources {
		if resource.Credential != id {
			continue
		}
		if prev := w.prevResource(resource.Identifier); prev != (zonesetup.ResourceValues{}) {
			return prev
		}
	}
	return zonesetup.ResourceValues{}
}

// askSecret asks for a secret input and writes it to
// local/out/secrets/<id>. It offers to keep the source prev reads the
// secret from. For an input with a confirm question, a no returns an empty
// credential.
func (w *wizard) askSecret(
	ctx context.Context,
	input ScenarioInput,
	prev zonesetup.ResourceValues,
) (zonesetup.ResourceValues, error) {
	if input.Confirm != "" {
		add, err := w.confirm(ctx, input.Confirm, prev.Static())
		if err != nil || !add {
			return zonesetup.ResourceValues{}, err
		}
	}
	env, file, err := w.staticToken(ctx, prev, input.Prompt, input.ID)
	return zonesetup.ResourceValues{SecretEnv: env, SecretFile: file}, err
}

// chooseGitHub asks where the GitHub token comes from: a GitHub provider in
// the zone, which gives each user their own token, or a personal access
// token, which every user shares. prev is the previous answer.
func (w *wizard) chooseGitHub(ctx context.Context, prev zonesetup.ResourceValues) (zonesetup.ResourceValues, error) {
	providers, err := w.zone.Providers(ctx)
	if err != nil {
		return zonesetup.ResourceValues{}, fmt.Errorf("listing the zone's providers: %w", err)
	}
	options := w.gitHubOptions(providers)
	value := options[0].Value
	switch {
	case slices.ContainsFunc(options, func(o Option) bool { return o.Value == prev.CredentialProvider }):
		value = prev.CredentialProvider
	case prev.Static():
		value = gitHubToken
	}
	choice, err := w.choose(ctx, "How does the sandbox get a GitHub token?", options, value)
	if err != nil || choice != gitHubToken {
		return zonesetup.ResourceValues{CredentialProvider: choice}, err
	}
	env, file, err := w.staticToken(ctx, prev, "GitHub personal access token", githubTokenFile)
	return zonesetup.ResourceValues{SecretEnv: env, SecretFile: file}, err
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

// staticToken returns the secret_env and secret_file of a static token. When
// prev reads the token from an environment variable or a file that holds
// one, it asks whether to keep that source. Otherwise, or when the answer is
// no, it asks for the token and writes it to local/out/secrets/name.
func (w *wizard) staticToken(
	ctx context.Context,
	prev zonesetup.ResourceValues,
	question, name string,
) (string, string, error) {
	keep := ""
	switch {
	case prev.SecretEnv != "":
		keep = "Keep reading the " + question + " from " + prev.SecretEnv + "?"
	case readSecret(prev.SecretFile) != "":
		keep = "Keep the saved " + question + "?"
	}
	if keep != "" {
		yes, err := w.confirm(ctx, keep, true)
		if err != nil || yes {
			return prev.SecretEnv, prev.SecretFile, err
		}
	}
	secret, err := w.secret(ctx, question, required)
	if err != nil {
		return "", "", err
	}
	file, err := w.l.writeSecret(name, secret)
	return "", file, err
}

// prevResource returns the resource the previous values file has for
// identifier, or the zero value.
func (w *wizard) prevResource(identifier string) zonesetup.ResourceValues {
	for _, resource := range w.prev.values.Resources {
		if resource.Identifier == identifier {
			return resource
		}
	}
	return zonesetup.ResourceValues{}
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
	choice, err := w.choose(ctx, "Which cluster runs the sandbox?", options, value)
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
	ranges, err := w.input(ctx, "The cluster's pod and service ranges in CIDR form, comma-separated. "+
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
func (w *wizard) chooseIssuer(ctx context.Context) error {
	value := issuerQuick
	switch {
	case w.l.Settings[KeyTunnel] != "":
		value = issuerNamed
	case w.l.Settings[KeyIssuer] != "":
		value = issuerPublished
	}
	choice, err := w.choose(ctx, "How does the zone reach the cluster's issuer?", []Option{
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
		name, err := w.input(ctx, "Tunnel name", w.l.Settings[KeyTunnel], checkTunnelName)
		if err != nil {
			return err
		}
		issuerURL, err := w.input(ctx, "Tunnel hostname, as https://<hostname>", w.l.Settings[KeyIssuer], checkHostIssuer)
		if err != nil {
			return err
		}
		w.env[KeyTunnel], w.env[KeyIssuer] = name, issuerURL
	case issuerPublished:
		issuerURL, err := w.input(ctx, "Issuer URL", w.l.Settings[KeyIssuer], checkClusterIssuer)
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
	ours := cmp.Or(w.out.PolicySet, zonesetup.DefaultPolicySetName)
	var others []string
	for _, set := range active {
		if set.Name != ours {
			others = append(others, fmt.Sprintf("%q", set.Name))
		}
	}
	if len(others) == 0 {
		return false, nil
	}
	return w.confirm(ctx, fmt.Sprintf("The zone's active policy set is %s. Only one set can be active. "+
		"Replace it with %q when the sandbox runs? Its policies will stop applying to everyone in the zone.",
		strings.Join(others, ", "), ours), false)
}

// write writes the values file, local/answers.yaml when there are answers,
// and the settings file. It refuses a setting with a line break, which would
// add a line to local.env.
func (w *wizard) write() error {
	w.env[KeyCluster] = cmp.Or(w.l.Settings[KeyCluster], defaultCluster)
	w.env[KeyZoneSetupValues] = w.l.valuesName()
	w.env[KeyGitHubLogin] = w.l.Settings[KeyGitHubLogin]
	for _, key := range settingKeys() {
		if strings.ContainsAny(w.env[key], "\r\n") {
			return fmt.Errorf("%w: %s has a line break: %q", ErrSettings, key, w.env[key])
		}
	}
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
	return w.l.writeSettings(w.env)
}

// Initialized reports whether the settings file and the values file both
// exist.
func (l *Local) Initialized() bool {
	for _, path := range []string{l.settingsPath(), l.valuesPath()} {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}

// writeSettings writes the settings file from local/local.env.example, with
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
	if err = os.WriteFile(l.settingsPath(), out.Bytes(), fileMode); err != nil {
		return fmt.Errorf("writing %s: %w", l.settingsPath(), err)
	}
	_, _ = fmt.Fprintf(l.Out, "Wrote %s.\n", l.settingsName())
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

// settingsName returns the settings file's path as the operator named it, or
// local/local.env.
func (l *Local) settingsName() string {
	return cmp.Or(l.SettingsFile, filepath.Join("local", "local.env"))
}

func (l *Local) settingsPath() string {
	return cmp.Or(l.SettingsFile, filepath.Join(l.Root, "local", "local.env"))
}

// valuesName returns the values file's path relative to the repository root.
func (l *Local) valuesName() string {
	return cmp.Or(l.Settings[KeyZoneSetupValues], zoneSetupFile)
}

func (l *Local) valuesPath() string {
	return filepath.Join(l.Root, l.valuesName())
}

// previousValues reads the values file without the ParseValues checks.
func (l *Local) previousValues() previous {
	_, envErr := os.Stat(l.settingsPath())
	raw, err := os.ReadFile(l.valuesPath())
	prev := previous{exists: envErr == nil || err == nil}
	if err == nil {
		if prev.err = yaml.Unmarshal(raw, &prev.values); prev.err != nil {
			prev = previous{err: prev.err}
		}
	}
	return prev
}

// zoneAddresses reports whether every address of the zone's host is a
// loopback address, and whether every one is a loopback or private address.
func zoneAddresses(ctx context.Context, zoneURL string) (bool, bool) {
	parsed, err := url.Parse(zoneURL)
	if err != nil {
		return false, false
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, parsed.Hostname())
	if err != nil || len(addrs) == 0 {
		return false, false
	}
	loopback, private := true, true
	for _, addr := range addrs {
		loopback = loopback && addr.IP.IsLoopback()
		private = private && (addr.IP.IsLoopback() || addr.IP.IsPrivate())
	}
	return loopback, private
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

// readSecret returns the trimmed content of path, or "" when it is empty,
// relative or cannot be read. zone-setup refuses a relative secret file, so
// Init asks for that secret again.
func readSecret(path string) string {
	if !filepath.IsAbs(path) {
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
	ranges, bad := parseCIDRs(answer)
	if bad != "" {
		return fmt.Errorf("%w: %q is not an address range such as 10.0.0.0/16", ErrAnswer, bad)
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
