// Command zone-setup configures a Keycard zone for the sandboxes from a values
// file. It finds or creates the provider for the cluster's OIDC issuer and the
// egress proxy Application with implicit consent, finds, creates or patches
// each resource, stores each secret_env credential, and makes each resource a
// dependency of the Application. For each user it binds the user's
// ServiceAccount to the Application with a token credential, and publishes the
// user's impersonation policy and a subject grant to the resources. One version
// of the policy set the values file names holds every policy, and the run
// activates it. It prints the
// k8s_sandbox and sandbox_user_access values as YAML. With --dry-run it changes
// nothing and prints nothing on stdout.
//
//	zone-setup --values zone-setup.yaml > zone-setup.out.yaml
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/keycardai/keycard-go"
	"github.com/urfave/cli/v3"
	"sigs.k8s.io/yaml"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newCommand().Run(ctx, os.Args)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zone-setup: %v\n", err)
		os.Exit(1)
	}
}

const replaceActiveFlag = "replace-active-policy-set"

func newCommand() *cli.Command {
	return &cli.Command{
		Name:            "zone-setup",
		Usage:           "find or create the zone's provider, application, resources, users' access and policy set",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "values", Required: true, Usage: "the values file, YAML or JSON"},
			&cli.BoolFlag{Name: "dry-run", Usage: "change nothing and print what would change"},
			&cli.BoolFlag{
				Name:  replaceActiveFlag,
				Usage: "activate the sandbox policy set even when another policy set is active, replacing it",
			},
		},
		Action: setup,
	}
}

type output struct {
	K8sSandbox struct {
		ApplicationID         string `json:"application_id"`
		ApplicationIdentifier string `json:"application_identifier"`
		Namespace             string `json:"namespace"`
		ProviderID            string `json:"provider_id"`
	} `json:"k8s_sandbox"`
	SandboxUserAccess []userAccess `json:"sandbox_user_access,omitempty"`
}

type userAccess struct {
	UserEmail      string `json:"user_email"`
	UserID         string `json:"user_id"`
	ServiceAccount string `json:"service_account"`
	ClientID       string `json:"client_id"`
	Namespace      string `json:"namespace"`
}

func setup(ctx context.Context, cmd *cli.Command) error {
	path := cmd.String("values")
	values, err := readValues(path)
	if err != nil {
		return err
	}
	secrets, err := zonesetup.LoadSecrets(values, os.LookupEnv, os.ReadFile)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	session, err := zonesetup.Connect(ctx, values, secrets)
	if err != nil {
		return fmt.Errorf("resolving the zone: %w", err)
	}
	sayf(cmd, "zone %s issues %s", session.Zone.ID, session.Zone.Protocols.Oauth2.Issuer)
	providerID, err := ensureProvider(ctx, cmd, session, values.IssuerURL)
	if err != nil {
		return err
	}
	identifier := zonesetup.ApplicationIdentifier(values.Namespace)
	applicationID, err := ensureApplication(ctx, cmd, session, identifier, values.Namespace)
	if err != nil {
		return err
	}
	err = ensureResources(ctx, cmd, session, applicationID, values.Resources, secrets.Resource)
	if err != nil {
		return err
	}
	access, err := ensureUserAccess(ctx, cmd, session, applicationID, providerID, values)
	if err != nil {
		return err
	}
	if err = ensurePolicies(ctx, cmd, session, values, access); err != nil {
		return err
	}
	if cmd.Bool("dry-run") {
		sayf(cmd, "dry run: nothing was written")
		return nil
	}
	var out output
	out.K8sSandbox.ApplicationID = applicationID
	out.K8sSandbox.ApplicationIdentifier = identifier
	out.K8sSandbox.Namespace = values.Namespace
	out.K8sSandbox.ProviderID = providerID
	out.SandboxUserAccess = access
	return writeOutput(cmd, out)
}

func writeOutput(cmd *cli.Command, out output) error {
	raw, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("encoding the output: %w", err)
	}
	if _, err = cmd.Writer.Write(raw); err != nil {
		return fmt.Errorf("writing the output: %w", err)
	}
	return nil
}

func ensureProvider(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	issuerURL string,
) (string, error) {
	providerID, err := session.FindProvider(ctx, issuerURL)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the provider: %w", err)
	case providerID != "":
		sayf(cmd, "found provider %s", providerID)
		return providerID, nil
	case cmd.Bool("dry-run"):
		sayf(cmd, "would create a provider for %s", issuerURL)
		return "", nil
	}
	if providerID, err = session.CreateProvider(ctx, issuerURL); err != nil {
		return "", fmt.Errorf("creating the provider: %w", err)
	}
	sayf(cmd, "created provider %s", providerID)
	return providerID, nil
}

