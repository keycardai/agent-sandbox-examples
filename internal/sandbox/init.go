package sandbox

import (
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/keycardai/keycard-go"

	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	defaultManagementAPI = "https://api.keycard.ai"
	defaultCluster       = "keycard-sandbox-local"
	// githubTokenSecret and managementSecret name the secrets of a GitHub
	// personal access token and of the service account.
	githubTokenSecret = "github-token"
	managementSecret  = "management-client-secret"
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

// EnvClientID and EnvClientSecret hold the organization service account.
// When they are set, init does not ask for the account, and zone-setup uses
// them instead of the account in the settings.
const (
	EnvClientID     = "KEYCARD_CLIENT_ID"
	EnvClientSecret = "KEYCARD_CLIENT_SECRET"
)

// ErrAnswer reports a missing or refused answer.
var ErrAnswer = errors.New("no answer")

// QuestionUser is the question Init asks to choose who the agent acts as.
const QuestionUser = "Who does the agent act as? Every demo has an agent for this person."

// InitResult holds the answers Init does not keep in the config.
type InitResult struct {
	// ReplaceActive reports whether the operator agreed to replace the
	// zone's active policy set.
	ReplaceActive bool
	// Saved reports whether the config file holds the settings.
	Saved bool
}

// wizard is the state of one Init.
type wizard struct {
	l     *Local
	ask   Asker
	prev  previous
	env   Settings
	roots *x509.CertPool
	// out starts as the previous values. Each chosen section replaces only
	// the keys it asks about.
	out  zonesetup.Values
	org  *zonesetup.Org
	zone *zonesetup.Session
	// answers holds the answers to text inputs, of every scenario. Its
	// Shared map is nil until the wizard reads the settings' answers.
	answers scenarioAnswers
	// demos holds, for each demo, the other people with an agent in it.
	demos map[string][]string
}

// previous holds the settings from before Init, as zone-setup's values,
// and the people with an agent. exists reports whether there were any.
type previous struct {
	values zonesetup.Values
	agents ConfigAgents
	exists bool
}

// valuesFile is zone-setup's values file, as the config gives it. Its keys
// are a subset of zonesetup.Values. A secret is named by the environment
// variable secretEnv makes.
type valuesFile struct {
	ManagementAPIURL string `yaml:"management_api_url"`
	ZoneID           string `yaml:"zone_id"`
	ZoneURL          string `yaml:"zone_url"`
	Account          struct {
		ClientID        string `yaml:"client_id"`
		ClientSecretEnv string `yaml:"client_secret_env,omitempty"`
	} `yaml:"management_service_account"`
	Users     []zonesetup.UserValues `yaml:"users"`
	Resources []resourceValues       `yaml:"resources,omitempty"`
}

type resourceValues struct {
	Identifier         string `yaml:"identifier"`
	Name               string `yaml:"name"`
	Prefix             bool   `yaml:"prefix"`
	CredentialProvider string `yaml:"credential_provider,omitempty"`
	SecretEnv          string `yaml:"secret_env,omitempty"`
}

// Init asks the operator for the settings and the scenario's inputs, and
// keeps them in the store as the config, with the secrets the operator
// types. It asks whether to save them: the config file then holds the
// settings, and the keychain the secrets. Each question defaults to the
// settings from before, if any.
//
// Init reads zones, users and providers from the management API. Its only
// write creates a zone, when the operator asks for one. It does not change
// policies. It asks whether `sandbox run` may replace another active policy
// set, and returns the answer.
//
// Init asks with l.Ask, and returns ErrNoTerminal when l.Ask is nil. It
// keeps the settings after the last question, so an error before then
// leaves them unchanged.
func (l *Local) Init(ctx context.Context) (InitResult, error) {
	if l.Ask == nil {
		return InitResult{Saved: l.Store.Saved()}, fmt.Errorf("%w. Run %s in a terminal", ErrNoTerminal,
			l.Command("init"))
	}
	w := &wizard{l: l, ask: l.Ask, prev: l.previousValues(), env: Settings{}}
	w.out = w.prev.values
	w.demos = maps.Clone(w.prev.agents.Demos)
	change, err := w.chooseChanges(ctx)
	switch {
	case err != nil:
		return InitResult{Saved: l.Store.Saved()}, err
	case len(change) == 0 && !w.missingAnswers():
		_, _ = fmt.Fprintln(l.Out, "Kept your settings as they are.")
		return InitResult{Saved: l.Store.Saved()}, nil
	}
	result, err := w.run(ctx, change)
	if err != nil || !result.Saved {
		return result, err
	}
	next := l.Command("up")
	if result.ReplaceActive {
		next += " --" + zonesetup.ReplaceActiveFlag
	}
	_, _ = fmt.Fprintf(l.Out, "\nTo start the sandbox, run:\n\n  %s\n", next)
	return result, nil
}

// run asks the sections in change, asks whether to replace the zone's
// active policy set, and keeps the settings.
func (w *wizard) run(ctx context.Context, change map[string]bool) (InitResult, error) {
	if err := w.runSections(ctx, change); err != nil {
		return InitResult{}, err
	}
	replace := false
	if w.zone != nil {
		var err error
		if replace, err = w.askReplaceActive(ctx); err != nil {
			return InitResult{}, err
		}
	}
	save, err := w.askSave(ctx)
	if err != nil {
		return InitResult{}, err
	}
	err = w.write(save)
	return InitResult{ReplaceActive: replace, Saved: w.l.Store.Saved()}, err
}

// QuestionSave is the question Init asks to save the settings.
const QuestionSave = "Save these settings for next time?"

// askSave asks whether to save the settings, when they are not saved yet.
// Settings saved before stay saved, with the changes. When the keychain does
// not work, it says so and does not ask.
func (w *wizard) askSave(ctx context.Context) (bool, error) {
	store := w.l.Store
	if err := store.KeychainErr(); err != nil {
		_, _ = fmt.Fprintf(w.l.Out, "Setup cannot save the settings, because %v.\n", err)
		return false, nil
	}
	switch {
	case !store.CanSave():
		return false, nil
	case store.Saved():
		return true, nil
	}
	return w.confirm(ctx, QuestionSave+" Yes saves them in "+HomeRelative(store.Path())+", and the secrets you typed in "+
		store.KeychainWhere()+". No keeps them until you quit.", true)
}

// The sections of the settings, in the order Init asks them.
const (
	SectionAccount   = "account"
	SectionZone      = "zone"
	SectionScenario  = "scenario"
	SectionCluster   = "cluster"
	changeEverything = "\x00everything"
	changeNothing    = "\x00nothing"
)

// chooseChanges returns the sections to ask. On a first run that is every
// section. When there are settings, it shows them and asks what to change.
// If the answer is nothing, it returns an empty map. Init then keeps the
// settings, and asks only the answers they lack.
func (w *wizard) chooseChanges(ctx context.Context) (map[string]bool, error) {
	all := allSections()
	if !w.prev.exists {
		return all, nil
	}
	_, _ = fmt.Fprintf(w.l.Out, "Your current settings:\n\n%s\n", w.summary())
	choice, err := w.choose(ctx, "What do you want to change?", []Option{
		{Label: "Nothing. Keep these settings.", Value: changeNothing},
		{Label: "Everything. Answer each question again, starting from these answers.", Value: changeEverything},
		{Label: "The Keycard account", Value: SectionAccount},
		{Label: "The zone, and who the agent acts as", Value: SectionZone},
		{Label: "The demo, its credentials and its settings", Value: SectionScenario},
		{Label: "The cluster, and how Keycard reaches it", Value: SectionCluster},
	}, changeNothing)
	switch {
	case err != nil:
		return nil, err
	case choice == changeNothing:
		return map[string]bool{}, nil
	case choice == changeEverything:
		return all, nil
	}
	return map[string]bool{choice: true}, nil
}

func allSections() map[string]bool {
	return map[string]bool{SectionAccount: true, SectionZone: true, SectionScenario: true, SectionCluster: true}
}

// summary describes the current settings, one line per section, in plain
// words.
func (w *wizard) summary() string {
	return fmt.Sprintf("  Keycard account          %s\n"+
		"  Zone and user            %s\n"+
		"  Demo and credentials     %s\n"+
		"  Cluster                  %s\n",
		w.accountSummary(), w.zoneSummary(), w.scenarioSummary(), w.clusterSummary())
}

// accountSummary names the Keycard API by its host, and the service
// account setup signs in with. The account in the environment wins.
func (w *wizard) accountSummary() string {
	values := w.prev.values
	account := "the service account " + values.ManagementServiceAccount.ClientID
	switch {
	case w.l.getenv(EnvClientID) != "":
		account = "the service account from " + EnvClientID
	case values.ManagementServiceAccount.ClientID == "":
		return "Not set up yet"
	}
	return hostOf(cmp.Or(values.ManagementAPIURL, defaultManagementAPI)) + ", with " + account
}

// zoneSummary names who the agent acts as, and the zone by its host. The
// settings keep no zone name.
func (w *wizard) zoneSummary() string {
	values := w.prev.values
	person := cmp.Or(w.prev.agents.Default, "No one picked yet")
	zone := cmp.Or(hostOf(values.ZoneURL), values.ZoneID)
	if zone == "" {
		return person + ", and no zone yet"
	}
	return person + ", in the zone at " + zone
}

// hostOf returns a URL's host, or the text as it is when it has none.
func hostOf(raw string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return raw
}

// scenarioSummary names the demo by its title, says where each of its
// credentials comes from, and names the demo's other people with an agent.
func (w *wizard) scenarioSummary() string {
	name := w.l.demo()
	demo := name
	if scenario, err := LoadScenario(w.l.Root, name); err == nil {
		demo = demoLabel(scenario)
	}
	credentials := make([]string, 0, len(w.prev.values.Resources))
	for _, resource := range w.prev.values.Resources {
		credentials = append(credentials, resource.Name+": "+credentialSource(resource))
	}
	summary := demo + ", with no credentials"
	if len(credentials) > 0 {
		summary = demo + ". " + strings.Join(credentials, ", ")
	}
	if others := w.prev.agents.Demos[name]; len(others) > 0 {
		summary += ". Agents also for " + strings.Join(others, ", ")
	}
	return summary
}

// credentialSource says where a resource's credential comes from.
func credentialSource(resource zonesetup.ResourceValues) string {
	switch {
	case resource.CredentialProvider != "":
		return "each person signs in (provider " + resource.CredentialProvider + ")"
	case secretName(resource.SecretEnv) != "":
		return "your key"
	case resource.SecretEnv != "":
		return "from " + resource.SecretEnv
	default:
		return "none"
	}
}

// clusterSummary says where the agent runs, and how Keycard reaches the
// cluster.
func (w *wizard) clusterSummary() string {
	settings := w.l.Settings
	local := "The local cluster " + cmp.Or(settings[KeyCluster], defaultCluster)
	switch {
	case settings[KeyKubeContext] != "":
		return "Your existing cluster " + settings[KeyKubeContext]
	case settings[KeyTunnel] != "":
		return local + ", reached through your Cloudflare tunnel " + settings[KeyTunnel] + " at " + settings[KeyIssuer]
	case settings[KeyIssuer] != "":
		return local + ", reached at " + settings[KeyIssuer]
	default:
		return local + ", reached through a Cloudflare quick tunnel"
	}
}

// runSections asks the chosen sections and keeps the others from the
// settings.
func (w *wizard) runSections(ctx context.Context, change map[string]bool) error {
	if err := w.account(ctx, change); err != nil {
		return err
	}
	for _, section := range []struct {
		name      string
		ask, keep func(context.Context) error
	}{
		{SectionZone, w.askZone, w.keepZone},
		{SectionScenario, w.chooseScenario, w.keepScenario},
		{SectionCluster, w.chooseCluster, w.keepCluster},
	} {
		run := section.keep
		if change[section.name] || (section.name == SectionScenario && w.zoneChanged()) {
			run = section.ask
		}
		if err := run(ctx); err != nil {
			return err
		}
	}
	return nil
}

// zoneChanged reports whether the operator chose another zone than the
// settings name. The demo's credentials can name the old zone's
// providers, so Init then asks the demo's questions again.
func (w *wizard) zoneChanged() bool {
	changed := w.out.ZoneID != w.prev.values.ZoneID || w.out.ZoneURL != strings.TrimRight(w.prev.values.ZoneURL, "/")
	if changed {
		_, _ = fmt.Fprintln(w.l.Out, "The zone changed. A demo's credentials can depend on the zone, "+
			"so confirm them for the new one.")
	}
	return changed
}

// account asks for the Keycard account when that section is chosen. When
// another chosen section needs Keycard, it signs in with the saved account.
// Otherwise it keeps the account from the settings.
func (w *wizard) account(ctx context.Context, change map[string]bool) error {
	switch {
	case change[SectionAccount]:
		return w.login(ctx)
	case change[SectionZone] || change[SectionScenario]:
		return w.signInAgain(ctx)
	default:
		w.keepAccount()
		return nil
	}
}

// askZone asks for the zone and who the agent acts as. The demos' other
// people with an agent are the old zone's users, so a new zone drops them.
func (w *wizard) askZone(ctx context.Context) error {
	if err := w.chooseZone(ctx); err != nil {
		return err
	}
	if w.out.ZoneID != w.prev.values.ZoneID && len(w.demos) > 0 {
		w.demos = nil
		_, _ = fmt.Fprintln(w.l.Out, "Removed the demos' other agents, which were for the old zone's users.")
	}
	return w.chooseUser(ctx)
}

// keepAccount keeps the extra roots from the settings.
func (w *wizard) keepAccount() {
	w.env[KeyCAFile] = w.l.Settings[KeyCAFile]
}

// envAccount reports whether the environment holds the whole account, so
// there is nothing to ask again when Keycard refuses it.
func (w *wizard) envAccount() bool {
	secret, _ := w.l.envSecret()
	return w.l.getenv(EnvClientID) != "" && secret != ""
}

// signInAgain signs in with the saved account. If there is no saved secret
// or the sign-in fails, it calls login.
func (w *wizard) signInAgain(ctx context.Context) error {
	w.keepAccount()
	account := w.out.ManagementServiceAccount
	envSecret, _ := w.l.envSecret()
	clientID := cmp.Or(w.l.getenv(EnvClientID), account.ClientID)
	secret := cmp.Or(envSecret, w.l.keptSecret(account.ClientSecretEnv))
	roots, err := rootsFrom(w.env[KeyCAFile])
	if err != nil || secret == "" || w.out.ManagementAPIURL == "" {
		return w.login(ctx)
	}
	w.roots = roots
	if w.org, err = zonesetup.Login(ctx, w.out.ManagementAPIURL, clientID, secret, roots); err != nil {
		_, _ = fmt.Fprintf(w.l.Out, "Signing in with the saved account failed: %v\n", err)
		return w.login(ctx)
	}
	return nil
}

// keepZone keeps the zone and the people from the settings. After a
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

// keepScenario keeps the demo, its resources and its answers from the
// settings. It asks the demo's per-user inputs of anyone without an answer,
// such as someone just given an agent.
func (w *wizard) keepScenario(ctx context.Context) error {
	w.env[KeyScenario] = w.l.Settings[KeyScenario]
	w.answers = w.l.answers()
	scenario, err := w.l.scenario()
	if err != nil {
		return fmt.Errorf("%w. Change the demo to fix it", err)
	}
	return w.askPerUserInputs(ctx, scenario, true)
}

// keepCluster keeps the cluster and the issuer settings.
func (w *wizard) keepCluster(context.Context) error {
	for _, key := range []string{KeyIssuer, KeyTunnel, KeyKubeContext, KeyImageTag, KeyClusterCIDRs} {
		w.env[key] = w.l.Settings[key]
	}
	return nil
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
// is not production, and the service account, then signs in.
func (w *wizard) login(ctx context.Context) error {
	var err error
	w.out.ManagementAPIURL, err = w.input(ctx, "Keycard API URL",
		cmp.Or(w.prev.values.ManagementAPIURL, defaultManagementAPI), checkHTTPS)
	if err != nil {
		return err
	}
	if err = w.askRoots(ctx); err != nil {
		return err
	}
	return w.signIn(ctx)
}

// signIn asks for the service account and signs in. It asks for the account
// again while the token endpoint refuses it, unless the environment holds
// the whole account. Any other error stops Init. A typed secret goes in the
// store once the sign-in succeeds.
func (w *wizard) signIn(ctx context.Context) error {
	clientID := w.prev.values.ManagementServiceAccount.ClientID
	for useSaved := true; ; useSaved = false {
		account, secret, typed, err := w.askAccount(ctx, clientID, useSaved)
		if err != nil {
			return err
		}
		w.org, err = zonesetup.Login(ctx, w.out.ManagementAPIURL, account.ClientID, secret, w.roots)
		refused := errors.Is(err, zonesetup.ErrRefused)
		switch {
		case refused && useSaved && w.envAccount():
			_, key := w.l.envSecret()
			return fmt.Errorf("signing in with %s and %s: %w", EnvClientID, key, err)
		case refused:
			_, _ = fmt.Fprintf(w.l.Out, "%v\nTry again.\n", err)
			clientID = account.ClientID
			continue
		case err != nil:
			return fmt.Errorf("signing in: %w", err)
		}
		if typed {
			w.l.Store.setSecret(managementSecret, secret)
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
		caFile, err = w.input(ctx, "CA certificate file, if this Keycard API needs one. Leave it empty unless "+
			"the API uses a private certificate, as localdev does. Give the full path to a PEM file.",
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
// whether the operator typed the secret. KEYCARD_CLIENT_ID gives the client
// ID without asking. When useSaved is true, the secret comes from
// KEYCARD_CLIENT_SECRET or KEYCARD_MANAGEMENT_CLIENT_SECRET, or the
// operator can keep the saved one.
func (w *wizard) askAccount(
	ctx context.Context,
	clientID string,
	useSaved bool,
) (zonesetup.ManagementServiceAccountValues, string, bool, error) {
	var account zonesetup.ManagementServiceAccountValues
	saved := w.prev.values.ManagementServiceAccount
	if env := w.l.getenv(EnvClientID); env != "" {
		_, _ = fmt.Fprintf(w.l.Out, "Using the client ID from %s.\n", EnvClientID)
		clientID = env
	} else {
		var err error
		if clientID, err = w.input(ctx, "Service account client ID", clientID, required); err != nil {
			return account, "", false, err
		}
	}
	account.ClientID = clientID
	if secret, key := w.l.envSecret(); useSaved && secret != "" {
		_, _ = fmt.Fprintf(w.l.Out, "Using the client secret from %s.\n", key)
		return account, secret, false, nil
	}
	if secret := w.l.keptSecret(saved.ClientSecretEnv); useSaved && secret != "" && clientID == saved.ClientID {
		keep, err := w.confirm(ctx, "Keep the saved client secret?", true)
		if err != nil || keep {
			account.ClientSecretEnv = saved.ClientSecretEnv
			return account, secret, false, err
		}
	}
	secret, err := w.secret(ctx, "Service account client secret", required)
	account.ClientSecretEnv = secretEnv(managementSecret)
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
// default is the zone the settings name, or else the first zone.
// Each zone shows by its name, with its URL only when another zone has the
// same name.
func (w *wizard) zoneOptions(zones []keycard.Zone) ([]Option, string) {
	options := make([]Option, 0, len(zones)+1)
	value := newZone
	if len(zones) > 0 {
		value = zones[0].ID
	}
	named := map[string]int{}
	for _, zone := range zones {
		named[zone.Name]++
	}
	for _, zone := range zones {
		issuer := zone.Protocols.Oauth2.Issuer
		label := cmp.Or(zone.Name, issuer)
		if named[zone.Name] > 1 {
			label += "  " + issuer
		}
		options = append(options, Option{Label: label, Value: zone.ID})
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

// chooseUser asks who the agent acts as: one of the zone's users. Every
// demo has an agent for them, and the commands for one agent act on it. The
// management API cannot create users, so chooseUser lists the zone's users
// again until the operator picks one.
func (w *wizard) chooseUser(ctx context.Context) error {
	for {
		users, err := w.zone.Users(ctx)
		if err != nil {
			return fmt.Errorf("listing the zone's users: %w", err)
		}
		choice, err := w.choose(ctx, QuestionUser, w.userOptions(users), userDefault(users, w.l.Settings[KeyUserEmail]))
		if err != nil {
			return err
		}
		if choice != checkAgain {
			w.env[KeyUserEmail] = choice
			return nil
		}
	}
}

// userOptions returns an option for each of the zone's users, and one to
// list them again.
func (w *wizard) userOptions(users []keycard.User) []Option {
	options := make([]Option, 0, len(users)+1)
	for _, user := range users {
		options = append(options, Option{Label: user.Email, Value: user.Email})
	}
	if len(users) == 0 {
		_, _ = fmt.Fprintln(w.l.Out, "The zone has no users, and init cannot create one. "+
			"Add the user in the Keycard console, or have them sign in to the zone. Then check again.")
	}
	return append(options, Option{Label: "Not in the list? Pick this to check again", Value: checkAgain})
}

// userDefault is who the list starts at: the person picked before while
// the zone still has them, else the first user, else the option to check
// again.
func userDefault(users []keycard.User, picked string) string {
	switch {
	case slices.ContainsFunc(users, func(user keycard.User) bool { return user.Email == picked }):
		return picked
	case len(users) > 0:
		return users[0].Email
	}
	return checkAgain
}

// people returns who has an agent in the demo the wizard asks about.
func (w *wizard) people() []string {
	agents := ConfigAgents{Default: w.env[KeyUserEmail], Demos: w.demos}
	return agents.people(w.demo())
}

// demo returns the name of the demo the wizard asks about.
func (w *wizard) demo() string {
	return cmp.Or(w.env[KeyScenario], w.l.demo())
}

// chooseScenario asks which scenario to run, then asks its inputs. It
// replaces the resources any scenario lists, and keeps the other resources
// in the settings.
func (w *wizard) chooseScenario(ctx context.Context) error {
	scenarios, err := Scenarios(w.l.Root)
	if err != nil {
		return err
	}
	value := cmp.Or(w.l.Settings[KeyScenario], DefaultScenario)
	if !slices.ContainsFunc(scenarios, func(s *Scenario) bool { return s.Name == value }) {
		value = scenarios[0].Name
	}
	choice, err := w.choose(ctx, QuestionDemo, demoOptions(scenarios, value), value)
	if err != nil {
		return err
	}
	w.env[KeyScenario] = choice
	w.answers = w.l.answers()
	w.out.Resources = nil
	scenario := scenarios[slices.IndexFunc(scenarios, func(s *Scenario) bool { return s.Name == choice })]
	if err = w.askInputs(ctx, scenario); err != nil {
		return err
	}
	listed := listedResources(scenarios)
	for _, resource := range w.prev.values.Resources {
		if !listed[resource.Identifier] {
			w.out.Resources = append(w.out.Resources, resource)
		}
	}
	return nil
}

// listedResources returns the identifiers of the resources any of the
// scenarios lists.
func listedResources(scenarios []*Scenario) map[string]bool {
	listed := map[string]bool{}
	for _, scenario := range scenarios {
		for _, resource := range scenario.Resources {
			listed[resource.Identifier] = true
		}
	}
	return listed
}

// demoOptions lists the demos with the default first, so that the list
// starts at the top, and the rest by name.
func demoOptions(scenarios []*Scenario, value string) []Option {
	options := make([]Option, 0, len(scenarios))
	for _, scenario := range scenarios {
		option := Option{Label: demoLabel(scenario), Value: scenario.Name}
		if scenario.Name == value {
			options = append([]Option{option}, options...)
			continue
		}
		options = append(options, option)
	}
	return options
}

// demoLabel names a demo in the list: its title, then its name in
// brackets when the title does not say it.
func demoLabel(scenario *Scenario) string {
	words := strings.FieldsFunc(strings.ToLower(scenario.Title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if slices.Contains(words, scenario.Name) {
		return scenario.Title
	}
	return scenario.Title + " (" + scenario.Name + ")"
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
		case inputText:
			if input.PerUser {
				err = w.askPerUser(ctx, input, false)
				break
			}
			w.answers.Shared[input.ID], err = w.input(ctx, input.Prompt, cmp.Or(w.answers.Shared[input.ID], input.Default),
				required)
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

// askPerUserInputs asks the scenario's per-user inputs, as askPerUser does.
func (w *wizard) askPerUserInputs(ctx context.Context, scenario *Scenario, missingOnly bool) error {
	for _, input := range scenario.Inputs {
		if !input.PerUser {
			continue
		}
		if err := w.askPerUser(ctx, input, missingOnly); err != nil {
			return err
		}
	}
	return nil
}

// askPerUser asks a per-user input once for each person with an agent,
// starting from their saved answer. With missingOnly, it asks only those
// without one.
func (w *wizard) askPerUser(ctx context.Context, input ScenarioInput, missingOnly bool) error {
	for _, email := range w.people() {
		saved := w.answers.Users[email][input.ID]
		answer := saved
		if !missingOnly || saved == "" {
			var err error
			prompt := strings.ReplaceAll(input.Prompt, userRef, email)
			if answer, err = w.input(ctx, prompt, cmp.Or(saved, input.Default), required); err != nil {
				return err
			}
		}
		w.answers.set(email, input.ID, answer)
	}
	return nil
}

// missingAnswers reports whether a person with an agent in the settings'
// demo has no answer, and no default, for one of its per-user inputs.
func (w *wizard) missingAnswers() bool {
	scenario, err := w.l.scenario()
	if err != nil {
		return false
	}
	answers := w.l.answers()
	for _, input := range scenario.Inputs {
		for _, email := range w.prev.agents.people(w.l.demo()) {
			if input.PerUser && input.Default == "" && answers.Users[email][input.ID] == "" {
				return true
			}
		}
	}
	return false
}

// prevCredential returns the settings' resource for the first of the
// scenario's resources whose credential is input id, or the zero value.
func (w *wizard) prevCredential(scenario *Scenario, id string) zonesetup.ResourceValues {
	for _, resource := range scenario.Resources {
		if resource.Credential != id {
			continue
		}
		for _, prev := range w.prev.values.Resources {
			if prev.Identifier == resource.Identifier {
				return prev
			}
		}
	}
	return zonesetup.ResourceValues{}
}

// askSecret asks for a secret input, and keeps it as the input's id. It
// offers to keep where prev reads the secret from. For an input with a
// confirm question, a no returns an empty credential.
func (w *wizard) askSecret(
	ctx context.Context,
	input ScenarioInput,
	prev zonesetup.ResourceValues,
) (zonesetup.ResourceValues, error) {
	if input.Confirm != "" {
		add, err := w.confirm(ctx, input.Confirm, prev.Static() || w.l.keptSecret(secretEnv(input.ID)) != "")
		if err != nil || !add {
			return zonesetup.ResourceValues{}, err
		}
	}
	env, err := w.staticToken(ctx, prev, input.Prompt, input.ID)
	return zonesetup.ResourceValues{SecretEnv: env}, err
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
	// Without a GitHub provider, a token is the only way, so there is
	// nothing to choose.
	choice := gitHubToken
	if len(options) > 1 {
		if choice, err = w.choose(ctx, "How does the agent get into GitHub?", options, value); err != nil {
			return zonesetup.ResourceValues{}, err
		}
	}
	if choice != gitHubToken {
		return zonesetup.ResourceValues{CredentialProvider: choice}, nil
	}
	env, err := w.staticToken(ctx, prev, "GitHub personal access token", githubTokenSecret)
	return zonesetup.ResourceValues{SecretEnv: env}, err
}

// gitHubOptions returns a choice for each GitHub provider in the zone, then
// one for a personal access token.
func (w *wizard) gitHubOptions(providers []keycard.Provider) []Option {
	var options []Option
	for _, provider := range providers {
		if gitHubProvider(provider) {
			options = append(options, Option{
				Label: "Each person signs in with their own GitHub account (provider " + provider.Name + ")",
				Value: provider.Identifier,
			})
		}
	}
	if len(options) == 0 {
		_, _ = fmt.Fprintln(w.l.Out, "The zone has no GitHub provider. So the agent uses a personal access "+
			"token. To have each person sign in with their own GitHub account, add a GitHub provider in the "+
			"Keycard console and run setup again.")
	}
	return append(options, Option{
		Label: "One personal access token, shared by everyone",
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

// staticToken returns the secret_env of a static token. When prev reads the
// token from a variable of the operator's, or when this program keeps a
// secret as name, as from another demo, it asks whether to keep that
// source. Otherwise, or when the answer is no, it asks for the token and
// keeps it as name.
func (w *wizard) staticToken(
	ctx context.Context,
	prev zonesetup.ResourceValues,
	question, name string,
) (string, error) {
	source := cmp.Or(prev.SecretEnv, secretEnv(name))
	keep := ""
	switch {
	case secretName(source) == "":
		keep = "Keep reading the " + question + " from " + source + "?"
	case w.l.keptSecret(source) != "":
		keep = "Keep the saved " + question + "?"
	}
	if keep != "" {
		yes, err := w.confirm(ctx, keep, true)
		if err != nil || yes {
			return source, err
		}
	}
	secret, err := w.secret(ctx, question, required)
	if err != nil {
		return "", err
	}
	w.l.Store.setSecret(name, secret)
	return secretEnv(name), nil
}

// chooseCluster asks whether to create a local k3d cluster or use an
// existing one from the kubeconfig. It keeps the saved image tag.
func (w *wizard) chooseCluster(ctx context.Context) error {
	contexts, err := w.l.kubeContexts(ctx)
	if err != nil {
		return err
	}
	options := []Option{{Label: "On this machine, in a new local cluster", Value: newCluster}}
	for _, name := range contexts {
		options = append(options, Option{Label: "In your existing cluster " + name, Value: name})
	}
	value := cmp.Or(w.l.Settings[KeyKubeContext], newCluster)
	if !slices.Contains(contexts, value) {
		value = newCluster
	}
	choice, err := w.choose(ctx, "Where does the agent run?", options, value)
	if err != nil {
		return err
	}
	w.env[KeyImageTag] = w.l.Settings[KeyImageTag]
	if choice == newCluster {
		if err = w.askClusterName(ctx); err != nil {
			return err
		}
		return w.chooseIssuer(ctx)
	}
	return w.existingCluster(ctx, choice)
}

// QuestionClusterName is the question Init asks to name a new cluster.
const QuestionClusterName = "What should the new cluster be called?"

// askClusterName asks for the local cluster's name. A new name leaves the
// cluster of the old one running, so it says how to delete that one.
func (w *wizard) askClusterName(ctx context.Context) error {
	before := cmp.Or(w.l.Settings[KeyCluster], defaultCluster)
	name, err := w.input(ctx, QuestionClusterName+" Use lowercase letters, digits and dashes, at most 32.", before,
		checkClusterName)
	if err != nil {
		return err
	}
	w.env[KeyCluster] = name
	if exists, _ := w.l.clusterExists(ctx, before); name != before && exists {
		_, _ = fmt.Fprintf(w.l.Out, "The cluster %s keeps running. To delete it, run k3d cluster delete %s.\n",
			before, before)
	}
	return nil
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
	ranges, err := w.input(ctx, "Your cluster's pod and service IP ranges, comma-separated, such as "+
		"10.0.0.0/16,172.20.0.0/16. The agent cannot reach anything in them.",
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
	choice, err := w.choose(ctx, "How does Keycard reach the cluster? Keycard checks who the agent is with keys "+
		"the cluster publishes, so they need a public URL.", []Option{
		{
			Label: "A Cloudflare quick tunnel. No account needed, but each start gets a new URL and a new cluster.",
			Value: issuerQuick,
		},
		{Label: "A named Cloudflare tunnel in your account, with a fixed URL", Value: issuerNamed},
		{Label: "Your own HTTPS URL that forwards to the sandbox mirror", Value: issuerPublished},
	}, value)
	if err != nil {
		return err
	}
	switch choice {
	case issuerNamed:
		name, err := w.input(ctx, "Name of your Cloudflare tunnel", w.l.Settings[KeyTunnel], checkTunnelName)
		if err != nil {
			return err
		}
		issuerURL, err := w.input(ctx, "Your tunnel's URL, such as https://sandbox.example.com", w.l.Settings[KeyIssuer],
			checkHostIssuer)
		if err != nil {
			return err
		}
		w.env[KeyTunnel], w.env[KeyIssuer] = name, issuerURL
	case issuerPublished:
		issuerURL, err := w.input(ctx, "Your HTTPS URL, such as https://sandbox.example.com. It must forward to the "+
			"sandbox mirror.", w.l.Settings[KeyIssuer], checkClusterIssuer)
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
	current := strings.Join(others, ", ")
	return w.confirm(ctx, fmt.Sprintf("Turn on the sandbox's access rules in this zone? The zone uses the policy set "+
		"%s now. A zone uses one set at a time, so %s stops applying to everyone in the zone.", current, current),
		false)
}

// write keeps what setup asked in the store as the config, and saves it
// when save is set. When saving fails, it returns ErrNotSaved, and the
// store keeps the config for the session.
func (w *wizard) write(save bool) error {
	agents := ConfigAgents{Default: w.env[KeyUserEmail], Demos: w.demos}
	w.answers.keepUsers(agents.everyone())
	w.env[KeyCluster] = cmp.Or(w.env[KeyCluster], w.l.Settings[KeyCluster], defaultCluster)
	w.env[KeyGitHubLogin] = w.l.Settings[KeyGitHubLogin]
	store := w.l.Store
	if store == nil {
		return fmt.Errorf("%w: there is no store for the settings", ErrSettings)
	}
	store.setConfig(configFrom(w.env, &w.out, w.answers, w.demos))
	if !save {
		_, _ = fmt.Fprintln(w.l.Out, "Kept the settings for this session. Nothing was saved.")
		return nil
	}
	if err := store.save(); err != nil {
		return fmt.Errorf("%w: %w", ErrNotSaved, err)
	}
	_, _ = fmt.Fprintf(w.l.Out, "Saved the settings in %s, and the secrets in %s.\n", HomeRelative(store.Path()),
		store.KeychainWhere())
	return nil
}

// Initialized reports whether the steps have their settings: the store has
// a config, or KEYCARD_LOCAL_ZONE_SETUP_VALUES names a zone-setup values
// file.
func (l *Local) Initialized() bool {
	return l.config() != nil || l.Settings[KeyZoneSetupValues] != ""
}

// previousValues returns the config as zone-setup's values, for the
// defaults of setup's questions.
func (l *Local) previousValues() previous {
	config := l.config()
	if config == nil {
		return previous{}
	}
	return previous{values: config.zoneSetupValues(l.demo()), agents: config.Agents, exists: true}
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

func checkClusterName(answer string) error {
	if len(answer) > maxClusterNameBytes || !clusterName.MatchString(answer) {
		return fmt.Errorf("%w: use at most 32 lowercase letters, digits and dashes, "+
			"starting and ending with a letter or digit", ErrAnswer)
	}
	return nil
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
