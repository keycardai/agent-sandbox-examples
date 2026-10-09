//nolint:wrapcheck // Each case's closure returns the Session's error for errors.Is.
package zonesetup_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/keycardai/keycard-go"

	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	policiesPath   = "/zones/zone-a/policies"
	policySetsPath = "/zones/zone-a/policy-sets"
	manifestPath   = policySetsPath + "/set-k/manifest"
	etagK          = `"etag-k"`
)

// TestUserPolicies compares the whole Cedar text, because the text decides
// which ServiceAccount token lets the Application act for which user. The
// user's identifier differs from its ID, as it does when the provider sets
// user_identifier_claim, so a line that matches the wrong one fails. The
// expected names, descriptions and text are the ones svc-catalog's
// kubernetes-sandbox-user package renders. Only a namespace other than
// DefaultNamespace goes into the names and `@id`s.
func TestUserPolicies(t *testing.T) {
	t.Parallel()
	const issuer = "https://oidc.example.com"
	user := keycard.User{ID: adaID, Identifier: adaIdentifier, Email: "ada@example.com"}
	type policy struct{ name, description, cedar string }
	cases := []struct {
		namespace string
		want      []policy
	}{
		{
			namespace: "agents",
			want: []policy{
				{
					name: "Sandbox impersonation: ada@example.com",
					description: "Permits Sandbox Egress Proxy to act on behalf of ada@example.com on resources " +
						"listed in its dependencies, for requests from their sandbox",
					cedar: `@id("sandbox-impersonation-user-1")
permit (
  principal is Keycard::Application,
  action,
  resource
) when {
  principal.identifier == "urn:keycard:sandbox-proxy:agents" &&
  context has impersonate && context.impersonate == true &&
  context has subject && context.subject.identifier == "ada-identifier" &&
  context has actor_claims && context.actor_claims has issuer_claims &&
  context.actor_claims.issuer_claims.contains({"key": "iss", "value": "https://oidc.example.com"}) &&
  context.actor_claims.issuer_claims.contains({"key": "sub", "value": "system:serviceaccount:agents:user-1"}) &&
  principal.dependencies.contains(resource)
};`,
				},
				{
					name:        "Sandbox access: ada@example.com",
					description: "Permits ada@example.com's sandbox to act on their behalf through Sandbox Egress Proxy",
					cedar: `@id("sandbox-access-user-1")
permit (
  principal is Keycard::User,
  action,
  resource
) when {
  principal.identifier == "ada-identifier" &&
  context has actor_claims && context.actor_claims has issuer_claims &&
  context.actor_claims.issuer_claims.contains({"key": "iss", "value": "https://oidc.example.com"}) &&
  context.actor_claims.issuer_claims.contains({"key": "sub", "value": "system:serviceaccount:agents:user-1"})
};`,
				},
			},
		},
		{
			namespace: "team-b",
			want: []policy{
				{
					name: "Sandbox (team-b) impersonation: ada@example.com",
					description: "Permits Sandbox Egress Proxy to act on behalf of ada@example.com on resources " +
						"listed in its dependencies, for requests from their sandbox",
					cedar: `@id("sandbox-team-b-impersonation-user-1")
permit (
  principal is Keycard::Application,
  action,
  resource
) when {
  principal.identifier == "urn:keycard:sandbox-proxy:team-b" &&
  context has impersonate && context.impersonate == true &&
  context has subject && context.subject.identifier == "ada-identifier" &&
  context has actor_claims && context.actor_claims has issuer_claims &&
  context.actor_claims.issuer_claims.contains({"key": "iss", "value": "https://oidc.example.com"}) &&
  context.actor_claims.issuer_claims.contains({"key": "sub", "value": "system:serviceaccount:team-b:user-1"}) &&
  principal.dependencies.contains(resource)
};`,
				},
				{
					name:        "Sandbox (team-b) access: ada@example.com",
					description: "Permits ada@example.com's sandbox to act on their behalf through Sandbox Egress Proxy",
					cedar: `@id("sandbox-team-b-access-user-1")
permit (
  principal is Keycard::User,
  action,
  resource
) when {
  principal.identifier == "ada-identifier" &&
  context has actor_claims && context.actor_claims has issuer_claims &&
  context.actor_claims.issuer_claims.contains({"key": "iss", "value": "https://oidc.example.com"}) &&
  context.actor_claims.issuer_claims.contains({"key": "sub", "value": "system:serviceaccount:team-b:user-1"})
};`,
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.namespace, func(t *testing.T) {
			t.Parallel()
			policies := zonesetup.UserPolicies(tc.namespace, issuer, zonesetup.ApplicationName, user)
			if len(policies) != len(tc.want) {
				t.Fatalf("got %d policies, want %d", len(policies), len(tc.want))
			}
			for i, got := range policies {
				want := tc.want[i]
				if got.Name != want.name {
					t.Errorf("policy %d name = %q, want %q", i, got.Name, want.name)
				}
				if got.Description != want.description {
					t.Errorf("policy %d description = %q, want %q", i, got.Description, want.description)
				}
				if got.Cedar != want.cedar {
					t.Errorf("policy %d Cedar =\n%s\nwant\n%s", i, got.Cedar, want.cedar)
				}
			}
		})
	}
}

// TestFindPolicy runs each listing through SandboxPolicies, FindPolicy and
// FindPolicySet. SandboxPolicies matches the namespace's name prefixes, and
// FindPolicy and FindPolicySet the exact name. All three ignore archived
// matches and refuse two matches.
func TestFindPolicy(t *testing.T) {
	t.Parallel()
	const name = "Sandbox impersonation: ada@example.com"
	page := func(items string) http.HandlerFunc {
		return rawBody(`{"items":[` + items + `],"pagination":{"after_cursor":""}}`)
	}
	cases := []struct {
		name       string
		items      string
		wantID     string
		wantErr    error
		wantSetErr error
	}{
		{
			name: "only the namespace's name matches",
			items: `{"id":"p-2","name":"Sandbox (agents-2) impersonation: ada@example.com"},` +
				`{"id":"p-1","name":"Sandbox impersonation: ada@example.com"}`,
			wantID: "p-1",
		},
		{
			name:  "an archived match is ignored",
			items: `{"id":"p-old","name":"Sandbox impersonation: ada@example.com","archived_at":"2026-01-01T00:00:00Z"}`,
		},
		{
			name: "two unarchived matches are refused",
			items: `{"id":"p-1","name":"Sandbox impersonation: ada@example.com"},` +
				`{"id":"p-2","name":"Sandbox impersonation: ada@example.com"}`,
			wantErr: zonesetup.ErrPolicyAmbiguous, wantSetErr: zonesetup.ErrPolicySetAmbiguous,
		},
		{
			name:    "a match without an id is refused",
			items:   `{"name":"Sandbox impersonation: ada@example.com"}`,
			wantErr: zonesetup.ErrAPI, wantSetErr: zonesetup.ErrAPI,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{zones: []fakeItem{{zoneA, issuerA}}, handlers: map[string]http.HandlerFunc{
				policiesPath: page(tc.items), policySetsPath: page(tc.items),
			}}
			session := connectZoneA(t, fake)
			policies, err := session.SandboxPolicies(t.Context(), "agents")
			if !errors.Is(err, tc.wantErr) || policies[name].ID != tc.wantID || len(policies) > 1 {
				t.Errorf("SandboxPolicies = %v, %v, want %q, %v", policies, err, tc.wantID, tc.wantErr)
			}
			policy, err := session.FindPolicy(t.Context(), name)
			if !errors.Is(err, tc.wantErr) || policy.ID != tc.wantID {
				t.Errorf("FindPolicy = %q, %v, want %q, %v", policy.ID, err, tc.wantID, tc.wantErr)
			}
			set, err := session.FindPolicySet(t.Context(), name)
			if !errors.Is(err, tc.wantSetErr) || set.ID != tc.wantID {
				t.Errorf("FindPolicySet = %q, %v, want %q, %v", set.ID, err, tc.wantID, tc.wantSetErr)
			}
		})
	}
}

// TestPolicyWrites checks the body of each policy write, and that a write
// fails when the response does not confirm it.
func TestPolicyWrites(t *testing.T) {
	t.Parallel()
	cedar := zonesetup.ImpersonationPolicy("agents", "https://oidc.example.com", zonesetup.ApplicationName,
		keycard.User{ID: adaID, Identifier: adaID, Email: "ada@example.com"}).Cedar
	cedarJSON, err := json.Marshal(cedar)
	if err != nil {
		t.Fatal(err)
	}
	entries := []zonesetup.ApplyEntry{
		{PolicyID: "policy-other", PolicyVersionID: "version-other"},
		{PolicyID: "policy-k", CedarRaw: cedar},
		{NewPolicy: &zonesetup.NewPolicy{Name: "Sandbox access: ada@example.com", Description: "d"}, CedarRaw: cedar},
	}
	apply := func(schema string, entries []zonesetup.ApplyEntry) func(context.Context, *zonesetup.Session) error {
		return func(ctx context.Context, s *zonesetup.Session) error {
			_, err := s.ApplyPolicySet(ctx, "set-k", etagK, schema, entries)
			return err
		}
	}
	cases := []struct {
		name        string
		path        string
		response    string
		write       func(context.Context, *zonesetup.Session) error
		wantBody    string
		wantIfMatch string
		wantErr     error
	}{
		{
			name: "a policy set targets the zone", path: policySetsPath, response: `{"id":"set-new"}`,
			write: func(ctx context.Context, s *zonesetup.Session) error {
				_, err := s.CreatePolicySet(ctx, "Kubernetes Sandbox")
				return err
			},
			wantBody: `{"name":"Kubernetes Sandbox","target_type":"zone"}`,
		},
		{
			name: "an apply activates the entries under the set's schema, guarded by the ETag", path: manifestPath,
			response: `{"created":true,"policy_set_version":{"id":"set-version-l"},` +
				`"binding":{"policy_set_version_id":"set-version-l"}}`,
			write: apply("2026-03-16", entries),
			wantBody: `{"schema_version":"2026-03-16","activate":true,"entries":[` +
				`{"policy_id":"policy-other","policy_version_id":"version-other"},` +
				`{"policy_id":"policy-k","cedar_raw":` + string(cedarJSON) + `},` +
				`{"new_policy":{"name":"Sandbox access: ada@example.com","description":"d"},` +
				`"cedar_raw":` + string(cedarJSON) + `}]}`,
			wantIfMatch: etagK,
		},
		{
			name: "an apply without a schema or entries lets the zone choose the schema", path: manifestPath,
			response: `{"created":true,"policy_set_version":{"id":"set-version-l"},` +
				`"binding":{"policy_set_version_id":"set-version-l"}}`,
			write:       apply("", nil),
			wantBody:    `{"activate":true,"entries":[]}`,
			wantIfMatch: etagK,
		},
		{
			name: "an apply response that binds another version is refused", path: manifestPath,
			response: `{"created":true,"policy_set_version":{"id":"set-version-l"},` +
				`"binding":{"policy_set_version_id":"set-version-k"}}`,
			write:       apply("", nil),
			wantBody:    `{"activate":true,"entries":[]}`,
			wantIfMatch: etagK,
			wantErr:     zonesetup.ErrAPI,
		},
		{
			name: "an archive sends no body", path: policiesPath + "/policy-k",
			response: `{"id":"policy-k","archived_at":"2026-10-06T00:00:00Z"}`,
			write: func(ctx context.Context, s *zonesetup.Session) error {
				return s.ArchivePolicy(ctx, "policy-k")
			},
		},
		{
			name: "an archive response without archived_at is refused", path: policiesPath + "/policy-k",
			response: `{"id":"policy-k"}`,
			write: func(ctx context.Context, s *zonesetup.Session) error {
				return s.ArchivePolicy(ctx, "policy-k")
			},
			wantErr: zonesetup.ErrAPI,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{zones: []fakeItem{{zoneA, issuerA}}}
			fake.handlers = map[string]http.HandlerFunc{
				tc.path: func(w http.ResponseWriter, r *http.Request) {
					if got := r.Header.Get("If-Match"); got != tc.wantIfMatch {
						t.Errorf("If-Match = %q, want %q", got, tc.wantIfMatch)
					}
					if body := readBody(w, r); len(body) > 0 {
						fake.mu.Lock()
						fake.posts = append(fake.posts, body)
						fake.mu.Unlock()
					}
					rawBody(tc.response)(w, r)
				},
			}
			session := connectZoneA(t, fake)
			if err := tc.write(t.Context(), session); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			assertBodies(t, tc.path, fake.posts, tc.wantBody)
		})
	}
}