func ensureApplication(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	identifier, namespace string,
) (string, error) {
	app, err := session.FindApplication(ctx, identifier)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the application: %w", err)
	case app.ID == "" && cmd.Bool("dry-run"):
		sayf(cmd, "would create application %s", identifier)
		return "", nil
	case app.ID == "":
		applicationID, err := session.CreateApplication(ctx, identifier, namespace)
		if err != nil {
			return "", fmt.Errorf("creating the application: %w", err)
		}
		sayf(cmd, "created application %s", applicationID)
		return applicationID, nil
	case app.Consent == keycard.ApplicationConsentImplicit:
		sayf(cmd, "found application %s", app.ID)
		return app.ID, nil
	case cmd.Bool("dry-run"):
		sayf(cmd, "would set consent to implicit on application %s", app.ID)
		return app.ID, nil
	}
	if err := session.SetApplicationConsentImplicit(ctx, app.ID); err != nil {
		return "", fmt.Errorf("setting consent on the application: %w", err)
	}
	sayf(cmd, "set consent to implicit on application %s", app.ID)
	return app.ID, nil
}

func ensureResources(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	applicationID string,
	resources []zonesetup.ResourceValues,
	tokens map[string]string,
) error {
	ids := make([]string, 0, len(resources))
	for _, resource := range resources {
		providerID, err := session.ResourceProvider(ctx, resource)
		if err != nil {
			return fmt.Errorf("finding the credential provider for %s: %w", resource.Identifier, err)
		}
		resourceID, err := ensureResource(ctx, cmd, session, resource, providerID)
		if err != nil {
			return err
		}
		ids = append(ids, resourceID)
		if resource.SecretEnv == "" {
			continue
		}
		token, ok := tokens[resource.SecretEnv]
		if !ok {
			return fmt.Errorf("%w: %s", zonesetup.ErrMissingSecret, resource.SecretEnv)
		}
		if err = ensureSecret(ctx, cmd, session, resource, resourceID, token); err != nil {
			return err
		}
	}
	return addMissingDependencies(ctx, cmd, session, applicationID, resources, ids)
}

func ensureResource(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	resource zonesetup.ResourceValues,
	providerID string,
) (string, error) {
	found, err := session.FindResource(ctx, resource.Identifier)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the resource %s: %w", resource.Identifier, err)
	case found.ID == "" && cmd.Bool("dry-run"):
		sayf(cmd, "would create a resource for %s", resource.Identifier)
		return "", nil
	case found.ID == "":
		resourceID, err := session.CreateResource(ctx, resource, providerID)
		if err != nil {
			return "", fmt.Errorf("creating the resource %s: %w", resource.Identifier, err)
		}
		sayf(cmd, "created resource %s for %s", resourceID, resource.Identifier)
		return resourceID, nil
	case found.Prefix == resource.Prefix && found.CredentialProviderID == providerID:
		sayf(cmd, "found resource %s for %s", found.ID, resource.Identifier)
		return found.ID, nil
	case cmd.Bool("dry-run"):
		sayf(cmd, "would patch resource %s for %s", found.ID, resource.Identifier)
		return found.ID, nil
	}
	if err := session.PatchResource(ctx, found.ID, resource.Prefix, providerID); err != nil {
		return "", fmt.Errorf("patching the resource %s: %w", resource.Identifier, err)
	}
	sayf(cmd, "patched resource %s for %s", found.ID, resource.Identifier)
	return found.ID, nil
}

func ensureSecret(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	resource zonesetup.ResourceValues,
	resourceID, token string,
) error {
	if resourceID == "" {
		sayf(cmd, "would store the %s credential for %s", resource.SecretEnv, resource.Identifier)
		return nil
	}
	secretID, err := session.FindSecret(ctx, resourceID)
	switch {
	case err != nil:
		return fmt.Errorf("finding the credential for %s: %w", resource.Identifier, err)
	case secretID == "" && cmd.Bool("dry-run"):
		sayf(cmd, "would store the %s credential for %s", resource.SecretEnv, resource.Identifier)
	case secretID == "":
		if err := session.CreateSecret(ctx, resourceID, resource.SecretEnv, token); err != nil {
			return fmt.Errorf("storing the credential for %s: %w", resource.Identifier, err)
		}
		sayf(cmd, "stored the %s credential for %s", resource.SecretEnv, resource.Identifier)
	case cmd.Bool("dry-run"):
		sayf(cmd, "would update the %s credential for %s", resource.SecretEnv, resource.Identifier)
	default:
		if err := session.UpdateSecret(ctx, secretID, token); err != nil {
			return fmt.Errorf("updating the credential for %s: %w", resource.Identifier, err)
		}
		sayf(cmd, "updated the %s credential for %s", resource.SecretEnv, resource.Identifier)
	}
	return nil
}

