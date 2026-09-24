//nolint:goconst,varnamelen // The cases repeat API paths and ids so each expectation reads on its own.
package zonesetup_test

import (
	"reflect"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

// TestRunUsers pins the per-user decisions: user lookup, the token
// credential, the impersonation policy and the policy set.
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
			wantWrites: freshWrites()[:10],
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
			name: "a second user adds a policy next to the first",
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
				"POST /zones/zone-1/policies/pol-2/versions",
				"POST /zones/zone-1/policy-sets/set-1/versions",
				"PATCH /zones/zone-1/policy-sets/set-1/versions/sv-2",
			},
			check: checkSecondUser,
		},
		{
			name:       "a zone with no default schema cannot take a policy version",
			seed:       func(f *zoneAPI) { f.schemaVersion = "" },
			wantWrites: freshWrites()[:9],
			wantErr:    "not found: default policy schema version in the zone",
		},
	})
}

// checkFreshZonePolicy pins the credential, the policy text and the policy
// set version that was published and activated.
func checkFreshZonePolicy(t *testing.T, f *zoneAPI) {
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
	wantPolicy := `permit (
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
	set := f.bodyOf("POST /zones/zone-1/policy-sets")
	if set["name"] != zonesetup.DefaultPolicySetName || set["target_type"] != "zone" {
		t.Errorf("policy set body = %v", set)
	}
	sv := f.bodyOf("POST /zones/zone-1/policy-sets/set-1/versions")
	wantManifest := map[string]any{"entries": []any{map[string]any{"policy_id": "pol-1", "policy_version_id": "pv-1"}}}
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

func checkSecondUser(t *testing.T, f *zoneAPI) {
	t.Helper()
	cred := f.bodyOf("POST /zones/zone-1/application-credentials")
	if cred["subject"] != "system:serviceaccount:agents:usr-2" {
		t.Errorf("second credential subject = %v", cred["subject"])
	}
	if got := f.bodyOf("POST /zones/zone-1/policies/pol-2/versions")["cedar_raw"]; got != zonesetup.ImpersonationPolicy(
		zonesetup.DefaultNamespace,
		"usr-2",
	) {
		t.Errorf("second policy text = %v", got)
	}
	sv := f.bodyOf("POST /zones/zone-1/policy-sets/set-1/versions")
	wantManifest := map[string]any{"entries": []any{
		map[string]any{"policy_id": "pol-1", "policy_version_id": "pv-1"},
		map[string]any{"policy_id": "pol-2", "policy_version_id": "pv-2"},
	}}
	if !reflect.DeepEqual(sv["manifest"], wantManifest) {
		t.Errorf("manifest = %v, want %v", sv["manifest"], wantManifest)
	}
}
