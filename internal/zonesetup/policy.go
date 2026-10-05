package zonesetup

import (
	"context"
	"fmt"

	"github.com/keycardai/keycard-go"
)

// PolicyName names the impersonation policy for one user.
func PolicyName(userID string) string {
	return "Sandbox impersonation: " + userID
}

// ImpersonationPolicy is the Cedar policy that lets exactly one
// ServiceAccount subject impersonate exactly one user. The API server signs
// the ServiceAccount token, so a pod cannot forge the actor half. A pod that
// names another user in its configuration fails the exchange with
// access_denied.
func ImpersonationPolicy(namespace, userID string) string {
	return fmt.Sprintf(`permit (principal, action, resource)
when {
  context.impersonate && context.subject.identifier == %q &&
  context.actor_claims.issuer_claims.contains({
    "key": "sub", "value": %q
  })
};
`, userID, ServiceAccountSubject(namespace, userID))
}

// FindPolicy returns the zone's unarchived policy named name. Its ID is ""
// when no policy matches. Every error wraps ErrAPI or ErrPolicyAmbiguous.
func (s *Session) FindPolicy(ctx context.Context, name string) (keycard.Policy, error) {
	all, err := s.listPolicies(ctx, name)
	if err != nil {
		return keycard.Policy{}, err
	}
	var found []keycard.Policy
	for _, policy := range all {
		if policy.Name == name && policy.ArchivedAt.IsZero() {
			found = append(found, policy)
		}
	}
	switch {
	case len(found) > 1:
		return keycard.Policy{}, fmt.Errorf("%w: %d policies are named %q", ErrPolicyAmbiguous, len(found), name)
	case len(found) == 1 && found[0].ID == "":
		return keycard.Policy{}, fmt.Errorf("%w: the policy %q has no id", ErrAPI, name)
	case len(found) == 1:
		return found[0], nil
	default:
		return keycard.Policy{}, nil
	}
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

// LatestPolicyVersion returns the policy's newest version with its Cedar
// text. Its ID is "" when the policy has no version. Every error wraps ErrAPI.
func (s *Session) LatestPolicyVersion(ctx context.Context, policyID string) (keycard.PolicyVersion, error) {
	page, err := s.zones.Policies.Versions.List(ctx, policyID, keycard.ZonePolicyVersionListParams{
		ZoneID: s.Zone.ID,
		Limit:  keycard.Int(1),
		Order:  keycard.ZonePolicyVersionListParamsOrderDesc,
		Format: keycard.ZonePolicyVersionListParamsFormatCedar,
	})
	if err != nil {
		return keycard.PolicyVersion{}, fmt.Errorf("%w: list versions of policy %s: %w",
			ErrAPI, policyID, requestError(err))
	}
	if page == nil || !page.JSON.Items.Valid() {
		return keycard.PolicyVersion{}, fmt.Errorf("%w: list versions of policy %s: %w", ErrAPI, policyID, errNoPage)
	}
	if len(page.Items) == 0 {
		return keycard.PolicyVersion{}, nil
	}
	version := page.Items[0]
	if version.ID == "" || version.PolicyID != policyID {
		return keycard.PolicyVersion{}, fmt.Errorf("%w: a version listed for policy %s has no id or another policy_id",
			ErrAPI, policyID)
	}
	return version, nil
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

// ActivePolicySets returns the zone's policy sets that have an active
// binding. Every error wraps ErrAPI.
func (s *Session) ActivePolicySets(ctx context.Context) ([]keycard.PolicySetWithBinding, error) {
	all, err := s.listPolicySets(ctx, keycard.ZonePolicySetListParams{FilterActive: keycard.Bool(true)})
	if err != nil {
		return nil, err
	}
	var active []keycard.PolicySetWithBinding
	for _, set := range all {
		if set.Active {
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
// set. A zone has one active policy set, so this replaces the binding of any
// other set.
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

func (s *Session) listPolicies(ctx context.Context, name string) ([]keycard.Policy, error) {
	return listAll("policies", func(params pageParams) (listPage[keycard.Policy], error) {
		page, err := s.zones.Policies.List(ctx, s.Zone.ID, keycard.ZonePolicyListParams{
			After: params.After, Limit: params.Limit, QueryName: []string{name},
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