func addMissingDependencies(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	applicationID string,
	resources []zonesetup.ResourceValues,
	resourceIDs []string,
) error {
	has := map[string]bool{}
	if applicationID != "" && len(resourceIDs) > 0 {
		var err error
		if has, err = session.ApplicationDependencies(ctx, applicationID); err != nil {
			return fmt.Errorf("listing the application's dependencies: %w", err)
		}
	}
	for i, resourceID := range resourceIDs {
		if resourceID == "" || applicationID == "" {
			sayf(cmd, "would add the resource for %s as a dependency", resources[i].Identifier)
			continue
		}
		if err := ensureDependency(ctx, cmd, session, applicationID, resourceID, has[resourceID]); err != nil {
			return err
		}
	}
	return nil
}

func ensureDependency(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	applicationID, resourceID string,
	has bool,
) error {
	switch {
	case has:
		sayf(cmd, "found resource %s as a dependency", resourceID)
	case cmd.Bool("dry-run"):
		sayf(cmd, "would add resource %s as a dependency", resourceID)
	default:
		if err := session.AddApplicationDependency(ctx, applicationID, resourceID); err != nil {
			return fmt.Errorf("adding a dependency: %w", err)
		}
		sayf(cmd, "added resource %s as a dependency", resourceID)
	}
	return nil
}

func ensureUserAccess(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	applicationID, providerID string,
	values *zonesetup.Values,
) ([]userAccess, error) {
	access := make([]userAccess, 0, len(values.Users))
	for _, user := range values.Users {
		userID, err := session.FindUser(ctx, user.Email)
		if err != nil {
			return nil, fmt.Errorf("finding the user %s: %w", user.Email, err)
		}
		sayf(cmd, "found user %s for %s", userID, user.Email)
		subject := zonesetup.ServiceAccountSubject(values.Namespace, userID)
		credentialID, err := ensureTokenCredential(ctx, cmd, session, applicationID, providerID, subject)
		if err != nil {
			return nil, err
		}
		access = append(access, userAccess{
			UserEmail:      user.Email,
			UserID:         userID,
			ServiceAccount: subject,
			ClientID:       credentialID,
			Namespace:      values.Namespace,
		})
	}
	return access, nil
}

func ensureTokenCredential(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	applicationID, providerID, subject string,
) (string, error) {
	if applicationID == "" || providerID == "" {
		sayf(cmd, "would create a token credential for %s", subject)
		return "", nil
	}
	credentialID, err := session.FindTokenCredential(ctx, applicationID, providerID, subject)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the token credential for %s: %w", subject, err)
	case credentialID != "":
		sayf(cmd, "found token credential %s for %s", credentialID, subject)
		return credentialID, nil
	case cmd.Bool("dry-run"):
		sayf(cmd, "would create a token credential for %s", subject)
		return "", nil
	}
	if credentialID, err = session.CreateTokenCredential(ctx, applicationID, providerID, subject); err != nil {
		return "", fmt.Errorf("creating the token credential for %s: %w", subject, err)
	}
	sayf(cmd, "created token credential %s for %s", credentialID, subject)
	return credentialID, nil
}

// ensurePolicies finds or publishes each user's impersonation policy and
// subject grant, then makes the policy set hold every one of them. An exchange
// needs both: the subject grant lets the user reach the resource, and the
// impersonation policy lets the Application act for the user. A values file
// that lists no resources gets the impersonation policy alone, since a grant
// that names no resource permits nothing.
func ensurePolicies(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	values *zonesetup.Values,
	access []userAccess,
) error {
	if len(access) == 0 {
		return nil
	}
	identifiers := make([]string, 0, len(values.Resources))
	for _, resource := range values.Resources {
		identifiers = append(identifiers, resource.Identifier)
	}
	var entries []keycard.PolicySetManifestEntryParam
	for _, user := range access {
		actor, err := ensurePolicy(ctx, cmd, session,
			zonesetup.PolicyName(user.UserID),
			fmt.Sprintf("Lets the sandbox of %s impersonate that user, and nobody else", user.UserEmail),
			zonesetup.ImpersonationPolicy(values.Namespace, user.UserID))
		if err != nil {
			return err
		}
		entries = append(entries, actor)
		if len(identifiers) == 0 {
			continue
		}
		grant, err := ensurePolicy(ctx, cmd, session,
			zonesetup.SubjectGrantName(user.UserID),
			fmt.Sprintf("Lets %s reach the destinations the sandbox mints credentials for", user.UserEmail),
			zonesetup.SubjectGrantPolicy(user.UserID, identifiers))
		if err != nil {
			return err
		}
		entries = append(entries, grant)
	}
	return ensurePolicySet(ctx, cmd, session, values.PolicySet, entries)
}

