package zonesetup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/keycardai/keycard-go"
	"github.com/keycardai/keycard-go/packages/param"
)

// ErrPolicySetActive is a zone whose active policy set is not the sandbox
// set. The run does not take the slot unless told to.
var ErrPolicySetActive = errors.New("another policy set is active")

// SandboxUserAccessOutputs are one per-user install's outputs.
type SandboxUserAccessOutputs struct {
	UserEmail      string `json:"user_email"`
	UserID         string `json:"user_id"`
	ServiceAccount string `json:"service_account"`
	ClientID       string `json:"client_id"`
	Namespace      string `json:"namespace"`
}

// ensureUsers performs the per-user steps for every user in the values
// file and returns their outputs and the policy versions the policy set
// must hold.
func (s *Setup) ensureUsers(
	ctx context.Context,
	zoneID, appID, providerID string,
) ([]SandboxUserAccessOutputs, []manifestEntry, error) {
	outputs := make([]SandboxUserAccessOutputs, 0, len(s.Values.Users))
	entries := make([]manifestEntry, 0, len(s.Values.Users))
	for _, user := range s.Values.Users {
		userID, err := s.resolveUser(ctx, zoneID, user.Email)
		if err != nil {
			return nil, nil, fmt.Errorf("user %s: %w", user.Email, err)
		}
		subject := ServiceAccountSubject(s.Values.Namespace, userID)
		credentialID, err := s.ensureCredential(ctx, zoneID, appID, providerID, subject)
		if err != nil {
			return nil, nil, fmt.Errorf("user %s: credential: %w", user.Email, err)
		}
		entry, err := s.ensurePolicy(ctx, zoneID, user.Email, userID)
		if err != nil {
			return nil, nil, fmt.Errorf("user %s: policy: %w", user.Email, err)
		}
		entries = append(entries, entry)
		outputs = append(outputs, SandboxUserAccessOutputs{
			UserEmail:      user.Email,
			UserID:         userID,
			ServiceAccount: subject,
			ClientID:       credentialID,
			Namespace:      s.Values.Namespace,
		})
	}
	return outputs, entries, nil
}

// resolveUser is the public id of the existing user with this email. The
// user must already exist in the zone. Nothing here creates one.
func (s *Setup) resolveUser(ctx context.Context, zoneID, email string) (string, error) {
	res, err := s.zones.Users.List(ctx, zoneID, keycard.ZoneUserListParams{
		FilterEmail: keycard.ZoneUserListParamsFilterEmailUnion{OfString: param.NewOpt(email)},
	})
	if err != nil {
		return "", apiErrorf(err, "listing users")
	}
	user, found, err := exactlyOne(res.Items, func(user keycard.User) bool {
		return strings.EqualFold(user.Email, email)
	}, "users have email "+email)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: user with email %s. Create the user first", ErrNotFound, email)
	}
	s.sayf("user %s is %s", email, user.ID)
	return user.ID, nil
}

// ensureCredential creates or finds the workload-identity credential: a
// token credential on the Application, verified against the cluster
// Provider, bound to the user's ServiceAccount subject.
func (s *Setup) ensureCredential(ctx context.Context, zoneID, appID, providerID, subject string) (string, error) {
	if appID == pendingID || providerID == pendingID {
		s.sayf("would create token credential for %s", subject)
		return pendingID, nil
	}
	credentialID, found, err := s.findTokenCredential(ctx, zoneID, appID, providerID, subject)
	if err != nil {
		return "", err
	}
	if found {
		s.sayf("token credential for %s is %s", subject, credentialID)
		return credentialID, nil
	}
	if s.DryRun {
		s.sayf("would create token credential for %s", subject)
		return pendingID, nil
	}
	credential, err := s.zones.ApplicationCredentials.New(ctx, zoneID, keycard.ZoneApplicationCredentialNewParams{
		OfApplicationCredentialCreateToken: &keycard.ZoneApplicationCredentialNewParamsBodyApplicationCredentialCreateToken{
			ApplicationID: appID,
			ProviderID:    providerID,
			Type:          "token",
			Subject:       param.NewOpt(subject),
		},
	})
	if err != nil {
		return "", apiErrorf(err, "creating token credential for %s", subject)
	}
	s.sayf("created token credential for %s as %s", subject, credential.ID)
	return credential.ID, nil
}

