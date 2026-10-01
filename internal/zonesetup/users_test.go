//nolint:goconst,varnamelen // The cases repeat API paths and ids so each expectation reads on its own.
package zonesetup_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

// TestRunUsers pins the per-user decisions: user lookup, the token
// credential, both halves of the user's decision and the policy set.
func TestRunUsers(t *testing.T) {
	t.Parallel()
	runCases(t, []runCase{
		{
			name:       "an unknown user stops the run after the per-zone entities",
			values:     func(v *zonesetup.Values) { v.Users = []zonesetup.UserValues{{Email: "nobody@example.com"}} },
			wantWrites: perZoneWrites(),
			wantErr:    "user nobody@example.com: not found: user with email nobody@example.com. Create the user first",
		},
		{
			name: "two users with one email is ambiguous",
			seed: func(f *zoneAPI) {
				f.seedComplete()
				f.users = append(f.users, &record{"id": "usr-2", "email": "casey@example.com", "identifier": "usr-2"})
			},
			wantWrites: []string{"PATCH /zones/zone-1/secrets/sec-1"},
			wantErr:    "user casey@example.com: ambiguous: 2 users have email casey@example.com",
		},
		{
			name: "an existing token credential for the subject is reused",
			seed: func(f *zoneAPI) {
				f.seedComplete()
				f.credentials = append(f.credentials, &record{
					"id": "cred-9", "application_id": "app-1", "provider_id": "prov-1", "type": "token",
					"subject": "system:serviceaccount:agents:someone-else",
				})
			},
			wantWrites: []string{"PATCH /zones/zone-1/secrets/sec-1"},
			wantOut:    completeOutputs(),
		},
		{
			name:       "another active policy set is not replaced without the flag",
			seed:       func(f *zoneAPI) { f.seedOtherActiveSet() },
			wantWrites: freshWrites()[:12],
			wantErr: `policy set: another policy set is active: "Production" (set-other). ` +
				`Activating "Kubernetes Sandbox" would replace it. Re-run with --replace-active-policy-set to do that`,
		},
		{
			name:    "a dry run reports the same refusal",
			seed:    func(f *zoneAPI) { f.seedOtherActiveSet() },
			dryRun:  true,
			wantErr: `another policy set is active: "Production" (set-other)`,
		},
		{
			name:       "the flag lets the run take the active slot",
			seed:       func(f *zoneAPI) { f.seedOtherActiveSet() },
			replace:    true,
			wantWrites: freshWrites(),
			wantOut:    completeOutputs(),
			wantLog:    []string{`policy set "Production" (set-other) is active and will be replaced`},
			check:      checkSlotTaken,
		},
		{
			name: "a complete but inactive sandbox set is activated without a new version",
			seed: func(f *zoneAPI) {
				f.seedComplete()
				f.activeVersion = ""
			},
			wantWrites: []string{
				"PATCH /zones/zone-1/secrets/sec-1",
				"PATCH /zones/zone-1/policy-sets/set-1/versions/sv-1",
			},
			wantOut: completeOutputs(),
		},
		{
			name: "a second user adds their own pair of policies",
			seed: func(f *zoneAPI) {
				f.seedComplete()
				f.users = append(f.users, &record{"id": "usr-2", "email": "sam@example.com", "identifier": "usr-2"})
			},
			values: func(v *zonesetup.Values) {
				v.Users = append(v.Users, zonesetup.UserValues{Email: "sam@example.com"})
			},
			wantWrites: []string{
				"PATCH /zones/zone-1/secrets/sec-1",
				"POST /zones/zone-1/application-credentials",
				"POST /zones/zone-1/policies",
				"POST /zones/zone-1/policies/pol-3/versions",
				"POST /zones/zone-1/policies",
				"POST /zones/zone-1/policies/pol-4/versions",
				"POST /zones/zone-1/policy-sets/set-1/versions",
				"PATCH /zones/zone-1/policy-sets/set-1/versions/sv-2",
			},
			check: checkSecondUser,
		},
		{
			name:   "a sandbox that reaches nothing gets the actor policy alone",
			values: func(v *zonesetup.Values) { v.Resources = nil },
			wantWrites: []string{
				"POST /zones/zone-1/providers",
				"POST /zones/zone-1/applications",
				"POST /zones/zone-1/application-credentials",
				"POST /zones/zone-1/policies",
				"POST /zones/zone-1/policies/pol-1/versions",
				"POST /zones/zone-1/policy-sets",
				"POST /zones/zone-1/policy-sets/set-1/versions",
				"PATCH /zones/zone-1/policy-sets/set-1/versions/sv-1",
			},
			check: checkNoSubjectGrant,
		},
		{
			name:       "a zone with no default schema cannot take a policy version",
			seed:       func(f *zoneAPI) { f.schemaVersion = "" },
			wantWrites: freshWrites()[:9],
			wantErr:    "not found: default policy schema version in the zone",
		},
	})
}

