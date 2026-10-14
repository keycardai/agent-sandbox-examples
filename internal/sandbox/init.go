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

	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	defaultManagementAPI = "https://api.keycard.ai"
	defaultCluster       = "keycard-sandbox-local"
	zoneSetupFile        = "zone-setup.yaml"
	githubAPI            = "https://api.github.com"
	githubGit            = "https://github.com"
	openRouterAPI        = "https://openrouter.ai/api/v1"
	// zonePoll and zoneWait bound the wait for a new zone. The policy
	// engine caches a 404 for an unknown zone for about a minute.
	zonePoll = 5 * time.Second
	zoneWait = 2 * time.Minute
	// Choice values that cannot match a zone ID, user email, provider
	// identifier or kubeconfig context.
	newZone    = "\x00new"
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
}

// previous holds the values file from before Init. exists reports whether
// local/local.env or the values file exists.
type previous struct {
	values zonesetup.Values
	exists bool
}

// Init asks the operator for the settings and writes local/local.env and the
// zone-setup values file. Secrets the operator types go in 0600 files under
// local/out/secrets, and the values file names them. Each question defaults
// to the value in the existing files. Init keeps the keys it does not ask
// about, such as namespace and policy_set.
//
// Init reads zones, users and providers from the management API. Its only
// write creates a zone, when the operator asks for one. It does not change
// policies. It asks whether `sandbox run` may replace another active policy
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
	sectionAccount     = "account"
	sectionZone        = "zone"
	sectionCredentials = "credentials"
	sectionCluster     = "cluster"
	changeEverything   = "\x00everything"
	changeNothing      = "\x00nothing"
)