// findTokenCredential is the id of the application's token credential for
// this provider and subject, if there is one.
func (s *Setup) findTokenCredential(
	ctx context.Context,
	zoneID, appID, providerID, subject string,
) (string, bool, error) {
	credentials, err := listAll(func(after string) ([]keycard.CredentialUnion, string, error) {
		res, err := s.zones.ApplicationCredentials.List(ctx, zoneID, keycard.ZoneApplicationCredentialListParams{
			ApplicationID: param.NewOpt(appID), After: optString(after), Limit: param.NewOpt[int64](listPageSize),
		})
		if err != nil {
			return nil, "", apiErrorf(err, "listing credentials of application %s", appID)
		}
		return res.Items, next(res.PageInfo.HasNextPage, res.Pagination.AfterCursor), nil
	})
	if err != nil {
		return "", false, err
	}
	for _, credential := range credentials {
		if credential.Type == "token" && credential.ProviderID == providerID && credential.Subject == subject {
			return credential.ID, true, nil
		}
	}
	return "", false, nil
}

// manifestEntry is one policy version in the sandbox policy set.
type manifestEntry struct {
	policyID  string
	versionID string
}

// pending reports whether a dry run left the entry without ids.
func (e manifestEntry) pending() bool { return e.policyID == pendingID || e.versionID == pendingID }

// ensurePolicy creates or finds the user's impersonation policy and makes
// sure its latest version holds the policy text the contract requires. A
// version is immutable, so a change publishes a new one.
func (s *Setup) ensurePolicy(ctx context.Context, zoneID, email, userID string) (manifestEntry, error) {
	name := PolicyName(userID)
	cedar := ImpersonationPolicy(s.Values.Namespace, userID)
	policy, found, err := s.findPolicy(ctx, zoneID, name)
	if err != nil {
		return manifestEntry{}, err
	}
	if !found {
		return s.createPolicy(ctx, zoneID, name, email, cedar)
	}
	latest, published, err := s.latestPolicyVersion(ctx, zoneID, policy.ID)
	if err != nil {
		return manifestEntry{}, err
	}
	if published && strings.TrimSpace(latest.CedarRaw) == strings.TrimSpace(cedar) {
		s.sayf("policy %s version %d holds the impersonation policy", policy.ID, latest.Version)
		return manifestEntry{policyID: policy.ID, versionID: latest.ID}, nil
	}
	if s.DryRun {
		s.sayf("would publish a new version of policy %s", policy.ID)
		return manifestEntry{policyID: policy.ID}, nil
	}
	return s.publishPolicyVersion(ctx, zoneID, policy.ID, cedar)
}

func (s *Setup) createPolicy(ctx context.Context, zoneID, name, email, cedar string) (manifestEntry, error) {
	if s.DryRun {
		s.sayf("would create policy %q and publish its version", name)
		return manifestEntry{}, nil
	}
	policy, err := s.zones.Policies.New(ctx, zoneID, keycard.ZonePolicyNewParams{
		Name:        name,
		Description: param.NewOpt(fmt.Sprintf("Lets the sandbox of %s impersonate that user, and nobody else", email)),
	})
	if err != nil {
		return manifestEntry{}, apiErrorf(err, "creating policy %q", name)
	}
	s.sayf("created policy %q as %s", name, policy.ID)
	return s.publishPolicyVersion(ctx, zoneID, policy.ID, cedar)
}

