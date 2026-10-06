package zonesetup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/keycardai/keycard-go"
)

// digestAnnotation names the Cedar annotation that holds a policy's digest.
// zone-setup compares it to find an unchanged policy. The zone stores a
// version as Cedar JSON, keeps its annotations, and returns its text
// reformatted.
const digestAnnotation = "sandbox_digest"

const (
	impersonationKind = "impersonation"
	accessKind        = "access"
)

// A UserPolicy is one Cedar policy zone-setup publishes for one user.
type UserPolicy struct {
	Name        string
	Description string
	// Cedar starts with the `@id` and sandbox_digest annotations.
	Cedar string
	// Digest is the hex SHA-256 of Cedar without its sandbox_digest line.
	Digest string
}

// UserPolicies returns the impersonation and access policies for one user of
// the namespace's sandboxes. issuer is the cluster's OIDC issuer URL. user
// needs its ID and Identifier.
//
// An exchange goes through two checks, and both must allow it. The subject
// check asks whether the user may reach the resource, and AccessPolicy
// answers it. The actor check asks whether the Application may impersonate
// the user for that resource, and ImpersonationPolicy answers it.
func UserPolicies(namespace, issuer string, user keycard.User) []UserPolicy {
	return []UserPolicy{ImpersonationPolicy(namespace, issuer, user), AccessPolicy(namespace, issuer, user)}
}

// AccessPolicy is the Cedar policy that lets the user reach resources through
// the sandbox. The zone runs the subject check with the user as principal,
// impersonate false, and the actor's token claims in context. The policy
// matches only when the actor token is the user's ServiceAccount token from
// the cluster's issuer. ImpersonationPolicy limits the resources.
func AccessPolicy(namespace, issuer string, user keycard.User) UserPolicy {
	return newUserPolicy(accessKind, namespace, user.ID,
		fmt.Sprintf("Lets user %s reach resources through the sandbox in namespace %s", user.ID, namespace),
		fmt.Sprintf(`permit (
  principal is Keycard::User,
  action,
  resource
) when {
  principal.identifier == %q &&
  context has actor_claims &&
  context.actor_claims has issuer_claims &&
  context.actor_claims.issuer_claims.contains({"key": "iss", "value": %q}) &&
  context.actor_claims.issuer_claims.contains({"key": "sub", "value": %q})
};
`, user.Identifier, issuer, ServiceAccountSubject(namespace, user.ID)))
}

// ImpersonationPolicy is the Cedar policy that lets the namespace's egress
// proxy Application impersonate one user. It permits only the Application's
// dependencies, and only with that user's ServiceAccount token from the
// cluster's issuer. The API server signs the ServiceAccount token, so a pod
// cannot forge it.
//
// The Application's dependencies in Keycard are the resources a sandbox may
// reach. Adding a dependency grants it to every enrolled user. NEVER list
// resources in the policy.
//
// Every context attribute the policy reads is optional in the zone's Cedar
// schema. Guard each read with `has`. An unguarded read fails validation on
// publish with "unable to guarantee safety of access to optional attribute".
// `identifier` and `dependencies` are required attributes of
// Keycard::Application in the zone's default schema.
func ImpersonationPolicy(namespace, issuer string, user keycard.User) UserPolicy {
	application := ApplicationIdentifier(namespace)
	return newUserPolicy(impersonationKind, namespace, user.ID,
		fmt.Sprintf("Lets Application %s impersonate user %s for its dependencies", application, user.ID),
		fmt.Sprintf(`permit (
  principal is Keycard::Application,
  action,
  resource
) when {
  principal.identifier == %q &&
  principal.dependencies.contains(resource) &&
  context has impersonate && context.impersonate == true &&
  context has subject &&
  context.subject.identifier == %q &&
  context has actor_claims &&
  context.actor_claims has issuer_claims &&
  context.actor_claims.issuer_claims.contains({"key": "iss", "value": %q}) &&
  context.actor_claims.issuer_claims.contains({"key": "sub", "value": %q})
};
`, application, user.Identifier, issuer, ServiceAccountSubject(namespace, user.ID)))
}

// newUserPolicy names a policy by its kind, namespace and user, and adds the
// `@id` and sandbox_digest annotations to body. The zone keys the policy by
// its `@id` in the Cedar JSON it stores.
func newUserPolicy(kind, namespace, userID, description, body string) UserPolicy {
	text := fmt.Sprintf("@id(%q)\n%s", "sandbox-"+kind+"/"+namespace+"/"+userID, body)
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	return UserPolicy{
		Name:        policyNamePrefix(kind, namespace) + userID,
		Description: description,
		Cedar:       fmt.Sprintf("@%s(%q)\n%s", digestAnnotation, digest, text),
		Digest:      digest,
	}
}

