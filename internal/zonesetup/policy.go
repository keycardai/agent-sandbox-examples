package zonesetup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/keycardai/keycard-go"
	"github.com/keycardai/keycard-go/option"
)

const (
	impersonationKind = "impersonation"
	accessKind        = "access"
)

// A UserPolicy is one Cedar policy zone-setup publishes for one user.
type UserPolicy struct {
	Name        string
	Description string
	// Cedar starts with the `@id` annotation.
	Cedar string
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
// `@id` annotation to body. The zone keys the policy by its `@id` in the Cedar
// JSON it stores.
func newUserPolicy(kind, namespace, userID, description, body string) UserPolicy {
	return UserPolicy{
		Name:        policyNamePrefix(kind, namespace) + userID,
		Description: description,
		Cedar:       fmt.Sprintf("@id(%q)\n%s", "sandbox-"+kind+"/"+namespace+"/"+userID, body),
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

// A Manifest is the content of one policy set version. Every entry has a
// version validated against SchemaVersion.
type Manifest struct {
	PolicySetID   string          `json:"policy_set_id"`
	VersionID     string          `json:"policy_set_version_id"`
	SchemaVersion string          `json:"schema_version"`
	Entries       []ManifestEntry `json:"entries"`
}

// A ManifestEntry pins one version of one policy.
type ManifestEntry struct {
	PolicyID        string `json:"policy_id"`
	PolicyVersionID string `json:"policy_version_id"`
}

// An ApplyEntry is one policy in the content ApplyPolicySet sends.
//
//   - PolicyID and PolicyVersionID pin that version. Its schema must be the
//     apply's schema.
//   - PolicyID and CedarRaw keep the pinned version when its content and
//     schema match, and publish a new version otherwise.
//   - NewPolicy and CedarRaw create the policy and its first version.
type ApplyEntry struct {
	PolicyID        string     `json:"policy_id,omitempty"`
	PolicyVersionID string     `json:"policy_version_id,omitempty"`
	NewPolicy       *NewPolicy `json:"new_policy,omitempty"`
	CedarRaw        string     `json:"cedar_raw,omitempty"`
}

// A NewPolicy names a policy that an ApplyEntry creates.
type NewPolicy struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// An ApplyChange is what an apply did with one policy. Action is one of
// created_policy, created_version, reused, repinned and dropped.
type ApplyChange struct {
	PolicyID string `json:"policy_id"`
	Name     string `json:"name"`
	Action   string `json:"action"`
}

// An ApplyResult is the policy set version an apply activated.
type ApplyResult struct {
	VersionID string
	// Created is false when the content matched the latest version.
	Created bool
	Changes []ApplyChange
}

// PolicySetManifest returns the manifest of the policy set's version
// versionID, or of its latest version when versionID is "". For the latest
// version it also returns the ETag that ApplyPolicySet sends as If-Match. A
// set with no version has an empty latest manifest. Every error wraps ErrAPI.
func (s *Session) PolicySetManifest(ctx context.Context, setID, versionID string) (Manifest, string, error) {
	path := s.policySetPath(setID) + "/manifest"
	if versionID != "" {
		path += "?" + url.Values{"version": {versionID}}.Encode()
	}
	var manifest Manifest
	var response *http.Response
	if err := s.client().Get(ctx, path, nil, &manifest, option.WithResponseInto(&response)); err != nil {
		return Manifest{}, "", fmt.Errorf("%w: get the manifest of policy set %s: %w", ErrAPI, setID, requestError(err))
	}
	if manifest.PolicySetID != setID || manifest.Entries == nil {
		return Manifest{}, "", fmt.Errorf("%w: get the manifest of policy set %s: "+
			"the response holds another set or no entries", ErrAPI, setID)
	}
	if versionID != "" {
		if manifest.VersionID != versionID {
			return Manifest{}, "", fmt.Errorf("%w: get the manifest of policy set %s version %s: "+
				"the response holds another version", ErrAPI, setID, versionID)
		}
		return manifest, "", nil
	}
	etag := response.Header.Get("ETag")
	if etag == "" {
		return Manifest{}, "", fmt.Errorf("%w: get the manifest of policy set %s: the response holds no ETag",
			ErrAPI, setID)
	}
	return manifest, etag, nil
}

// ApplyPolicySet replaces the content of the policy set with entries and
// activates the resulting version in one transaction. The zone validates
// every entry against schema. When schema is "", the zone uses the latest
// version's schema if it still accepts writes, else its default schema. etag
// is the latest manifest's ETag from PolicySetManifest. The zone returns HTTP
// 412 when etag does not match the latest manifest. Every error wraps ErrAPI.
func (s *Session) ApplyPolicySet(
	ctx context.Context,
	setID, etag, schema string,
	entries []ApplyEntry,
) (ApplyResult, error) {
	if entries == nil {
		entries = []ApplyEntry{}
	}
	body, err := json.Marshal(struct {
		SchemaVersion string       `json:"schema_version,omitempty"`
		Activate      bool         `json:"activate"`
		Entries       []ApplyEntry `json:"entries"`
	}{schema, true, entries})
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: apply policy set %s: %w", ErrAPI, setID, err)
	}
	var applied struct {
		Created bool          `json:"created"`
		Changes []ApplyChange `json:"changes"`
		Version struct {
			ID string `json:"id"`
		} `json:"policy_set_version"`
		Binding struct {
			VersionID string `json:"policy_set_version_id"`
		} `json:"binding"`
	}
	err = s.client().Put(ctx, s.policySetPath(setID)+"/manifest", body, &applied, option.WithHeader("If-Match", etag))
	if err != nil {
		return ApplyResult{}, fmt.Errorf("%w: apply policy set %s: %w", ErrAPI, setID, requestError(err))
	}
	if applied.Version.ID == "" || applied.Binding.VersionID != applied.Version.ID {
		return ApplyResult{}, fmt.Errorf("%w: apply policy set %s: the response holds no version or does not bind it",
			ErrAPI, setID)
	}
	return ApplyResult{VersionID: applied.Version.ID, Created: applied.Created, Changes: applied.Changes}, nil
}

func (s *Session) policySetPath(setID string) string {
	return "zones/" + url.PathEscape(s.Zone.ID) + "/policy-sets/" + url.PathEscape(setID)
}

// client returns a keycard.Client for the routes keycard-go has no method
// for. It uses the session's base URL, token and HTTP client.
func (s *Session) client() *keycard.Client {
	return &keycard.Client{Options: s.zones.Options}
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