// findPolicy is the unarchived policy with exactly this name, if there is
// one.
func (s *Setup) findPolicy(ctx context.Context, zoneID, name string) (keycard.Policy, bool, error) {
	return findNamed(func(after string) ([]keycard.Policy, string, error) {
		res, err := s.zones.Policies.List(ctx, zoneID, keycard.ZonePolicyListParams{
			QueryName: []string{name}, After: optString(after), Limit: param.NewOpt[int64](listPageSize),
		})
		if err != nil {
			return nil, "", apiErrorf(err, "listing policies")
		}
		return res.Items, res.Pagination.AfterCursor, nil
	}, func(policy keycard.Policy) bool {
		return policy.Name == name && policy.ArchivedAt.IsZero()
	}, fmt.Sprintf("policies are named %q", name))
}

// latestPolicyVersion is the policy's newest version with its Cedar text,
// if one is published.
func (s *Setup) latestPolicyVersion(ctx context.Context, zoneID, policyID string) (keycard.PolicyVersion, bool, error) {
	versions, err := s.zones.Policies.Versions.List(ctx, policyID, keycard.ZonePolicyVersionListParams{
		ZoneID: zoneID, Limit: param.NewOpt[int64](1), Format: keycard.ZonePolicyVersionListParamsFormatCedar,
	})
	if err != nil {
		return keycard.PolicyVersion{}, false, apiErrorf(err, "listing versions of policy %s", policyID)
	}
	if len(versions.Items) == 0 {
		return keycard.PolicyVersion{}, false, nil
	}
	return versions.Items[0], true, nil
}

func (s *Setup) publishPolicyVersion(ctx context.Context, zoneID, policyID, cedar string) (manifestEntry, error) {
	schema, err := s.defaultSchemaVersion(ctx, zoneID)
	if err != nil {
		return manifestEntry{}, err
	}
	version, err := s.zones.Policies.Versions.New(ctx, policyID, keycard.ZonePolicyVersionNewParams{
		ZoneID:        zoneID,
		SchemaVersion: schema,
		CedarRaw:      param.NewOpt(cedar),
	})
	if err != nil {
		return manifestEntry{}, apiErrorf(err, "publishing a version of policy %s", policyID)
	}
	s.sayf("published policy %s version %d as %s", policyID, version.Version, version.ID)
	return manifestEntry{policyID: policyID, versionID: version.ID}, nil
}

// defaultSchemaVersion is the zone's default Cedar schema version, which new
// policy and policy set versions are validated against.
func (s *Setup) defaultSchemaVersion(ctx context.Context, zoneID string) (string, error) {
	if s.schemaVersion != "" {
		return s.schemaVersion, nil
	}
	res, err := s.zones.PolicySchemas.List(ctx, zoneID, keycard.ZonePolicySchemaListParams{
		FilterDefault: param.NewOpt(true),
	})
	if err != nil {
		return "", apiErrorf(err, "listing policy schemas")
	}
	for _, schema := range res.Items {
		if schema.IsDefault {
			s.schemaVersion = schema.Version
			return schema.Version, nil
		}
	}
	return "", fmt.Errorf("%w: default policy schema version in the zone", ErrNotFound)
}

// ensurePolicySet publishes the sandbox policy set version that holds
// every user's impersonation policy and activates it.
//
// A zone has one active policy set. Activating the sandbox set while another
// set is active replaces that set's binding, which is refused unless
// ReplaceActivePolicySet is set. The check runs before anything is written,
// and in a dry run too, so a dry run reports the same refusal the real run
// would.
func (s *Setup) ensurePolicySet(ctx context.Context, zoneID string, entries []manifestEntry) error {
	name := s.Values.PolicySet
	set, found, err := s.findPolicySet(ctx, zoneID, name)
	if err != nil {
		return err
	}
	if !found || !set.Active {
		if err = s.checkOtherActive(ctx, zoneID, set.ID); err != nil {
			return err
		}
	}
	for _, entry := range entries {
		if entry.pending() {
			s.sayf("would publish a version of policy set %q with %d impersonation policies and activate it",
				name, len(entries))
			return nil
		}
	}
	if !found {
		if s.DryRun {
			s.sayf("would create policy set %q, publish its version with %d impersonation policies and activate it",
				name, len(entries))
			return nil
		}
		set, err = s.createPolicySet(ctx, zoneID, name)
		if err != nil {
			return err
		}
	}
	return s.reconcilePolicySetVersion(ctx, zoneID, set, entries)
}