// chooseChanges returns the sections to ask. On a first run that is every
// section. When the settings files exist, it shows the current settings and
// asks what to change. If the answer is nothing, it returns an empty map and
// Init does not write the files.
func (w *wizard) chooseChanges(ctx context.Context) (map[string]bool, error) {
	all := map[string]bool{sectionAccount: true, sectionZone: true, sectionCredentials: true, sectionCluster: true}
	if !w.prev.exists {
		return all, nil
	}
	_, _ = fmt.Fprintf(w.l.Out, "Your current settings, from local/local.env and %s:\n\n%s\n",
		w.l.valuesName(), w.summary())
	choice, err := w.choose(ctx, "What do you want to change?", []Option{
		{Label: "Nothing. Keep these settings.", Value: changeNothing},
		{Label: "Everything. Answer each question again, starting from these answers.", Value: changeEverything},
		{Label: "The Keycard account", Value: sectionAccount},
		{Label: "The zone and the user", Value: sectionZone},
		{Label: "The GitHub token and the OpenRouter key", Value: sectionCredentials},
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
		"  GitHub and OpenRouter    %s\n"+
		"  Cluster and issuer       %s\n",
		cmp.Or(values.ManagementServiceAccount.ClientID, "none"), cmp.Or(values.ManagementAPIURL, "none"),
		user, cmp.Or(values.ZoneURL, values.ZoneID, "no zone"),
		w.credentialsSummary(), w.clusterSummary())
}

func (w *wizard) credentialsSummary() string {
	var credentials []string
	for _, resource := range w.prev.values.Resources {
		switch {
		case resource.Identifier == githubAPI && resource.CredentialProvider != "":
			credentials = append(credentials, "GitHub token from the provider "+resource.CredentialProvider)
		case resource.Identifier == githubAPI:
			credentials = append(credentials, "GitHub personal access token")
		case resource.Identifier == openRouterAPI:
			credentials = append(credentials, "OpenRouter API key")
		}
	}
	return cmp.Or(strings.Join(credentials, ", "), "none")
}

func (w *wizard) clusterSummary() string {
	settings := w.l.Settings
	k3d := "a local k3d cluster, " + cmp.Or(settings[KeyCluster], defaultCluster) + ", with "
	switch {
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
		{sectionCredentials, w.chooseResources, nil},
		{sectionCluster, w.chooseIssuer, w.keepCluster},
	} {
		run := section.keep
		if change[section.name] {
			run = section.ask
		}
		if run == nil {
			continue
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
	case change[sectionZone] || change[sectionCredentials]:
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

// keepCluster keeps the cluster and the issuer settings from local.env.
func (w *wizard) keepCluster(context.Context) error {
	for _, key := range []string{KeyIssuer, KeyTunnel} {
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
	if onHost(ctx, w.out.ZoneURL) {
		w.env[KeyZoneOnHost] = "true"
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

// chooseResources asks for the credentials of the demo's endpoints in
// sandbox/values.example.yaml: a GitHub token, and an optional OpenRouter
// API key. It keeps the other resources in the values file.
func (w *wizard) chooseResources(ctx context.Context) error {
	w.out.Resources = nil
	if err := w.chooseGitHub(ctx); err != nil {
		return err
	}
	if err := w.chooseOpenRouter(ctx); err != nil {
		return err
	}
	for _, resource := range w.prev.values.Resources {
		if !slices.Contains([]string{githubAPI, githubGit, openRouterAPI}, resource.Identifier) {
			w.out.Resources = append(w.out.Resources, resource)
		}
	}
	return nil
}

// chooseGitHub asks where the GitHub token comes from: a GitHub provider in
// the zone, which gives each user their own token, or a personal access
// token, which every user shares. The demo's check calls GitHub, so GitHub
// is not optional.
func (w *wizard) chooseGitHub(ctx context.Context) error {
	providers, err := w.zone.Providers(ctx)
	if err != nil {
		return fmt.Errorf("listing the zone's providers: %w", err)
	}
	options := w.gitHubOptions(providers)
	prev := w.prevResource(githubAPI)
	value := options[0].Value
	switch {
	case slices.ContainsFunc(options, func(o Option) bool { return o.Value == prev.CredentialProvider }):
		value = prev.CredentialProvider
	case prev.Static():
		value = gitHubToken
	}
	choice, err := w.choose(ctx, "How does the sandbox get a GitHub token?", options, value)
	if err != nil {
		return err
	}
	api := zonesetup.ResourceValues{Identifier: githubAPI, Name: "GitHub", Prefix: true, CredentialProvider: choice}
	git := zonesetup.ResourceValues{Identifier: githubGit, Name: "GitHub git", Prefix: true, CredentialProvider: choice}
	if choice == gitHubToken {
		env, file, err := w.staticToken(ctx, prev, "GitHub personal access token", "github-token")
		if err != nil {
			return err
		}
		api.CredentialProvider, api.SecretEnv, api.SecretFile = "", env, file
		git.CredentialProvider, git.SecretEnv, git.SecretFile = "", env, file
	}
	w.out.Resources = append(w.out.Resources, api, git)
	return nil
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

func (w *wizard) chooseOpenRouter(ctx context.Context) error {
	prev := w.prevResource(openRouterAPI)
	add, err := w.confirm(ctx, "Add an OpenRouter API key?", prev.Static())
	if err != nil || !add {
		return err
	}
	resource := zonesetup.ResourceValues{Identifier: openRouterAPI, Name: "OpenRouter", Prefix: true}
	resource.SecretEnv, resource.SecretFile, err = w.staticToken(ctx, prev, "OpenRouter API key", "openrouter-api-key")
	if err != nil {
		return err
	}
	w.out.Resources = append(w.out.Resources, resource)
	return nil
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
	var others []string
	for _, set := range active {
		if set.Name != zonesetup.DefaultPolicySetName {
			others = append(others, fmt.Sprintf("%q", set.Name))
		}
	}
	if len(others) == 0 {
		return false, nil
	}
	return w.confirm(ctx, fmt.Sprintf("The zone's active policy set is %s. Only one set can be active. "+
		"Replace it with %q when the sandbox runs? Its policies will stop applying to everyone in the zone.",
		strings.Join(others, ", "), zonesetup.DefaultPolicySetName), false)
}

// write writes the values file and local/local.env.
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
	w.env[KeyCluster] = cmp.Or(w.l.Settings[KeyCluster], defaultCluster)
	w.env[KeyZoneSetupValues] = w.l.valuesName()
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

func checkHostIssuer(answer string) error {
	if !hostIssuer.MatchString(answer) {
		return fmt.Errorf("%w: enter https://<hostname>, with no port or path", ErrAnswer)
	}
	return nil
}