// ensurePolicy finds or creates the policy named name and makes its latest
// version hold cedar.
func ensurePolicy(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	name, description, cedar string,
) (keycard.PolicySetManifestEntryParam, error) {
	policy, err := session.FindPolicy(ctx, name)
	switch {
	case err != nil:
		return keycard.PolicySetManifestEntryParam{}, fmt.Errorf("finding the policy %q: %w", name, err)
	case policy.ID == "" && cmd.Bool("dry-run"):
		sayf(cmd, "would create the policy %q", name)
		return keycard.PolicySetManifestEntryParam{}, nil
	case policy.ID == "":
		if policy.ID, err = session.CreatePolicy(ctx, name, description); err != nil {
			return keycard.PolicySetManifestEntryParam{}, fmt.Errorf("creating the policy %q: %w", name, err)
		}
		sayf(cmd, "created policy %s for %q", policy.ID, name)
		return publishPolicyVersion(ctx, cmd, session, policy.ID, cedar)
	}
	latest, err := session.LatestPolicyVersion(ctx, policy.ID)
	switch {
	case err != nil:
		return keycard.PolicySetManifestEntryParam{}, fmt.Errorf("reading the policy %q: %w", name, err)
	case latest.ID != "" && strings.TrimSpace(latest.CedarRaw) == strings.TrimSpace(cedar):
		sayf(cmd, "found policy %s version %s for %q", policy.ID, latest.ID, name)
		return keycard.PolicySetManifestEntryParam{PolicyID: policy.ID, PolicyVersionID: latest.ID}, nil
	case cmd.Bool("dry-run"):
		sayf(cmd, "would publish a new version of policy %s for %q", policy.ID, name)
		return keycard.PolicySetManifestEntryParam{PolicyID: policy.ID}, nil
	}
	return publishPolicyVersion(ctx, cmd, session, policy.ID, cedar)
}

func publishPolicyVersion(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	policyID, cedar string,
) (keycard.PolicySetManifestEntryParam, error) {
	versionID, err := session.CreatePolicyVersion(ctx, policyID, cedar)
	if err != nil {
		return keycard.PolicySetManifestEntryParam{}, fmt.Errorf("publishing a version of policy %s: %w", policyID, err)
	}
	sayf(cmd, "published policy %s version %s", policyID, versionID)
	return keycard.PolicySetManifestEntryParam{PolicyID: policyID, PolicyVersionID: versionID}, nil
}

// ensurePolicySet makes the active version of the policy set named name hold
// every entry, and activates it.
//
// A zone has one active policy set. Activating the sandbox set while another
// set is active replaces that set, which is refused with ErrPolicySetActive
// unless --replace-active-policy-set is given. The check runs before the set
// is written, and in a dry run too, so a dry run reports the same refusal.
func ensurePolicySet(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	name string,
	entries []keycard.PolicySetManifestEntryParam,
) error {
	set, err := session.FindPolicySet(ctx, name)
	if err != nil {
		return fmt.Errorf("finding the policy set %q: %w", name, err)
	}
	if !set.Active {
		if err = checkOtherActive(ctx, cmd, session, set.ID, name); err != nil {
			return err
		}
	}
	switch {
	case slices.ContainsFunc(entries, pendingEntry) || set.ID == "" && cmd.Bool("dry-run"):
		sayf(cmd, "would publish a version of policy set %q with %d policies and activate it", name, len(entries))
		return nil
	case set.ID == "":
		if set.ID, err = session.CreatePolicySet(ctx, name); err != nil {
			return fmt.Errorf("creating the policy set %q: %w", name, err)
		}
		sayf(cmd, "created policy set %s for %q", set.ID, name)
	}
	return reconcilePolicySetVersion(ctx, cmd, session, set, entries)
}

// pendingEntry is an entry for a policy or version a dry run would create.
func pendingEntry(entry keycard.PolicySetManifestEntryParam) bool {
	return entry.PolicyID == "" || entry.PolicyVersionID == ""
}