func (s *Setup) createPolicySet(ctx context.Context, zoneID, name string) (keycard.PolicySetWithBinding, error) {
	created, err := s.zones.PolicySets.New(ctx, zoneID, keycard.ZonePolicySetNewParams{
		Name:       name,
		TargetType: keycard.ZonePolicySetNewParamsTargetTypeZone,
	})
	if err != nil {
		return keycard.PolicySetWithBinding{}, apiErrorf(err, "creating policy set %q", name)
	}
	s.sayf("created policy set %q as %s", name, created.ID)
	return *created, nil
}

// findPolicySet is the unarchived policy set with exactly this name, if
// there is one.
func (s *Setup) findPolicySet(ctx context.Context, zoneID, name string) (keycard.PolicySetWithBinding, bool, error) {
	return findNamed(func(after string) ([]keycard.PolicySetWithBinding, string, error) {
		res, err := s.zones.PolicySets.List(ctx, zoneID, keycard.ZonePolicySetListParams{
			QueryName: []string{name}, After: optString(after), Limit: param.NewOpt[int64](listPageSize),
		})
		if err != nil {
			return nil, "", apiErrorf(err, "listing policy sets")
		}
		return res.Items, res.Pagination.AfterCursor, nil
	}, func(set keycard.PolicySetWithBinding) bool {
		return set.Name == name && set.ArchivedAt.IsZero()
	}, fmt.Sprintf("policy sets are named %q", name))
}

// reconcilePolicySetVersion makes the set's active version hold every
// entry. The manifest starts from the version that is active (or, for a set
// that is not active, its latest version), so entries this run does not
// manage are kept, and an entry for the same policy is moved to the version
// this run published. An unchanged manifest is activated as it is. A changed
// one is published as a new version and activated.
func (s *Setup) reconcilePolicySetVersion(
	ctx context.Context,
	zoneID string,
	set keycard.PolicySetWithBinding,
	entries []manifestEntry,
) error {
	baseVersionID, base, err := s.baseManifest(ctx, zoneID, set)
	if err != nil {
		return err
	}
	desired, changed := mergeManifest(base, entries)
	if !changed && baseVersionID != "" {
		if set.Active && set.ActiveVersionID == baseVersionID {
			s.sayf("policy set %s version %d is active and holds every impersonation policy", set.ID, set.ActiveVersion)
			return nil
		}
		return s.activate(ctx, zoneID, set.ID, baseVersionID, set.LatestVersion)
	}
	if s.DryRun {
		s.sayf("would publish a version of policy set %s with %d policies and activate it", set.ID, len(desired))
		return nil
	}
	return s.publishPolicySetVersion(ctx, zoneID, set.ID, desired)
}

// baseManifest is the manifest the next version starts from: the active
// version's, else the latest version's, else empty.
func (s *Setup) baseManifest(
	ctx context.Context,
	zoneID string,
	set keycard.PolicySetWithBinding,
) (string, []keycard.PolicySetManifestEntry, error) {
	baseVersionID := set.ActiveVersionID
	if baseVersionID == "" {
		baseVersionID = set.LatestVersionID
	}
	if baseVersionID == "" {
		return "", nil, nil
	}
	version, err := s.zones.PolicySets.Versions.Get(ctx, baseVersionID, keycard.ZonePolicySetVersionGetParams{
		ZoneID: zoneID, PolicySetID: set.ID,
	})
	if err != nil {
		return "", nil, apiErrorf(err, "reading policy set version %s", baseVersionID)
	}
	return baseVersionID, version.Manifest.Entries, nil
}