func policyNamePrefix(kind, namespace string) string {
	return "Sandbox " + kind + ": " + namespace + "/"
}

// SandboxPolicies returns the zone's unarchived policies that zone-setup
// publishes for the namespace, keyed by name. Every error wraps ErrAPI or
// ErrPolicyAmbiguous.
func (s *Session) SandboxPolicies(ctx context.Context, namespace string) (map[string]keycard.Policy, error) {
	prefixes := []string{policyNamePrefix(impersonationKind, namespace), policyNamePrefix(accessKind, namespace)}
	all, err := s.listPolicies(ctx, prefixes)
	if err != nil {
		return nil, err
	}
	found := map[string]keycard.Policy{}
	for _, policy := range all {
		owned := slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(policy.Name, prefix) })
		if !owned || !policy.ArchivedAt.IsZero() {
			continue
		}
		if policy.ID == "" {
			return nil, fmt.Errorf("%w: the policy %q has no id", ErrAPI, policy.Name)
		}
		if _, ok := found[policy.Name]; ok {
			return nil, fmt.Errorf("%w: more than one policy is named %q", ErrPolicyAmbiguous, policy.Name)
		}
		found[policy.Name] = policy
	}
	return found, nil
}

// CreatePolicy creates a policy with no version and returns its id.
func (s *Session) CreatePolicy(ctx context.Context, name, description string) (string, error) {
	policy, err := s.zones.Policies.New(ctx, s.Zone.ID, keycard.ZonePolicyNewParams{
		Name:        name,
		Description: keycard.String(description),
	})
	if err != nil {
		return "", fmt.Errorf("%w: create policy %q: %w", ErrAPI, name, requestError(err))
	}
	if policy == nil || policy.ID == "" {
		return "", fmt.Errorf("%w: create policy %q: the response holds no policy id", ErrAPI, name)
	}
	return policy.ID, nil
}

// ArchivePolicy archives the policy. Every error wraps ErrAPI.
func (s *Session) ArchivePolicy(ctx context.Context, policyID string) error {
	policy, err := s.zones.Policies.Archive(ctx, policyID, keycard.ZonePolicyArchiveParams{ZoneID: s.Zone.ID})
	if err != nil {
		return fmt.Errorf("%w: archive policy %s: %w", ErrAPI, policyID, requestError(err))
	}
	if policy == nil || policy.ID != policyID || policy.ArchivedAt.IsZero() {
		return fmt.Errorf("%w: archive policy %s: the response holds another policy or one not archived",
			ErrAPI, policyID)
	}
	return nil
}

// LatestPolicyDigest returns the id and sandbox_digest annotation of the
// policy's newest version. The id is "" when the policy has no version. The
// digest is "" when the version does not hold exactly one policy with that
// annotation. Every error wraps ErrAPI.
func (s *Session) LatestPolicyDigest(ctx context.Context, policyID string) (string, string, error) {
	page, err := s.zones.Policies.Versions.List(ctx, policyID, keycard.ZonePolicyVersionListParams{
		ZoneID: s.Zone.ID,
		Limit:  keycard.Int(1),
		Order:  keycard.ZonePolicyVersionListParamsOrderDesc,
		Format: keycard.ZonePolicyVersionListParamsFormatJson,
	})
	if err != nil {
		return "", "", fmt.Errorf("%w: list versions of policy %s: %w", ErrAPI, policyID, requestError(err))
	}
	if page == nil || !page.JSON.Items.Valid() {
		return "", "", fmt.Errorf("%w: list versions of policy %s: %w", ErrAPI, policyID, errNoPage)
	}
	if len(page.Items) == 0 {
		return "", "", nil
	}
	version := page.Items[0]
	if version.ID == "" || version.PolicyID != policyID {
		return "", "", fmt.Errorf("%w: a version listed for policy %s has no id or another policy_id",
			ErrAPI, policyID)
	}
	return version.ID, policyDigest(version.JSON.CedarJson.Raw()), nil
}

// policyDigest returns the sandbox_digest annotation of the one static policy
// in a version's Cedar JSON, or "".
func policyDigest(cedarJSON string) string {
	var set struct {
		StaticPolicies map[string]struct {
			Annotations map[string]*string `json:"annotations"`
		} `json:"staticPolicies"`
		TemplateLinks []json.RawMessage `json:"templateLinks"`
	}
	if json.Unmarshal([]byte(cedarJSON), &set) != nil || len(set.StaticPolicies) != 1 || len(set.TemplateLinks) > 0 {
		return ""
	}
	for _, policy := range set.StaticPolicies {
		if digest := policy.Annotations[digestAnnotation]; digest != nil {
			return *digest
		}
	}
	return ""
}