// checkFreshZonePolicies pins the credential and both halves of the user's
// decision.
func checkFreshZonePolicies(t *testing.T, f *zoneAPI) {
	t.Helper()
	cred := f.bodyOf("POST /zones/zone-1/application-credentials")
	wantCred := map[string]any{
		"application_id": "app-1", "provider_id": "prov-1", "type": "token",
		"subject": "system:serviceaccount:agents:usr-1",
	}
	if !reflect.DeepEqual(cred, wantCred) {
		t.Errorf("credential body = %v, want %v", cred, wantCred)
	}
	pv := f.bodyOf("POST /zones/zone-1/policies/pol-1/versions")
	// Every optional context attribute is guarded with `has` before it is
	// read. The zone refuses to publish a version that reads one unguarded.
	wantPolicy := `@id("sandbox-impersonation-usr-1")
permit (
  principal is Keycard::Application,
  action,
  resource
) when {
  context has impersonate && context.impersonate == true &&
  context has subject &&
  context.subject.identifier == "usr-1" &&
  context has actor_claims &&
  context.actor_claims has issuer_claims &&
  context.actor_claims.issuer_claims.contains({
    "key": "sub", "value": "system:serviceaccount:agents:usr-1"
  })
};
`
	if got := pv["cedar_raw"]; got != wantPolicy {
		t.Errorf("policy text =\n%v\nwant\n%s", got, wantPolicy)
	}
	if pv["schema_version"] != testSchemaVersion {
		t.Errorf("policy schema_version = %v, want %s", pv["schema_version"], testSchemaVersion)
	}
	wantDescriptions := map[string]string{
		"pol-1": "Lets the sandbox of casey@example.com impersonate that user, and nobody else",
		"pol-2": "Lets casey@example.com reach the destinations the sandbox mints credentials for",
	}
	for id, want := range wantDescriptions {
		if got := (*f.find(f.policies, id))["description"]; got != want {
			t.Errorf("policy %s description = %v, want %q", id, got, want)
		}
	}
	grant := f.bodyOf("POST /zones/zone-1/policies/pol-2/versions")
	if got, want := grant["cedar_raw"], zonesetup.SubjectGrantPolicy("usr-1", testResourceIdentifiers()); got != want {
		t.Errorf("subject grant text =\n%v\nwant\n%s", got, want)
	}
	if grant["schema_version"] != testSchemaVersion {
		t.Errorf("subject grant schema_version = %v, want %s", grant["schema_version"], testSchemaVersion)
	}
}

// checkFreshZonePolicySet pins the policy set version that was published
// and activated.
func checkFreshZonePolicySet(t *testing.T, f *zoneAPI) {
	t.Helper()
	set := f.bodyOf("POST /zones/zone-1/policy-sets")
	if set["name"] != zonesetup.DefaultPolicySetName || set["target_type"] != "zone" {
		t.Errorf("policy set body = %v", set)
	}
	sv := f.bodyOf("POST /zones/zone-1/policy-sets/set-1/versions")
	wantManifest := map[string]any{"entries": []any{
		map[string]any{"policy_id": "pol-1", "policy_version_id": "pv-1"},
		map[string]any{"policy_id": "pol-2", "policy_version_id": "pv-2"},
	}}
	if !reflect.DeepEqual(sv["manifest"], wantManifest) || sv["schema_version"] != testSchemaVersion {
		t.Errorf("policy set version body = %v", sv)
	}
	if act := f.bodyOf("PATCH /zones/zone-1/policy-sets/set-1/versions/sv-1"); act["active"] != true {
		t.Errorf("activation body = %v", act)
	}
	if f.activeVersion != "sv-1" {
		t.Errorf("active policy set version = %s, want sv-1", f.activeVersion)
	}
}

func checkSlotTaken(t *testing.T, f *zoneAPI) {
	t.Helper()
	if f.activeVersion != "sv-1" {
		t.Errorf("active version = %s, want sv-1", f.activeVersion)
	}
}

