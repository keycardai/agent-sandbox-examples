package zonesetup

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"github.com/keycardai/keycard-go"
)

// ReplaceActiveFlag is the flag that sets Options.ReplaceActive.
const ReplaceActiveFlag = "replace-active-policy-set"

// Options control how Run changes the zone.
type Options struct {
	// DryRun logs what would change and does not change the zone.
	DryRun bool
	// ReplaceActive activates the sandbox policy set even when another
	// policy set is active. The other set's policies stop applying.
	ReplaceActive bool
	// Log gets one line per entity Run finds, creates or changes.
	Log io.Writer
}

// Output holds the k8s_sandbox and sandbox_user_access values a sandbox
// needs from the zone.
type Output struct {
	K8sSandbox struct {
		ApplicationID         string `json:"application_id"`
		ApplicationIdentifier string `json:"application_identifier"`
		Namespace             string `json:"namespace"`
		ProviderID            string `json:"provider_id"`
	} `json:"k8s_sandbox"`
	SandboxUserAccess []UserAccess `json:"sandbox_user_access,omitempty"`
}

// UserAccess is one user's sandbox identity.
type UserAccess struct {
	UserEmail      string `json:"user_email"`
	UserID         string `json:"user_id"`
	ServiceAccount string `json:"service_account"`
	ClientID       string `json:"client_id"`
	Namespace      string `json:"namespace"`
	// UserIdentifier is the user's zone-scoped identifier. The policies
	// match on it. It is not in the output.
	UserIdentifier string `json:"-"`
}

// Run configures the zone for the sandboxes. It finds or creates the
// provider for the cluster's OIDC issuer, and the egress proxy Application
// with implicit consent. It finds, creates or patches each resource, stores
// each static token in the zone's vault, and makes each resource a
// dependency of the Application. For each user it binds the user's
// ServiceAccount to the Application with a token credential. It publishes
// one policy set version with each user's impersonation and access policies,
// and activates it.
//
// With opts.DryRun it does not change the zone. The Output then has an empty
// id for each entity that does not exist yet.
func Run(ctx context.Context, values *Values, secrets *Secrets, opts *Options) (*Output, error) {
	session, err := Connect(ctx, values, secrets)
	if err != nil {
		return nil, fmt.Errorf("resolving the zone: %w", err)
	}
	opts.sayf("zone %s issues %s", session.Zone.ID, session.Zone.Protocols.Oauth2.Issuer)
	providerID, err := ensureProvider(ctx, opts, session, values.IssuerURL)
	if err != nil {
		return nil, err
	}
	identifier := ApplicationIdentifier(values.Namespace)
	application, err := ensureApplication(ctx, opts, session, identifier, values.Namespace)
	if err != nil {
		return nil, err
	}
	err = ensureResources(ctx, opts, session, application.ID, values.Resources, secrets.Resource)
	if err != nil {
		return nil, err
	}
	access, err := ensureUserAccess(ctx, opts, session, application.ID, providerID, values)
	if err != nil {
		return nil, err
	}
	if err = ensurePolicies(ctx, opts, session, values, application.Name, access); err != nil {
		return nil, err
	}
	if opts.DryRun {
		opts.sayf("dry run: nothing was written")
	}
	out := &Output{SandboxUserAccess: access}
	out.K8sSandbox.ApplicationID = application.ID
	out.K8sSandbox.ApplicationIdentifier = identifier
	out.K8sSandbox.Namespace = values.Namespace
	out.K8sSandbox.ProviderID = providerID
	return out, nil
}

// ReadValuesFile parses the values file at path. A non-empty issuerURL
// replaces its issuer_url. Errors name source as where issuerURL came from.
func ReadValuesFile(path, issuerURL, source string) (*Values, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the path is the values file the operator named.
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrValues, err)
	}
	defer func() { _ = file.Close() }()
	values, err := ParseValues(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if issuerURL != "" {
		if err = values.SetIssuerURL(source, issuerURL); err != nil {
			return nil, fmt.Errorf("replacing issuer_url: %w", err)
		}
	}
	return values, nil
}

func ensureProvider(
	ctx context.Context,
	opts *Options,
	session *Session,
	issuerURL string,
) (string, error) {
	providerID, err := session.FindProvider(ctx, issuerURL)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the provider: %w", err)
	case providerID != "":
		opts.sayf("found provider %s", providerID)
		return providerID, nil
	case opts.DryRun:
		opts.sayf("would create a provider for %s", issuerURL)
		return "", nil
	}
	if providerID, err = session.CreateProvider(ctx, issuerURL); err != nil {
		return "", fmt.Errorf("creating the provider: %w", err)
	}
	opts.sayf("created provider %s", providerID)
	return providerID, nil
}