// CreatePolicyVersion publishes cedar as the policy's next version, validated
// against the zone's default schema, and returns the version id. A version is
// immutable, so a change to the text always publishes a new one.
func (s *Session) CreatePolicyVersion(ctx context.Context, policyID, cedar string) (string, error) {
	schema, err := s.defaultSchemaVersion(ctx)
	if err != nil {
		return "", err
	}
	version, err := s.zones.Policies.Versions.New(ctx, policyID, keycard.ZonePolicyVersionNewParams{
		ZoneID:        s.Zone.ID,
		SchemaVersion: schema,
		CedarRaw:      keycard.String(cedar),
	})
	if err != nil {
		return "", fmt.Errorf("%w: create a version of policy %s: %w", ErrAPI, policyID, requestError(err))
	}
	if version == nil || version.ID == "" {
		return "", fmt.Errorf("%w: create a version of policy %s: the response holds no version id", ErrAPI, policyID)
	}
	return version.ID, nil
}

// FindPolicySet returns the zone's unarchived policy set named name. Its ID
// is "" when no set matches. Every error wraps ErrAPI or
// ErrPolicySetAmbiguous.
func (s *Session) FindPolicySet(ctx context.Context, name string) (keycard.PolicySetWithBinding, error) {
	all, err := s.listPolicySets(ctx, keycard.ZonePolicySetListParams{QueryName: []string{name}})
	if err != nil {
		return keycard.PolicySetWithBinding{}, err
	}
	var found []keycard.PolicySetWithBinding
	for _, set := range all {
		if set.Name == name && set.ArchivedAt.IsZero() {
			found = append(found, set)
		}
	}
	switch {
	case len(found) > 1:
		return keycard.PolicySetWithBinding{}, fmt.Errorf("%w: %d policy sets are named %q",
			ErrPolicySetAmbiguous, len(found), name)
	case len(found) == 1 && found[0].ID == "":
		return keycard.PolicySetWithBinding{}, fmt.Errorf("%w: the policy set %q has no id", ErrAPI, name)
	case len(found) == 1:
		return found[0], nil
	default:
		return keycard.PolicySetWithBinding{}, nil
	}
}

// ActivePolicySets returns the zone's zone-wide policy sets that have an
// active binding. Activating the sandbox set does not replace a
// user-targeted set. Every error wraps ErrAPI.
func (s *Session) ActivePolicySets(ctx context.Context) ([]keycard.PolicySetWithBinding, error) {
	all, err := s.listPolicySets(ctx, keycard.ZonePolicySetListParams{FilterActive: keycard.Bool(true)})
	if err != nil {
		return nil, err
	}
	var active []keycard.PolicySetWithBinding
	for _, set := range all {
		if set.Active && set.TargetType == keycard.PolicySetTargetTypeZone {
			active = append(active, set)
		}
	}
	return active, nil
}

// CreatePolicySet creates a zone-wide policy set with no version and returns
// its id.
func (s *Session) CreatePolicySet(ctx context.Context, name string) (string, error) {
	set, err := s.zones.PolicySets.New(ctx, s.Zone.ID, keycard.ZonePolicySetNewParams{
		Name:       name,
		TargetType: keycard.ZonePolicySetNewParamsTargetTypeZone,
	})
	if err != nil {
		return "", fmt.Errorf("%w: create policy set %q: %w", ErrAPI, name, requestError(err))
	}
	if set == nil || set.ID == "" {
		return "", fmt.Errorf("%w: create policy set %q: the response holds no policy set id", ErrAPI, name)
	}
	return set.ID, nil
}