func (s *Setup) publishPolicySetVersion(
	ctx context.Context,
	zoneID, setID string,
	entries []keycard.PolicySetManifestEntryParam,
) error {
	schema, err := s.defaultSchemaVersion(ctx, zoneID)
	if err != nil {
		return err
	}
	version, err := s.zones.PolicySets.Versions.New(ctx, setID, keycard.ZonePolicySetVersionNewParams{
		ZoneID:        zoneID,
		SchemaVersion: schema,
		Manifest:      keycard.PolicySetManifestParam{Entries: entries},
	})
	if err != nil {
		return apiErrorf(err, "publishing a version of policy set %s", setID)
	}
	s.sayf("published policy set %s version %d as %s with %d policies", setID, version.Version, version.ID, len(entries))
	return s.activate(ctx, zoneID, setID, version.ID, version.Version)
}

// checkOtherActive refuses to take the zone's active policy set slot from
// another set unless the run was told to. ownID is the sandbox set's id, or
// empty when it does not exist yet.
func (s *Setup) checkOtherActive(ctx context.Context, zoneID, ownID string) error {
	res, err := s.zones.PolicySets.List(ctx, zoneID, keycard.ZonePolicySetListParams{
		FilterActive: param.NewOpt(true),
	})
	if err != nil {
		return apiErrorf(err, "listing active policy sets")
	}
	for _, other := range res.Items {
		if !other.Active || other.ID == ownID {
			continue
		}
		if s.ReplaceActivePolicySet {
			s.sayf("policy set %q (%s) is active and will be replaced", other.Name, other.ID)
			return nil
		}
		return fmt.Errorf("%w: %q (%s). Activating %q would replace it. Re-run with --replace-active-policy-set to do that",
			ErrPolicySetActive, other.Name, other.ID, s.Values.PolicySet)
	}
	return nil
}

func (s *Setup) activate(ctx context.Context, zoneID, setID, versionID string, version int64) error {
	if s.DryRun {
		s.sayf("would activate policy set %s version %d", setID, version)
		return nil
	}
	_, err := s.zones.PolicySets.Versions.Update(ctx, versionID, keycard.ZonePolicySetVersionUpdateParams{
		ZoneID: zoneID, PolicySetID: setID, Active: true,
	})
	if err != nil {
		return apiErrorf(err, "activating policy set version %s", versionID)
	}
	s.sayf("activated policy set %s version %d", setID, version)
	return nil
}

// mergeManifest is the base manifest with each entry's policy pointed at the
// version this run established, and any policy the base lacks appended.
// changed reports whether the result differs from base.
func mergeManifest(
	base []keycard.PolicySetManifestEntry,
	entries []manifestEntry,
) ([]keycard.PolicySetManifestEntryParam, bool) {
	out := make([]keycard.PolicySetManifestEntryParam, 0, len(base)+len(entries))
	index := map[string]int{}
	for _, entry := range base {
		index[entry.PolicyID] = len(out)
		out = append(
			out,
			keycard.PolicySetManifestEntryParam{PolicyID: entry.PolicyID, PolicyVersionID: entry.PolicyVersionID},
		)
	}
	changed := false
	for _, entry := range entries {
		if i, ok := index[entry.policyID]; ok {
			if out[i].PolicyVersionID != entry.versionID {
				out[i].PolicyVersionID = entry.versionID
				changed = true
			}
			continue
		}
		index[entry.policyID] = len(out)
		out = append(out, keycard.PolicySetManifestEntryParam{PolicyID: entry.policyID, PolicyVersionID: entry.versionID})
		changed = true
	}
	return out, changed
}

// findNamed walks a cursor-paginated list and returns the single item match
// accepts, as exactlyOne does.
//
//nolint:ireturn // T is a type parameter, so every call site gets a concrete type.
func findNamed[T any](
	page func(after string) ([]T, string, error),
	match func(T) bool,
	describe string,
) (T, bool, error) {
	items, err := listAll(page)
	if err != nil {
		var zero T
		return zero, false, err
	}
	return exactlyOne(items, match, describe)
}
