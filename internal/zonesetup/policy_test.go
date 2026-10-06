//nolint:wrapcheck // Each case's closure returns the Session's error for errors.Is.
package zonesetup_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/keycardai/keycard-go"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

const (
	policiesPath   = "/zones/zone-a/policies"
	policySetsPath = "/zones/zone-a/policy-sets"
	schemasPath    = "/zones/zone-a/policy-schemas"
	defaultSchema  = `{"items":[{"version":"2026-02-24","is_default":true}],"pagination":{"after_cursor":""}}`
	noSchema       = `{"items":[],"pagination":{"after_cursor":""}}`
)

// TestUserPolicies compares the whole Cedar text, because the text decides
// which ServiceAccount token lets the Application act for which user. The
// user's identifier differs from its ID, as it does when the provider sets
// user_identifier_claim, so a line that matches the wrong one fails. The
// digest is the SHA-256 of the text after the digest line.
func TestUserPolicies(t *testing.T) {
	t.Parallel()
	const issuer = "https://oidc.example.com"
	user := keycard.User{ID: adaID, Identifier: adaIdentifier}
	want := []struct{ name, description, cedar string }{
		{
			name:        "Sandbox impersonation: agents/user-1",
			description: "Lets Application urn:keycard:sandbox-proxy:agents impersonate user user-1 for its dependencies",
			cedar: `@id("sandbox-impersonation/agents/user-1")
permit (
  principal is Keycard::Application,
  action,
  resource
) when {
  principal.identifier == "urn:keycard:sandbox-proxy:agents" &&
  principal.dependencies.contains(resource) &&
  context has impersonate && context.impersonate == true &&
  context has subject &&
  context.subject.identifier == "ada-identifier" &&
  context has actor_claims &&
  context.actor_claims has issuer_claims &&
  context.actor_claims.issuer_claims.contains({"key": "iss", "value": "https://oidc.example.com"}) &&
  context.actor_claims.issuer_claims.contains({"key": "sub", "value": "system:serviceaccount:agents:user-1"})
};
`,
		},
		{
			name:        "Sandbox access: agents/user-1",
			description: "Lets user user-1 reach resources through the sandbox in namespace agents",
			cedar: `@id("sandbox-access/agents/user-1")
permit (
  principal is Keycard::User,
  action,
  resource
) when {
  principal.identifier == "ada-identifier" &&
  context has actor_claims &&
  context.actor_claims has issuer_claims &&
  context.actor_claims.issuer_claims.contains({"key": "iss", "value": "https://oidc.example.com"}) &&
  context.actor_claims.issuer_claims.contains({"key": "sub", "value": "system:serviceaccount:agents:user-1"})
};
`,
		},
	}
	policies := zonesetup.UserPolicies("agents", issuer, user)
	if len(policies) != len(want) {
		t.Fatalf("got %d policies, want %d", len(policies), len(want))
	}
	for i, policy := range policies {
		sum := sha256.Sum256([]byte(want[i].cedar))
		digest := hex.EncodeToString(sum[:])
		wantCedar := `@sandbox_digest("` + digest + "\")\n" + want[i].cedar
		if policy.Name != want[i].name {
			t.Errorf("policy %d name = %q, want %q", i, policy.Name, want[i].name)
		}
		if policy.Description != want[i].description {
			t.Errorf("policy %d description = %q, want %q", i, policy.Description, want[i].description)
		}
		if policy.Digest != digest {
			t.Errorf("policy %d digest = %s, want %s", i, policy.Digest, digest)
		}
		if policy.Cedar != wantCedar {
			t.Errorf("policy %d Cedar =\n%s\nwant\n%s", i, policy.Cedar, wantCedar)
		}
	}
}