// PolicySetManifest returns the entries of one version of the policy set.
// Every error wraps ErrAPI.
func (s *Session) PolicySetManifest(
	ctx context.Context,
	setID, versionID string,
) ([]keycard.PolicySetManifestEntry, error) {
	version, err := s.zones.PolicySets.Versions.Get(ctx, versionID, keycard.ZonePolicySetVersionGetParams{
		ZoneID: s.Zone.ID, PolicySetID: setID,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: get policy set version %s: %w", ErrAPI, versionID, requestError(err))
	}
	if version == nil || version.ID != versionID || version.PolicySetID != setID ||
		!version.Manifest.JSON.Entries.Valid() {
		return nil, fmt.Errorf("%w: get policy set version %s: the response holds another version or no manifest",
			ErrAPI, versionID)
	}
	return version.Manifest.Entries, nil
}

// CreatePolicySetVersion publishes a version of the policy set that holds
// entries, validated against the zone's default schema, and returns the
// version id. The version is not active until ActivatePolicySetVersion.
func (s *Session) CreatePolicySetVersion(
	ctx context.Context,
	setID string,
	entries []keycard.PolicySetManifestEntryParam,
) (string, error) {
	schema, err := s.defaultSchemaVersion(ctx)
	if err != nil {
		return "", err
	}
	version, err := s.zones.PolicySets.Versions.New(ctx, setID, keycard.ZonePolicySetVersionNewParams{
		ZoneID:        s.Zone.ID,
		SchemaVersion: schema,
		Manifest:      keycard.PolicySetManifestParam{Entries: entries},
	})
	if err != nil {
		return "", fmt.Errorf("%w: create a version of policy set %s: %w", ErrAPI, setID, requestError(err))
	}
	if version == nil || version.ID == "" {
		return "", fmt.Errorf("%w: create a version of policy set %s: the response holds no version id",
			ErrAPI, setID)
	}
	return version.ID, nil
}

// ActivatePolicySetVersion binds the version as the zone's active policy
// set. A zone has one active zone-wide policy set, so this replaces the
// binding of any other zone-wide set.
func (s *Session) ActivatePolicySetVersion(ctx context.Context, setID, versionID string) error {
	version, err := s.zones.PolicySets.Versions.Update(ctx, versionID, keycard.ZonePolicySetVersionUpdateParams{
		ZoneID: s.Zone.ID, PolicySetID: setID, Active: true,
	})
	if err != nil {
		return fmt.Errorf("%w: activate policy set version %s: %w", ErrAPI, versionID, requestError(err))
	}
	if version == nil || version.ID != versionID {
		return fmt.Errorf("%w: activate policy set version %s: the response holds another version or none",
			ErrAPI, versionID)
	}
	return nil
}

// defaultSchemaVersion returns the zone's default Cedar schema version, which
// new policy and policy set versions are validated against. Every error wraps
// ErrAPI or ErrPolicySchemaNotFound.
func (s *Session) defaultSchemaVersion(ctx context.Context) (string, error) {
	if s.schemaVersion != "" {
		return s.schemaVersion, nil
	}
	all, err := listAll("policy schemas", func(params pageParams) (listPage[keycard.SchemaVersionWithZoneInfo], error) {
		page, err := s.zones.PolicySchemas.List(ctx, s.Zone.ID, keycard.ZonePolicySchemaListParams{
			After: params.After, Limit: params.Limit, FilterDefault: keycard.Bool(true),
		})
		if err != nil || page == nil {
			//nolint:wrapcheck // listAll passes it through requestError.
			return listPage[keycard.SchemaVersionWithZoneInfo]{}, err
		}
		return listPage[keycard.SchemaVersionWithZoneInfo]{
			items: page.Items, after: page.Pagination.AfterCursor,
			complete: page.JSON.Items.Valid() && page.JSON.Pagination.Valid(),
		}, nil
	})
	if err != nil {
		return "", err
	}
	for _, schema := range all {
		if schema.IsDefault && schema.Version != "" {
			s.schemaVersion = schema.Version
			return schema.Version, nil
		}
	}
	return "", fmt.Errorf("%w: the zone has no default policy schema", ErrPolicySchemaNotFound)
}

func (s *Session) listPolicies(ctx context.Context, names []string) ([]keycard.Policy, error) {
	return listAll("policies", func(params pageParams) (listPage[keycard.Policy], error) {
		page, err := s.zones.Policies.List(ctx, s.Zone.ID, keycard.ZonePolicyListParams{
			After: params.After, Limit: params.Limit, QueryName: names,
		})
		if err != nil || page == nil {
			return listPage[keycard.Policy]{}, err //nolint:wrapcheck // listAll passes it through requestError.
		}
		return listPage[keycard.Policy]{
			items: page.Items, after: page.Pagination.AfterCursor,
			complete: page.JSON.Items.Valid() && page.JSON.Pagination.Valid(),
		}, nil
	})
}

func (s *Session) listPolicySets(
	ctx context.Context,
	filter keycard.ZonePolicySetListParams,
) ([]keycard.PolicySetWithBinding, error) {
	return listAll("policy sets", func(params pageParams) (listPage[keycard.PolicySetWithBinding], error) {
		filter.After, filter.Limit = params.After, params.Limit
		page, err := s.zones.PolicySets.List(ctx, s.Zone.ID, filter)
		if err != nil || page == nil {
			return listPage[keycard.PolicySetWithBinding]{}, err //nolint:wrapcheck // listAll passes it through requestError.
		}
		return listPage[keycard.PolicySetWithBinding]{
			items: page.Items, after: page.Pagination.AfterCursor,
			complete: page.JSON.Items.Valid() && page.JSON.Pagination.Valid(),
		}, nil
	})
}
