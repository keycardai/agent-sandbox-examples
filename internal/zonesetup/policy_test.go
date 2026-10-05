//nolint:wrapcheck // Each case's closure returns the Session's error for errors.Is.
package zonesetup_test

import (
	"context"
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

// TestImpersonationPolicy pins the Cedar text, which is the whole check that
// one ServiceAccount acts for one user.
func TestImpersonationPolicy(t *testing.T) {
	t.Parallel()
	want := `permit (principal, action, resource)
when {
  context.impersonate && context.subject.identifier == "user-1" &&
  context.actor_claims.issuer_claims.contains({
    "key": "sub", "value": "system:serviceaccount:agents:user-1"
  })
};
`
	if got := zonesetup.ImpersonationPolicy("agents", "user-1"); got != want {
		t.Errorf("policy =\n%s\nwant\n%s", got, want)
	}
	if got := zonesetup.PolicyName("user-1"); got != "Sandbox impersonation: user-1" {
		t.Errorf("policy name = %s", got)
	}
}

// TestFindPolicy runs each listing through FindPolicy and FindPolicySet,
// which match an unarchived name exactly and refuse two matches.
func TestFindPolicy(t *testing.T) {
	t.Parallel()
	const name = "Sandbox impersonation: user-1"
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
			name: "the exact name among the query matches",
			items: `{"id":"p-10","name":"Sandbox impersonation: user-10"},` +
				`{"id":"p-1","name":"Sandbox impersonation: user-1"}`,
			wantID: "p-1",
		},
		{
			name:  "an archived match is ignored",
			items: `{"id":"p-old","name":"Sandbox impersonation: user-1","archived_at":"2026-01-01T00:00:00Z"}`,
		},
		{
			name: "two unarchived matches are refused",
			items: `{"id":"p-1","name":"Sandbox impersonation: user-1"},` +
				`{"id":"p-2","name":"Sandbox impersonation: user-1"}`,
			wantErr: zonesetup.ErrPolicyAmbiguous, wantSetErr: zonesetup.ErrPolicySetAmbiguous,
		},
		{
			name:    "a match without an id is refused",
			items:   `{"name":"Sandbox impersonation: user-1"}`,
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

// TestPolicyWrites pins the body of each policy write, and the response
// checks that refuse a write the API did not confirm.
func TestPolicyWrites(t *testing.T) {
	t.Parallel()
	cedar := zonesetup.ImpersonationPolicy("agents", "user-1")
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
					fake.mu.Lock()
					fake.posts = append(fake.posts, readBody(w, r))
					fake.mu.Unlock()
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

// TestPolicyReads pins the response checks on the reads the policy set
// decision rests on.
func TestPolicyReads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		path     string
		response string
		read     func(context.Context, *zonesetup.Session) (string, error)
		want     string
		wantErr  error
	}{
		{
			name: "the newest version is read with its Cedar text", path: policiesPath + "/policy-k/versions",
			response: `{"items":[{"id":"version-k","policy_id":"policy-k","cedar_raw":"permit"}],` +
				`"pagination":{"after_cursor":""}}`,
			read: func(ctx context.Context, s *zonesetup.Session) (string, error) {
				version, err := s.LatestPolicyVersion(ctx, "policy-k")
				return version.ID + " " + version.CedarRaw, err
			},
			want: "version-k permit",
		},
		{
			name: "a version of another policy is refused", path: policiesPath + "/policy-k/versions",
			response: `{"items":[{"id":"version-j","policy_id":"policy-j"}],"pagination":{"after_cursor":""}}`,
			read: func(ctx context.Context, s *zonesetup.Session) (string, error) {
				version, err := s.LatestPolicyVersion(ctx, "policy-k")
				return version.ID, err
			},
			wantErr: zonesetup.ErrAPI,
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
			name: "only sets with an active binding are active", path: policySetsPath,
			response: `{"items":[{"id":"set-k","active":false},{"id":"set-other","active":true}],` +
				`"pagination":{"after_cursor":""}}`,
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
				tc.path: rawBody(tc.response),
			}}
			session := connectZoneA(t, fake)
			got, err := tc.read(t.Context(), session)
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Errorf("read = %q, %v, want %q, %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