// checkSecondUser pins the second user's own pair of policies and the four
// entries the set then holds.
func checkSecondUser(t *testing.T, f *zoneAPI) {
	t.Helper()
	cred := f.bodyOf("POST /zones/zone-1/application-credentials")
	if cred["subject"] != "system:serviceaccount:agents:usr-2" {
		t.Errorf("second credential subject = %v", cred["subject"])
	}
	actor := f.bodyOf("POST /zones/zone-1/policies/pol-3/versions")["cedar_raw"]
	if actor != zonesetup.ImpersonationPolicy(zonesetup.DefaultNamespace, "usr-2") {
		t.Errorf("second user impersonation policy = %v", actor)
	}
	grant := f.bodyOf("POST /zones/zone-1/policies/pol-4/versions")["cedar_raw"]
	if grant != zonesetup.SubjectGrantPolicy("usr-2", testResourceIdentifiers()) {
		t.Errorf("second user subject grant = %v", grant)
	}
	sv := f.bodyOf("POST /zones/zone-1/policy-sets/set-1/versions")
	wantManifest := map[string]any{"entries": []any{
		map[string]any{"policy_id": "pol-1", "policy_version_id": "pv-1"},
		map[string]any{"policy_id": "pol-2", "policy_version_id": "pv-2"},
		map[string]any{"policy_id": "pol-3", "policy_version_id": "pv-3"},
		map[string]any{"policy_id": "pol-4", "policy_version_id": "pv-4"},
	}}
	if !reflect.DeepEqual(sv["manifest"], wantManifest) {
		t.Errorf("manifest = %v, want %v", sv["manifest"], wantManifest)
	}
}

// checkNoSubjectGrant pins that the one policy written is the actor half.
// A grant naming no resource permits nothing, so none is written.
func checkNoSubjectGrant(t *testing.T, f *zoneAPI) {
	t.Helper()
	if name := f.bodyOf("POST /zones/zone-1/policies")["name"]; name != zonesetup.PolicyName("usr-1") {
		t.Errorf("policy name = %v, want %v", name, zonesetup.PolicyName("usr-1"))
	}
	sv := f.bodyOf("POST /zones/zone-1/policy-sets/set-1/versions")
	wantManifest := map[string]any{"entries": []any{map[string]any{"policy_id": "pol-1", "policy_version_id": "pv-1"}}}
	if !reflect.DeepEqual(sv["manifest"], wantManifest) {
		t.Errorf("manifest = %v, want %v", sv["manifest"], wantManifest)
	}
}

// TestSubjectGrantContract pins the subject half of the decision. The
// identifiers are given out of alphabetical order, so the text also pins
// that the grant lists them in the order the values file does.
func TestSubjectGrantContract(t *testing.T) {
	t.Parallel()
	want := `@id("sandbox-access-usr-1")
permit (
  principal is Keycard::User,
  action,
  resource
) when {
  principal.identifier == "usr-1" &&
  resource has identifier &&
  ["https://openrouter.ai/api/v1", "https://api.github.com/"].contains(resource.identifier)
};
`
	identifiers := []string{openRouterURL, githubURL}
	if got := zonesetup.SubjectGrantPolicy("usr-1", identifiers); got != want {
		t.Errorf("policy =\n%s\nwant\n%s", got, want)
	}
	if got := zonesetup.SubjectGrantName("usr-1"); got != "Sandbox access: usr-1" {
		t.Errorf("subject grant name = %s", got)
	}
	if got := zonesetup.PolicyName("usr-1"); got != "Sandbox impersonation: usr-1" {
		t.Errorf("policy name = %s", got)
	}
}

// TestPolicyIDs pins the `@id` annotation of every policy. The zone keys the
// merged policy set by `@id`, so the two policies of two enrolled users need
// four distinct ids.
func TestPolicyIDs(t *testing.T) {
	t.Parallel()
	identifiers := []string{githubURL}
	got := []string{
		policyIDOf(t, zonesetup.ImpersonationPolicy(zonesetup.DefaultNamespace, "usr-1")),
		policyIDOf(t, zonesetup.SubjectGrantPolicy("usr-1", identifiers)),
		policyIDOf(t, zonesetup.ImpersonationPolicy(zonesetup.DefaultNamespace, "usr-2")),
		policyIDOf(t, zonesetup.SubjectGrantPolicy("usr-2", identifiers)),
	}
	want := []string{
		"sandbox-impersonation-usr-1", "sandbox-access-usr-1",
		"sandbox-impersonation-usr-2", "sandbox-access-usr-2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("policy ids = %q, want %q", got, want)
	}
}

// policyIDOf is the value of the policy's `@id` annotation, which is the
// first line of the Cedar text.
func policyIDOf(t *testing.T, cedar string) string {
	t.Helper()
	first, _, _ := strings.Cut(cedar, "\n")
	id, ok := strings.CutPrefix(first, `@id("`)
	if !ok {
		t.Fatalf("policy has no @id annotation:\n%s", cedar)
	}
	id, ok = strings.CutSuffix(id, `")`)
	if !ok {
		t.Fatalf("@id annotation is malformed: %s", first)
	}
	return id
}