// checkOtherActive refuses to replace another active policy set unless
// --replace-active-policy-set is given. ownID is the sandbox set's id, or ""
// when it does not exist yet.
func checkOtherActive(ctx context.Context, cmd *cli.Command, session *zonesetup.Session, ownID, name string) error {
	active, err := session.ActivePolicySets(ctx)
	if err != nil {
		return fmt.Errorf("listing the active policy sets: %w", err)
	}
	for _, other := range active {
		switch {
		case other.ID == ownID:
		case cmd.Bool(replaceActiveFlag):
			sayf(cmd, "policy set %q (%s) is active and will be replaced", other.Name, other.ID)
		default:
			return fmt.Errorf("%w: %q (%s). Activating %q would replace it. Re-run with --%s to do that",
				zonesetup.ErrPolicySetActive, other.Name, other.ID, name, replaceActiveFlag)
		}
	}
	return nil
}

// reconcilePolicySetVersion starts from the active version's manifest, or the
// latest version's when the set is not active, so entries this run does not
// manage are kept. An unchanged manifest is activated as it is. A changed one
// is published as a new version and activated.
func reconcilePolicySetVersion(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	set keycard.PolicySetWithBinding,
	entries []keycard.PolicySetManifestEntryParam,
) error {
	baseID, base, err := baseManifest(ctx, session, set)
	if err != nil {
		return err
	}
	desired, changed := mergeManifest(base, entries)
	switch {
	case !changed && baseID != "" && set.Active && set.ActiveVersionID == baseID:
		sayf(cmd, "found policy set %s version %s active with every policy", set.ID, baseID)
		return nil
	case !changed && baseID != "":
		return activatePolicySetVersion(ctx, cmd, session, set.ID, baseID)
	case cmd.Bool("dry-run"):
		sayf(cmd, "would publish a version of policy set %s with %d policies and activate it", set.ID, len(desired))
		return nil
	}
	versionID, err := session.CreatePolicySetVersion(ctx, set.ID, desired)
	if err != nil {
		return fmt.Errorf("publishing a version of policy set %s: %w", set.ID, err)
	}
	sayf(cmd, "published policy set %s version %s with %d policies", set.ID, versionID, len(desired))
	return activatePolicySetVersion(ctx, cmd, session, set.ID, versionID)
}

// baseManifest returns the id and entries of the set's active version, else
// of its latest version, else none.
func baseManifest(
	ctx context.Context,
	session *zonesetup.Session,
	set keycard.PolicySetWithBinding,
) (string, []keycard.PolicySetManifestEntry, error) {
	baseID := set.ActiveVersionID
	if baseID == "" {
		baseID = set.LatestVersionID
	}
	if baseID == "" {
		return "", nil, nil
	}
	base, err := session.PolicySetManifest(ctx, set.ID, baseID)
	if err != nil {
		return "", nil, fmt.Errorf("reading the policy set %s: %w", set.ID, err)
	}
	return baseID, base, nil
}

func activatePolicySetVersion(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	setID, versionID string,
) error {
	if cmd.Bool("dry-run") {
		sayf(cmd, "would activate policy set %s version %s", setID, versionID)
		return nil
	}
	if err := session.ActivatePolicySetVersion(ctx, setID, versionID); err != nil {
		return fmt.Errorf("activating policy set %s: %w", setID, err)
	}
	sayf(cmd, "activated policy set %s version %s", setID, versionID)
	return nil
}

// mergeManifest returns base with each entry's policy pointed at the entry's
// version, and each policy base lacks appended. changed is false when the
// result equals base.
func mergeManifest(
	base []keycard.PolicySetManifestEntry,
	entries []keycard.PolicySetManifestEntryParam,
) ([]keycard.PolicySetManifestEntryParam, bool) {
	out := make([]keycard.PolicySetManifestEntryParam, 0, len(base)+len(entries))
	index := map[string]int{}
	for _, entry := range base {
		index[entry.PolicyID] = len(out)
		out = append(out, keycard.PolicySetManifestEntryParam{
			PolicyID: entry.PolicyID, PolicyVersionID: entry.PolicyVersionID,
		})
	}
	changed := false
	for _, entry := range entries {
		if i, ok := index[entry.PolicyID]; ok {
			if out[i].PolicyVersionID != entry.PolicyVersionID {
				out[i].PolicyVersionID = entry.PolicyVersionID
				changed = true
			}
			continue
		}
		index[entry.PolicyID] = len(out)
		out = append(out, entry)
		changed = true
	}
	return out, changed
}

func sayf(cmd *cli.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.ErrWriter, format+"\n", args...)
}

func readValues(path string) (*zonesetup.Values, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the path is the values file the operator named.
	if err != nil {
		return nil, fmt.Errorf("%w: %w", zonesetup.ErrValues, err)
	}
	defer func() { _ = file.Close() }()
	values, err := zonesetup.ParseValues(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return values, nil
}