// ensureApplication returns the egress proxy Application. The Application
// has no ID in a dry run that would create it.
func ensureApplication(
	ctx context.Context,
	opts *Options,
	session *Session,
	identifier, namespace string,
) (keycard.Application, error) {
	app, err := session.FindApplication(ctx, identifier)
	switch {
	case err != nil:
		return keycard.Application{}, fmt.Errorf("finding the application: %w", err)
	case app.ID == "" && opts.DryRun:
		opts.sayf("would create application %s", identifier)
		return keycard.Application{Name: ApplicationName}, nil
	case app.ID == "":
		applicationID, err := session.CreateApplication(ctx, identifier, namespace)
		if err != nil {
			return keycard.Application{}, fmt.Errorf("creating the application: %w", err)
		}
		opts.sayf("created application %s", applicationID)
		return keycard.Application{ID: applicationID, Name: ApplicationName}, nil
	case app.Consent == keycard.ApplicationConsentImplicit:
		opts.sayf("found application %s", app.ID)
		return app, nil
	case opts.DryRun:
		opts.sayf("would set consent to implicit on application %s", app.ID)
		return app, nil
	}
	if err := session.SetApplicationConsentImplicit(ctx, app.ID); err != nil {
		return keycard.Application{}, fmt.Errorf("setting consent on the application: %w", err)
	}
	opts.sayf("set consent to implicit on application %s", app.ID)
	return app, nil
}

func ensureResources(
	ctx context.Context,
	opts *Options,
	session *Session,
	applicationID string,
	resources []ResourceValues,
	tokens map[string]string,
) error {
	ids := make([]string, 0, len(resources))
	for _, resource := range resources {
		providerID, err := session.ResourceProvider(ctx, resource)
		if err != nil {
			return fmt.Errorf("finding the credential provider for %s: %w", resource.Identifier, err)
		}
		resourceID, err := ensureResource(ctx, opts, session, resource, providerID)
		if err != nil {
			return err
		}
		ids = append(ids, resourceID)
		if resource.SecretEnv == "" {
			continue
		}
		token, ok := tokens[resource.SecretEnv]
		if !ok {
			return fmt.Errorf("%w: %s", ErrMissingSecret, resource.SecretEnv)
		}
		if err = ensureSecret(ctx, opts, session, resource, resourceID, token); err != nil {
			return err
		}
	}
	return addMissingDependencies(ctx, opts, session, applicationID, resources, ids)
}

func ensureResource(
	ctx context.Context,
	opts *Options,
	session *Session,
	resource ResourceValues,
	providerID string,
) (string, error) {
	found, err := session.FindResource(ctx, resource.Identifier)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the resource %s: %w", resource.Identifier, err)
	case found.ID == "" && opts.DryRun:
		opts.sayf("would create a resource for %s", resource.Identifier)
		return "", nil
	case found.ID == "":
		resourceID, err := session.CreateResource(ctx, resource, providerID)
		if err != nil {
			return "", fmt.Errorf("creating the resource %s: %w", resource.Identifier, err)
		}
		opts.sayf("created resource %s for %s", resourceID, resource.Identifier)
		return resourceID, nil
	case found.Prefix == resource.Prefix && found.CredentialProviderID == providerID:
		opts.sayf("found resource %s for %s", found.ID, resource.Identifier)
		return found.ID, nil
	case opts.DryRun:
		opts.sayf("would patch resource %s for %s", found.ID, resource.Identifier)
		return found.ID, nil
	}
	if err := session.PatchResource(ctx, found.ID, resource.Prefix, providerID); err != nil {
		return "", fmt.Errorf("patching the resource %s: %w", resource.Identifier, err)
	}
	opts.sayf("patched resource %s for %s", found.ID, resource.Identifier)
	return found.ID, nil
}

func ensureSecret(
	ctx context.Context,
	opts *Options,
	session *Session,
	resource ResourceValues,
	resourceID, token string,
) error {
	if resourceID == "" {
		opts.sayf("would store the %s credential for %s", resource.SecretEnv, resource.Identifier)
		return nil
	}
	secretID, err := session.FindSecret(ctx, resourceID)
	switch {
	case err != nil:
		return fmt.Errorf("finding the credential for %s: %w", resource.Identifier, err)
	case secretID == "" && opts.DryRun:
		opts.sayf("would store the %s credential for %s", resource.SecretEnv, resource.Identifier)
	case secretID == "":
		if err := session.CreateSecret(ctx, resourceID, resource.SecretEnv, token); err != nil {
			return fmt.Errorf("storing the credential for %s: %w", resource.Identifier, err)
		}
		opts.sayf("stored the %s credential for %s", resource.SecretEnv, resource.Identifier)
	case opts.DryRun:
		opts.sayf("would update the %s credential for %s", resource.SecretEnv, resource.Identifier)
	default:
		if err := session.UpdateSecret(ctx, secretID, token); err != nil {
			return fmt.Errorf("updating the credential for %s: %w", resource.Identifier, err)
		}
		opts.sayf("updated the %s credential for %s", resource.SecretEnv, resource.Identifier)
	}
	return nil
}