// TestFindPolicy runs each listing through SandboxPolicies and FindPolicySet.
// SandboxPolicies matches the namespace's name prefixes and FindPolicySet the
// exact name. Both ignore archived matches and refuse two matches.
func TestFindPolicy(t *testing.T) {
	t.Parallel()
	const name = "Sandbox impersonation: agents/user-1"
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
			items: `{"id":"p-2","name":"Sandbox impersonation: agents-2/user-1"},` +
				`{"id":"p-1","name":"Sandbox impersonation: agents/user-1"}`,
			wantID: "p-1",
		},
		{
			name:  "an archived match is ignored",
			items: `{"id":"p-old","name":"Sandbox impersonation: agents/user-1","archived_at":"2026-01-01T00:00:00Z"}`,
		},
		{
			name: "two unarchived matches are refused",
			items: `{"id":"p-1","name":"Sandbox impersonation: agents/user-1"},` +
				`{"id":"p-2","name":"Sandbox impersonation: agents/user-1"}`,
			wantErr: zonesetup.ErrPolicyAmbiguous, wantSetErr: zonesetup.ErrPolicySetAmbiguous,
		},
		{
			name:    "a match without an id is refused",
			items:   `{"name":"Sandbox impersonation: agents/user-1"}`,
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
	cedar := zonesetup.ImpersonationPolicy("agents", "https://oidc.example.com",
		keycard.User{ID: adaID, Identifier: adaID}).Cedar
	cedarJSON, err := json.Marshal(cedar)
	if err != nil {
		t.Fatal(err)
	}
	entries := []keycard.PolicySetManifestEntryParam{{PolicyID: "policy-k", PolicyVersionID: "version-k"}}
	cases := []struct {
		name     string
		path     string
		response string
		schemas  string
		write    func(context.Context, *zonesetup.Session) error
		wantBody string
		wantErr  error
	}{
		{
			name: "a policy version is validated against the default schema",
			path: policiesPath + "/policy-k/versions", response: `{"id":"version-new"}`, schemas: defaultSchema,
			write: func(ctx context.Context, s *zonesetup.Session) error {
				_, err := s.CreatePolicyVersion(ctx, "policy-k", cedar)
				return err
			},
			wantBody: `{"schema_version":"2026-02-24","cedar_raw":` + string(cedarJSON) + `}`,
		},
		{
			name: "a zone without a default schema is refused before a write",
			path: policiesPath + "/policy-k/versions", response: `{"id":"version-new"}`, schemas: noSchema,
			write: func(ctx context.Context, s *zonesetup.Session) error {
				_, err := s.CreatePolicyVersion(ctx, "policy-k", cedar)
				return err
			},
			wantErr: zonesetup.ErrPolicySchemaNotFound,
		},
		{
			name: "a policy set targets the zone", path: policySetsPath, response: `{"id":"set-new"}`,
			write: func(ctx context.Context, s *zonesetup.Session) error {
				_, err := s.CreatePolicySet(ctx, "Kubernetes Sandbox")
				return err
			},
			wantBody: `{"name":"Kubernetes Sandbox","target_type":"zone"}`,
		},
		{
			name: "a policy set version holds the manifest", schemas: defaultSchema,
			path: policySetsPath + "/set-k/versions", response: `{"id":"set-version-new"}`,
			write: func(ctx context.Context, s *zonesetup.Session) error {
				_, err := s.CreatePolicySetVersion(ctx, "set-k", entries)
				return err
			},
			wantBody: `{"schema_version":"2026-02-24",` +
				`"manifest":{"entries":[{"policy_id":"policy-k","policy_version_id":"version-k"}]}}`,
		},
		{
			name: "activation binds the version", path: policySetsPath + "/set-k/versions/set-version-k",
			response: `{"id":"set-version-k","active":true}`,
			write: func(ctx context.Context, s *zonesetup.Session) error {
				return s.ActivatePolicySetVersion(ctx, "set-k", "set-version-k")
			},
			wantBody: `{"active":true}`,
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
		{
			name: "an activation response for another version is refused",
			path: policySetsPath + "/set-k/versions/set-version-k", response: `{"id":"set-version-j"}`,
			write: func(ctx context.Context, s *zonesetup.Session) error {
				return s.ActivatePolicySetVersion(ctx, "set-k", "set-version-k")
			},
			wantBody: `{"active":true}`, wantErr: zonesetup.ErrAPI,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{zones: []fakeItem{{zoneA, issuerA}}}
			fake.handlers = map[string]http.HandlerFunc{
				schemasPath: rawBody(tc.schemas),
				tc.path: func(w http.ResponseWriter, r *http.Request) {
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

// TestPolicyReads checks that each read refuses a malformed response, and
// that a version's digest comes from the Cedar JSON the zone stores.
func TestPolicyReads(t *testing.T) {
	t.Parallel()
	versions := func(items string) string { return `{"items":[` + items + `],"pagination":{"after_cursor":""}}` }
	latestDigest := func(ctx context.Context, s *zonesetup.Session) (string, error) {
		versionID, digest, err := s.LatestPolicyDigest(ctx, "policy-k")
		return versionID + " " + digest, err
	}
	cases := []struct {
		name     string
		path     string
		response string
		read     func(context.Context, *zonesetup.Session) (string, error)
		want     string
		wantErr  error
	}{
		{
			name: "the newest version's digest is its policy's annotation", path: policiesPath + "/policy-k/versions",
			response: versions(`{"id":"version-k","policy_id":"policy-k","cedar_json":{"staticPolicies":{` +
				`"sandbox-impersonation/agents/user-1":{"annotations":{"id":"sandbox-impersonation/agents/user-1",` +
				`"sandbox_digest":"abc"}}},"templates":{},"templateLinks":[]}}`),
			read: latestDigest,
			want: "version-k abc",
		},
		{
			name: "a version with two policies has no digest", path: policiesPath + "/policy-k/versions",
			response: versions(`{"id":"version-k","policy_id":"policy-k","cedar_json":{"staticPolicies":{` +
				`"a":{"annotations":{"sandbox_digest":"abc"}},"b":{"annotations":{}}}}}`),
			read: latestDigest,
			want: "version-k ",
		},
		{
			name: "a version of another policy is refused", path: policiesPath + "/policy-k/versions",
			response: versions(`{"id":"version-j","policy_id":"policy-j"}`),
			read:     latestDigest,
			want:     " ",
			wantErr:  zonesetup.ErrAPI,
		},
		{
			name: "a manifest of another policy set is refused", path: policySetsPath + "/set-k/versions/set-version-k",
			response: `{"id":"set-version-k","policy_set_id":"set-j","manifest":{"entries":[]}}`,
			read: func(ctx context.Context, s *zonesetup.Session) (string, error) {
				_, err := s.PolicySetManifest(ctx, "set-k", "set-version-k")
				return "", err
			},
			wantErr: zonesetup.ErrAPI,
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
					if strings.HasSuffix(r.URL.Path, "/versions") && r.URL.Query().Get("format") != "json" {
						t.Errorf("listed versions with format %q, want json", r.URL.Query().Get("format"))
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
