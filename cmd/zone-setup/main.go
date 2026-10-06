// Command zone-setup configures a Keycard zone for the sandboxes from a values
// file. It finds or creates the provider for the cluster's OIDC issuer and the
// egress proxy Application with implicit consent, finds, creates or patches
// each resource, stores each secret_env credential, and makes each resource a
// dependency of the Application. For each user it binds the user's
// ServiceAccount to the Application with a token credential, and publishes the
// user's impersonation policy and access policy. It publishes one policy set
// version with every policy and activates it. It prints the k8s_sandbox and
// sandbox_user_access values as YAML. With --dry-run it changes nothing and
// prints nothing on stdout.
//
//	zone-setup --values zone-setup.yaml > zone-setup.out.yaml
package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
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

var errPendingEntry = errors.New("a policy set entry has no policy or version id")

func newCommand() *cli.Command {
	return &cli.Command{
		Name:            "zone-setup",
		Usage:           "find or create the zone's provider, application, resources, users' access and policy set",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "values", Required: true, Usage: "the values file, YAML or JSON"},
			&cli.BoolFlag{Name: "dry-run", Usage: "change nothing and print what would change"},
			&cli.BoolFlag{
				Name: replaceActiveFlag,
				Usage: "activate the sandbox policy set even when another policy set is active. " +
					"The other set's policies stop applying",
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
	// UserIdentifier is the user's zone-scoped identifier, which the
	// policies match. It is not part of the output.
	UserIdentifier string `json:"-"`
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
		found, err := session.FindUser(ctx, user.Email)
		if err != nil {
			return nil, fmt.Errorf("finding the user %s: %w", user.Email, err)
		}
		sayf(cmd, "found user %s for %s", found.ID, user.Email)
		subject := zonesetup.ServiceAccountSubject(values.Namespace, found.ID)
		credentialID, err := ensureTokenCredential(ctx, cmd, session, applicationID, providerID, subject)
		if err != nil {
			return nil, err
		}
		access = append(access, userAccess{
			UserEmail:      user.Email,
			UserID:         found.ID,
			ServiceAccount: subject,
			ClientID:       credentialID,
			Namespace:      values.Namespace,
			UserIdentifier: found.Identifier,
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

// ensurePolicies publishes each user's policies and makes the active policy
// set hold them. It removes from the set each policy of the namespace that
// the values file does not name, then archives it, so a removed user loses
// access. If the sandbox set is not active, it calls checkOtherActive before
// it creates or publishes anything.
func ensurePolicies(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	values *zonesetup.Values,
	access []userAccess,
) error {
	set, err := session.FindPolicySet(ctx, values.PolicySet)
	if err != nil {
		return fmt.Errorf("finding the policy set %q: %w", values.PolicySet, err)
	}
	owned, err := session.SandboxPolicies(ctx, values.Namespace)
	if err != nil {
		return fmt.Errorf("listing the sandbox policies: %w", err)
	}
	if len(access) == 0 && !set.Active {
		return archiveRemoved(ctx, cmd, session, owned, nil)
	}
	if !set.Active {
		if err = checkOtherActive(ctx, cmd, session, set.ID, values.PolicySet); err != nil {
			return err
		}
	}
	entries, err := ensureUserPolicies(ctx, cmd, session, values, access, owned)
	if err != nil {
		return err
	}
	ownedIDs := map[string]bool{}
	for _, policy := range owned {
		ownedIDs[policy.ID] = true
	}
	if err = ensurePolicySet(ctx, cmd, session, set, values.PolicySet, entries, ownedIDs); err != nil {
		return err
	}
	return archiveRemoved(ctx, cmd, session, owned, entries)
}

// archiveRemoved archives each owned policy that entries does not have.
// owned maps a policy name to the policy. Call it only after ensurePolicySet,
// so the active set does not hold a policy it archives.
func archiveRemoved(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	owned map[string]keycard.Policy,
	entries []keycard.PolicySetManifestEntryParam,
) error {
	kept := map[string]bool{}
	for _, entry := range entries {
		kept[entry.PolicyID] = true
	}
	for _, name := range slices.Sorted(maps.Keys(owned)) {
		policyID := owned[name].ID
		switch {
		case kept[policyID]:
		case cmd.Bool("dry-run"):
			sayf(cmd, "would archive policy %s for %q", policyID, name)
		default:
			if err := session.ArchivePolicy(ctx, policyID); err != nil {
				return fmt.Errorf("archiving the policy %q: %w", name, err)
			}
			sayf(cmd, "archived policy %s for %q", policyID, name)
		}
	}
	return nil
}

// ensureUserPolicies runs ensurePolicy for each policy of each user and
// returns their manifest entries. owned maps a policy name to the policy the
// zone already has.
func ensureUserPolicies(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	values *zonesetup.Values,
	access []userAccess,
	owned map[string]keycard.Policy,
) ([]keycard.PolicySetManifestEntryParam, error) {
	var entries []keycard.PolicySetManifestEntryParam
	for _, user := range access {
		policyUser := keycard.User{ID: user.UserID, Identifier: user.UserIdentifier}
		for _, policy := range zonesetup.UserPolicies(values.Namespace, values.IssuerURL, policyUser) {
			entry, err := ensurePolicy(ctx, cmd, session, owned[policy.Name], policy)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// ensurePolicy creates the policy when found has no ID, and publishes a
// version when the latest one does not have the policy's digest.
func ensurePolicy(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	found keycard.Policy,
	policy zonesetup.UserPolicy,
) (keycard.PolicySetManifestEntryParam, error) {
	switch {
	case found.ID == "" && cmd.Bool("dry-run"):
		sayf(cmd, "would create the policy %q", policy.Name)
		return keycard.PolicySetManifestEntryParam{}, nil
	case found.ID == "":
		policyID, err := session.CreatePolicy(ctx, policy.Name, policy.Description)
		if err != nil {
			return keycard.PolicySetManifestEntryParam{}, fmt.Errorf("creating the policy %q: %w", policy.Name, err)
		}
		sayf(cmd, "created policy %s for %q", policyID, policy.Name)
		return publishPolicyVersion(ctx, cmd, session, policyID, policy.Cedar)
	}
	versionID, digest, err := session.LatestPolicyDigest(ctx, found.ID)
	switch {
	case err != nil:
		return keycard.PolicySetManifestEntryParam{}, fmt.Errorf("reading the policy %q: %w", policy.Name, err)
	case versionID != "" && digest == policy.Digest:
		sayf(cmd, "found policy %s version %s for %q", found.ID, versionID, policy.Name)
		return keycard.PolicySetManifestEntryParam{PolicyID: found.ID, PolicyVersionID: versionID}, nil
	case cmd.Bool("dry-run"):
		sayf(cmd, "would publish a new version of policy %s for %q", found.ID, policy.Name)
		return keycard.PolicySetManifestEntryParam{PolicyID: found.ID}, nil
	}
	return publishPolicyVersion(ctx, cmd, session, found.ID, policy.Cedar)
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

// ensurePolicySet activates a version of the policy set that holds entries
// and no other owned policy. set has no ID when the set does not exist yet.
// Outside a dry run, an entry without a policy or version ID fails the run
// with errPendingEntry.
func ensurePolicySet(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	set keycard.PolicySetWithBinding,
	name string,
	entries []keycard.PolicySetManifestEntryParam,
	owned map[string]bool,
) error {
	pending := slices.ContainsFunc(entries, pendingEntry)
	switch {
	case pending && !cmd.Bool("dry-run"):
		return fmt.Errorf("%w: policy set %q", errPendingEntry, name)
	case pending || set.ID == "" && cmd.Bool("dry-run"):
		sayf(cmd, "would publish a version of policy set %q with %d policies and activate it", name, len(entries))
		return nil
	case set.ID == "":
		setID, err := session.CreatePolicySet(ctx, name)
		if err != nil {
			return fmt.Errorf("creating the policy set %q: %w", name, err)
		}
		set = keycard.PolicySetWithBinding{ID: setID}
		sayf(cmd, "created policy set %s for %q", setID, name)
	}
	return reconcilePolicySetVersion(ctx, cmd, session, set, entries, owned)
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
// latest version's when the set is not active. It activates an unchanged
// manifest as it is, and publishes a changed one as a new version first.
func reconcilePolicySetVersion(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	set keycard.PolicySetWithBinding,
	entries []keycard.PolicySetManifestEntryParam,
	owned map[string]bool,
) error {
	baseID, base, err := baseManifest(ctx, session, set)
	if err != nil {
		return err
	}
	desired, changed := mergeManifest(base, entries, owned)
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

// mergeManifest points each policy already in base at its entry's version,
// drops each owned policy that entries does not have, keeps every other
// policy, and appends the entries base does not have. changed is false when
// the result equals base.
func mergeManifest(
	base []keycard.PolicySetManifestEntry,
	entries []keycard.PolicySetManifestEntryParam,
	owned map[string]bool,
) ([]keycard.PolicySetManifestEntryParam, bool) {
	want := make(map[string]string, len(entries))
	for _, entry := range entries {
		want[entry.PolicyID] = entry.PolicyVersionID
	}
	out := make([]keycard.PolicySetManifestEntryParam, 0, len(base)+len(entries))
	kept := map[string]bool{}
	changed := false
	for _, entry := range base {
		versionID, wanted := want[entry.PolicyID]
		switch {
		case wanted:
			changed = changed || versionID != entry.PolicyVersionID
		case owned[entry.PolicyID]:
			changed = true
			continue
		default:
			versionID = entry.PolicyVersionID
		}
		kept[entry.PolicyID] = true
		out = append(out, keycard.PolicySetManifestEntryParam{PolicyID: entry.PolicyID, PolicyVersionID: versionID})
	}
	for _, entry := range entries {
		if !kept[entry.PolicyID] {
			out = append(out, entry)
			changed = true
		}
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