func addMissingDependencies(
	ctx context.Context,
	opts *Options,
	session *Session,
	applicationID string,
	resources []ResourceValues,
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
			opts.sayf("would add the resource for %s as a dependency", resources[i].Identifier)
			continue
		}
		if err := ensureDependency(ctx, opts, session, applicationID, resourceID, has[resourceID]); err != nil {
			return err
		}
	}
	return nil
}

func ensureDependency(
	ctx context.Context,
	opts *Options,
	session *Session,
	applicationID, resourceID string,
	has bool,
) error {
	switch {
	case has:
		opts.sayf("found resource %s as a dependency", resourceID)
	case opts.DryRun:
		opts.sayf("would add resource %s as a dependency", resourceID)
	default:
		if err := session.AddApplicationDependency(ctx, applicationID, resourceID); err != nil {
			return fmt.Errorf("adding a dependency: %w", err)
		}
		opts.sayf("added resource %s as a dependency", resourceID)
	}
	return nil
}

func ensureUserAccess(
	ctx context.Context,
	opts *Options,
	session *Session,
	applicationID, providerID string,
	values *Values,
) ([]UserAccess, error) {
	access := make([]UserAccess, 0, len(values.Users))
	for _, user := range values.Users {
		found, err := session.FindUser(ctx, user.Email)
		if err != nil {
			return nil, fmt.Errorf("finding the user %s: %w", user.Email, err)
		}
		opts.sayf("found user %s for %s", found.ID, user.Email)
		subject := ServiceAccountSubject(values.Namespace, found.ID)
		credentialID, err := ensureTokenCredential(ctx, opts, session, applicationID, providerID, subject)
		if err != nil {
			return nil, err
		}
		access = append(access, UserAccess{
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
	opts *Options,
	session *Session,
	applicationID, providerID, subject string,
) (string, error) {
	if applicationID == "" || providerID == "" {
		opts.sayf("would create a token credential for %s", subject)
		return "", nil
	}
	credentialID, err := session.FindTokenCredential(ctx, applicationID, providerID, subject)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the token credential for %s: %w", subject, err)
	case credentialID != "":
		opts.sayf("found token credential %s for %s", credentialID, subject)
		return credentialID, nil
	case opts.DryRun:
		opts.sayf("would create a token credential for %s", subject)
		return "", nil
	}
	if credentialID, err = session.CreateTokenCredential(ctx, applicationID, providerID, subject); err != nil {
		return "", fmt.Errorf("creating the token credential for %s: %w", subject, err)
	}
	opts.sayf("created token credential %s for %s", credentialID, subject)
	return credentialID, nil
}

// ensurePolicies makes the active policy set hold each user's policies. It
// removes from the set each policy of the namespace that the values file does
// not name, then archives it, so a removed user loses access. If the sandbox
// set is not active, it calls checkOtherActive before it writes anything.
func ensurePolicies(
	ctx context.Context,
	opts *Options,
	session *Session,
	values *Values,
	applicationName string,
	access []UserAccess,
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
		return archiveRemoved(ctx, opts, session, owned, nil)
	}
	if !set.Active {
		if err = checkOtherActive(ctx, opts, session, set.ID, values.PolicySet); err != nil {
			return err
		}
	}
	var policies []UserPolicy
	for _, user := range access {
		policyUser := keycard.User{ID: user.UserID, Identifier: user.UserIdentifier, Email: user.UserEmail}
		policies = append(policies,
			UserPolicies(values.Namespace, values.IssuerURL, applicationName, policyUser)...)
	}
	if err = ensurePolicySet(ctx, opts, session, set, values.PolicySet, policies, owned); err != nil {
		return err
	}
	return archiveRemoved(ctx, opts, session, owned, policies)
}

// archiveRemoved archives each owned policy that policies does not name.
// owned maps a policy name to the policy. Call it only after
// ensurePolicySet, so the active set never holds an archived policy.
func archiveRemoved(
	ctx context.Context,
	opts *Options,
	session *Session,
	owned map[string]keycard.Policy,
	policies []UserPolicy,
) error {
	for _, name := range slices.Sorted(maps.Keys(owned)) {
		policyID := owned[name].ID
		switch {
		case slices.ContainsFunc(policies, func(policy UserPolicy) bool { return policy.Name == name }):
		case opts.DryRun:
			opts.sayf("would archive policy %s for %q", policyID, name)
		default:
			if err := session.ArchivePolicy(ctx, policyID); err != nil {
				return fmt.Errorf("archiving the policy %q: %w", name, err)
			}
			opts.sayf("archived policy %s for %q", policyID, name)
		}
	}
	return nil
}

// ensurePolicySet applies and activates a version of the policy set that
// holds policies and no other owned policy. set has no ID when the set does
// not exist yet. owned maps a policy name to the policy the zone already
// has. If the set would be empty, it adds the placeholder policy.
//
// The apply starts from the manifest baseManifest returns and uses that
// version's schema. The zone validates each policy's Cedar against that
// schema, reuses the version of an unchanged policy, and publishes a new
// version of a changed one. NEVER publish a policy version separately.
// svc-pdp rejects a set version that pins a policy version with a different
// schema.
func ensurePolicySet(
	ctx context.Context,
	opts *Options,
	session *Session,
	set keycard.PolicySetWithBinding,
	name string,
	policies []UserPolicy,
	owned map[string]keycard.Policy,
) error {
	switch {
	case set.ID == "" && opts.DryRun:
		opts.sayf("would create the policy set %q with %d policies and activate it", name, len(policies))
		return nil
	case set.ID == "":
		setID, err := session.CreatePolicySet(ctx, name)
		if err != nil {
			return fmt.Errorf("creating the policy set %q: %w", name, err)
		}
		set = keycard.PolicySetWithBinding{ID: setID}
		opts.sayf("created policy set %s for %q", setID, name)
	}
	base, etag, err := baseManifest(ctx, session, set)
	if err != nil {
		return fmt.Errorf("reading the policy set %s: %w", set.ID, err)
	}
	entries, err := desiredEntries(ctx, opts, session, base.Entries, policies, owned)
	if err != nil {
		return err
	}
	if opts.DryRun {
		opts.sayf("would apply policy set %s with %d policies and activate it", set.ID, len(entries))
		return nil
	}
	applied, err := session.ApplyPolicySet(ctx, set.ID, etag, base.SchemaVersion, entries)
	if err != nil {
		return fmt.Errorf("applying the policy set %s: %w", set.ID, err)
	}
	reportApply(opts, set.ID, applied, len(entries))
	return nil
}

// reportApply logs each policy the apply did not reuse, and the version it
// activated.
func reportApply(opts *Options, setID string, applied ApplyResult, policies int) {
	for _, change := range applied.Changes {
		if change.Action != "reused" {
			opts.sayf("policy %s for %q: %s", change.PolicyID, change.Name, change.Action)
		}
	}
	if applied.Created {
		opts.sayf("published policy set %s version %s with %d policies", setID, applied.VersionID, policies)
	}
	opts.sayf("policy set %s version %s is active", setID, applied.VersionID)
}

// baseManifest returns the manifest of the set's active version, else of its
// latest version, and the latest version's ETag.
func baseManifest(
	ctx context.Context,
	session *Session,
	set keycard.PolicySetWithBinding,
) (Manifest, string, error) {
	latest, etag, err := session.PolicySetManifest(ctx, set.ID, "")
	if err != nil || set.ActiveVersionID == "" || set.ActiveVersionID == latest.VersionID {
		return latest, etag, err
	}
	active, _, err := session.PolicySetManifest(ctx, set.ID, set.ActiveVersionID)
	return active, etag, err
}

// checkOtherActive refuses to replace another active policy set unless
// opts.ReplaceActive is set. ownID is the sandbox set's id, or "" when the
// set does not exist yet.
func checkOtherActive(ctx context.Context, opts *Options, session *Session, ownID, name string) error {
	active, err := session.ActivePolicySets(ctx)
	if err != nil {
		return fmt.Errorf("listing the active policy sets: %w", err)
	}
	for _, other := range active {
		switch {
		case other.ID == ownID:
		case opts.ReplaceActive:
			opts.sayf("policy set %q (%s) is active and will be replaced", other.Name, other.ID)
		default:
			return fmt.Errorf("%w: %q (%s). Activating %q would replace it. Re-run with --%s to do that",
				ErrPolicySetActive, other.Name, other.ID, name, ReplaceActiveFlag)
		}
	}
	return nil
}

// mergeManifest returns the entries of an apply that starts from base. An
// owned policy that policies names gets its new Cedar text. Other owned
// policies are dropped. Every other entry keeps its pinned version. A policy
// that base does not hold goes at the end. The apply creates it if owned
// does not have it.
func mergeManifest(
	base []ManifestEntry,
	policies []UserPolicy,
	owned map[string]keycard.Policy,
) []ApplyEntry {
	ownedIDs := map[string]bool{}
	for _, policy := range owned {
		ownedIDs[policy.ID] = true
	}
	cedar := map[string]string{}
	for _, policy := range policies {
		if found, ok := owned[policy.Name]; ok {
			cedar[found.ID] = policy.Cedar
		}
	}
	out := make([]ApplyEntry, 0, len(base)+len(policies))
	inBase := map[string]bool{}
	for _, entry := range base {
		text, wanted := cedar[entry.PolicyID]
		switch {
		case wanted:
			out = append(out, ApplyEntry{PolicyID: entry.PolicyID, CedarRaw: text})
			inBase[entry.PolicyID] = true
		case !ownedIDs[entry.PolicyID]:
			out = append(out, ApplyEntry{PolicyID: entry.PolicyID, PolicyVersionID: entry.PolicyVersionID})
		}
	}
	for _, policy := range policies {
		if found, ok := owned[policy.Name]; !ok || !inBase[found.ID] {
			out = append(out, newEntry(policy, found))
		}
	}
	return out
}

// desiredEntries returns the entries of an apply that starts from base. It
// drops each entry for a policy the zone has archived, merges policies into
// the rest, and adds the placeholder policy when nothing is left.
func desiredEntries(
	ctx context.Context,
	opts *Options,
	session *Session,
	base []ManifestEntry,
	policies []UserPolicy,
	owned map[string]keycard.Policy,
) ([]ApplyEntry, error) {
	live, err := dropArchived(ctx, opts, session, base, owned)
	if err != nil {
		return nil, err
	}
	return withPlaceholder(ctx, session, mergeManifest(live, policies, owned))
}

// dropArchived returns base without the entries for policies the zone has
// archived. svc-pdp refuses an apply that pins an archived policy. An
// archived policy is not meant to be enforced, so dropping its entry is
// safe. Owned policies are not archived and are not looked up.
func dropArchived(
	ctx context.Context,
	opts *Options,
	session *Session,
	base []ManifestEntry,
	owned map[string]keycard.Policy,
) ([]ManifestEntry, error) {
	ownedIDs := map[string]bool{}
	for _, policy := range owned {
		ownedIDs[policy.ID] = true
	}
	var others []string
	for _, entry := range base {
		if !ownedIDs[entry.PolicyID] {
			others = append(others, entry.PolicyID)
		}
	}
	if len(others) == 0 {
		return base, nil
	}
	live, err := session.LivePolicyIDs(ctx, others)
	if err != nil {
		return nil, fmt.Errorf("reading the policies the set pins: %w", err)
	}
	kept := make([]ManifestEntry, 0, len(base))
	for _, entry := range base {
		if ownedIDs[entry.PolicyID] || live[entry.PolicyID] {
			kept = append(kept, entry)
			continue
		}
		opts.sayf("dropping policy %s from the set: the zone has no unarchived policy with that id", entry.PolicyID)
	}
	return kept, nil
}

// withPlaceholder returns entries, or the placeholder policy's entry when
// entries is empty. The apply creates the placeholder when the zone does not
// have it.
func withPlaceholder(
	ctx context.Context,
	session *Session,
	entries []ApplyEntry,
) ([]ApplyEntry, error) {
	if len(entries) > 0 {
		return entries, nil
	}
	placeholder := PlaceholderPolicy()
	found, err := session.FindPolicy(ctx, placeholder.Name)
	if err != nil {
		return nil, fmt.Errorf("finding the policy %q: %w", placeholder.Name, err)
	}
	return []ApplyEntry{newEntry(placeholder, found)}, nil
}

// newEntry returns the entry for a policy the set does not hold. found has
// no ID when the zone does not have the policy. The apply then creates it.
func newEntry(policy UserPolicy, found keycard.Policy) ApplyEntry {
	if found.ID != "" {
		return ApplyEntry{PolicyID: found.ID, CedarRaw: policy.Cedar}
	}
	return ApplyEntry{
		NewPolicy: &NewPolicy{Name: policy.Name, Description: policy.Description},
		CedarRaw:  policy.Cedar,
	}
}

func (opts *Options) sayf(format string, args ...any) {
	if opts.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(opts.Log, format+"\n", args...)
}