// TestPolicyReads checks that each read refuses a malformed response. Only
// the latest manifest has an ETag. ApplyPolicySet sends it as If-Match.
func TestPolicyReads(t *testing.T) {
	t.Parallel()
	manifest := func(versionID string) func(context.Context, *zonesetup.Session) (string, error) {
		return func(ctx context.Context, s *zonesetup.Session) (string, error) {
			got, etag, err := s.PolicySetManifest(ctx, "set-k", versionID)
			return fmt.Sprintf("%s %s %v %s", got.VersionID, got.SchemaVersion, got.Entries, etag), err
		}
	}
	// noManifest is what the read closure prints for a refused manifest.
	const noManifest = "  [] "
	// livePolicies asks about liveIDs and prints the live ones in order.
	liveIDs := []string{"pol-a", "pol-b", "pol-c"}
	livePolicies := func(ctx context.Context, s *zonesetup.Session) (string, error) {
		live, err := s.LivePolicyIDs(ctx, liveIDs)
		return strings.Join(slices.Sorted(maps.Keys(live)), " "), err
	}
	const versionK = `{"policy_set_id":"set-k","policy_set_version_id":"set-version-k","schema_version":"2026-03-16",` +
		`"entries":[{"policy_id":"policy-k","policy_version_id":"version-k"}]}`
	cases := []struct {
		name     string
		path     string
		response string
		etag     string
		read     func(context.Context, *zonesetup.Session) (string, error)
		want     string
		wantErr  error
	}{
		{
			name: "the latest manifest has its schema and ETag", path: manifestPath, response: versionK, etag: etagK,
			read: manifest(""),
			want: "set-version-k 2026-03-16 [{policy-k version-k}] " + etagK,
		},
		{
			name: "a set without a version has an empty latest manifest", path: manifestPath, etag: `"etag-empty"`,
			response: `{"policy_set_id":"set-k","policy_set_version_id":null,"schema_version":null,"entries":[]}`,
			read:     manifest(""),
			want:     `  [] "etag-empty"`,
		},
		{
			name: "a latest manifest without an ETag is refused", path: manifestPath, response: versionK,
			read:    manifest(""),
			want:    noManifest,
			wantErr: zonesetup.ErrAPI,
		},
		{
			name: "a version's manifest has no ETag", path: manifestPath, response: versionK, etag: etagK,
			read: manifest("set-version-k"),
			want: "set-version-k 2026-03-16 [{policy-k version-k}] ",
		},
		{
			name: "a manifest of another version is refused", path: manifestPath, response: versionK,
			read:    manifest("set-version-j"),
			want:    noManifest,
			wantErr: zonesetup.ErrAPI,
		},
		{
			name: "a manifest of another policy set is refused", path: manifestPath, etag: etagK,
			response: `{"policy_set_id":"set-j","entries":[]}`,
			read:     manifest(""),
			want:     "  [] ",
			wantErr:  zonesetup.ErrAPI,
		},
		{
			name: "only listed, unarchived ids are live", path: policiesPath,
			response: `{"items":[{"id":"pol-a"},{"id":"pol-b","archived_at":"2026-01-01T00:00:00Z"},{"id":"pol-x"}],` +
				`"pagination":{"after_cursor":""}}`,
			read: livePolicies,
			want: "pol-a",
		},
		{
			name: "a policy listing that reports more results is refused", path: policiesPath,
			response: `{"items":[{"id":"pol-a"}],"pagination":{"after_cursor":"pol-a"}}`,
			read:     livePolicies,
			wantErr:  zonesetup.ErrAPI,
		},
		{
			name: "only zone-wide sets with an active binding are active", path: policySetsPath,
			response: `{"items":[{"id":"set-k","active":false,"target_type":"zone"},` +
				`{"id":"set-other","active":true,"target_type":"zone"},` +
				`{"id":"set-user","active":true,"target_type":"user"}],"pagination":{"after_cursor":""}}`,
			read: func(ctx context.Context, s *zonesetup.Session) (string, error) {
				active, err := s.ActivePolicySets(ctx)
				ids := make([]string, 0, len(active))
				for _, set := range active {
					ids = append(ids, set.ID)
				}
				return strings.Join(ids, " "), err
			},
			want: "set-other",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{zones: []fakeItem{{zoneA, issuerA}}, handlers: map[string]http.HandlerFunc{
				tc.path: func(w http.ResponseWriter, r *http.Request) {
					if query := r.URL.Query(); r.URL.Path == policiesPath &&
						(!slices.Equal(query["filter[id]"], liveIDs) || query.Get("limit") != "3") {
						t.Errorf("listed policies with filter[id] %q and limit %q, want %q and 3",
							query["filter[id]"], query.Get("limit"), liveIDs)
					}
					if tc.etag != "" {
						w.Header().Set("ETag", tc.etag)
					}
					rawBody(tc.response)(w, r)
				},
			}}
			session := connectZoneA(t, fake)
			got, err := tc.read(t.Context(), session)
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Errorf("read = %q, %v, want %q, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
