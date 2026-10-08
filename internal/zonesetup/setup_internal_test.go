package zonesetup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/keycardai/keycard-go"
)

const (
	applicationK     = "application-k"
	bindCall         = "POST /zones/zone-a/application-credentials"
	listBindingsCall = "GET /zones/zone-a/application-credentials"
	listStoredCall   = "GET /zones/zone-a/secrets"
	postResources    = "POST /zones/zone-a/resources"
	storeCall        = "POST /zones/zone-a/secrets"
	resourceToken    = "resource-token-secret"
	adaID            = "user-1"
	adaIdentifier    = "ada-identifier"
)

type fakeZone struct {
	bodies map[string]string
	mu     sync.Mutex
	writes []string
	reads  []string
	// sent maps a write to the body of its last request.
	sent map[string]string
	// ifMatch maps a write to the If-Match header of its last request.
	ifMatch map[string]string
}

func (f *fakeZone) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := r.Method + " " + r.URL.Path
	if r.URL.Query().Get("filter[active]") == "true" {
		call = listActiveSetsCall
	}
	if version := r.URL.Query().Get("version"); version != "" {
		call += "?version=" + version
	}
	sent, _ := io.ReadAll(r.Body)
	f.record(r, call, string(sent))
	query := r.URL.Query()
	body := map[string]string{
		"POST /service-account-token": `{"access_token":"sa-token","token_type":"Bearer"}`,
		"GET /zones/zone-a":           `{"id":"zone-a"}`,
		listStoredCall:                `[]`,
		postResources:                 `{"id":"resource-new"}`,
		storeCall:                     `{"id":"secret-new"}`,
		"GET /zones/zone-a/providers": page(map[string]string{
			"github":        `{"id":"provider-gh","identifier":"github"}`,
			"keycard-vault": `{"id":"provider-vault","type":"keycard-vault"}`,
		}[query.Get("filter[identifier]")+query.Get("filter[type]")]),
		"GET /zones/zone-a/resources": page(map[string]string{
			"https://api.github.com": `{"id":"resource-gh","identifier":"https://api.github.com",` +
				`"credential_provider_id":"provider-gh"}`,
		}[query.Get("filter[identifier]")]),
		"GET /zones/zone-a/applications/application-k/dependencies": page(`{"id":"resource-gh"}`),
		"GET /zones/zone-a/users": page(map[string]string{
			"ada@example.com": `{"id":"user-1","email":"ada@example.com","identifier":"ada-identifier"}`,
		}[query.Get("filter[email]")]),
		listBindingsCall:   page(""),
		bindCall:           `{"id":"credential-new","type":"token"}`,
		listPoliciesCall:   page(""),
		listSetsCall:       page(""),
		listActiveSetsCall: page(""),
		postSetCall:        `{"id":"set-new"}`,
		getManifestNewCall: `{"policy_set_id":"set-new","policy_set_version_id":null,"schema_version":null,"entries":[]}`,
		applyNewCall: `{"created":true,"policy_set_version":{"id":"set-version-new"},` +
			`"binding":{"policy_set_version_id":"set-version-new"},"changes":[` +
			`{"policy_id":"policy-new","name":"Sandbox impersonation: ada@example.com","action":"created_policy"}]}`,
	}[call]
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/manifest") && query.Get("version") == "" {
		w.Header().Set("ETag", latestETag)
	}
	if override, ok := f.bodies[call]; ok {
		body = override
	}
	body = onlyIDs(body, query["filter[id]"])
	w.Header().Set("Content-Type", "application/json")
	if body == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_, _ = w.Write([]byte(body))
}

// page is a complete page of a list response.
func page(items string) string {
	return `{"items":[` + items + `],"pagination":{"after_cursor":""}}`
}

// manifest is the manifest of version versionID of set-k.
func manifest(versionID, schema, entries string) string {
	return `{"policy_set_id":"set-k","policy_set_version_id":"` + versionID + `","schema_version":"` + schema +
		`","entries":[` + entries + `]}`
}

// applied is an apply response that activates set-version-l.
func applied(created bool) string {
	return `{"created":` + strconv.FormatBool(created) + `,"policy_set_version":{"id":"set-version-l"},` +
		`"binding":{"policy_set_version_id":"set-version-l"},"changes":[]}`
}

// archived is the response to archiving the policy.
func archived(policyID string) string {
	return `{"id":"` + policyID + `","archived_at":"2026-10-06T00:00:00Z"}`
}

// pin is a manifest entry that pins one version of a policy.
func pin(policyID, versionID string) ApplyEntry {
	return ApplyEntry{PolicyID: policyID, PolicyVersionID: versionID}
}

// merged returns base with more's entries added or replaced.
func merged(base, more map[string]string) map[string]string {
	out := maps.Clone(base)
	maps.Copy(out, more)
	return out
}

// onlyIDs keeps the items of a page whose id is in ids, as svc-pdp answers
// filter[id]. It returns page unchanged when ids is empty.
func onlyIDs(page string, ids []string) string {
	if len(ids) == 0 {
		return page
	}
	var all struct {
		Items []json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal([]byte(page), &all)
	kept := []string{}
	for _, item := range all.Items {
		var policy struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(item, &policy) == nil && slices.Contains(ids, policy.ID) {
			kept = append(kept, string(item))
		}
	}
	return `{"items":[` + strings.Join(kept, ",") + `],"pagination":{"after_cursor":""}}`
}

func (f *fakeZone) record(r *http.Request, call, sent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		f.reads = append(f.reads, call)
		return
	}
	if r.URL.Path == "/service-account-token" {
		return
	}
	f.writes = append(f.writes, call)
	if f.sent == nil {
		f.sent, f.ifMatch = map[string]string{}, map[string]string{}
	}
	f.sent[call] = sent
	f.ifMatch[call] = r.Header.Get("If-Match")
}

func TestEnsureUserAccess(t *testing.T) {
	t.Parallel()
	const (
		email     = "ada@example.com"
		namespace = "agents"
		subject   = "system:serviceaccount:agents:user-1"
	)
	values := &Values{Namespace: namespace, Users: []UserValues{{Email: email}}}
	pending := []UserAccess{{
		UserEmail: email, UserID: adaID, ServiceAccount: subject, Namespace: namespace,
		UserIdentifier: adaIdentifier,
	}}
	cases := []struct {
		name          string
		dryRun        bool
		applicationID string
		wantAccess    []UserAccess
		wantWrites    []string
		wantLog       string
	}{
		{
			name: "a run creates the missing credential", applicationID: applicationK,
			wantAccess: []UserAccess{{
				UserEmail: email, UserID: adaID, ServiceAccount: subject,
				ClientID: "credential-new", Namespace: namespace, UserIdentifier: adaIdentifier,
			}},
			wantWrites: []string{bindCall},
			wantLog: "found user user-1 for ada@example.com\n" +
				"created token credential credential-new for " + subject + "\n",
		},
		{
			name: "a dry run writes nothing", dryRun: true, applicationID: applicationK, wantAccess: pending,
			wantLog: "found user user-1 for ada@example.com\nwould create a token credential for " + subject + "\n",
		},
		{
			name: "a dry run without the application lists no credentials", dryRun: true, wantAccess: pending,
			wantLog: "found user user-1 for ada@example.com\nwould create a token credential for " + subject + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeZone{}
			session := connect(t, fake)
			var stderr bytes.Buffer
			opts := &Options{DryRun: tc.dryRun, Log: &stderr}
			access, err := ensureUserAccess(t.Context(), opts, session, tc.applicationID, "provider-k", values)
			if err != nil {
				t.Fatalf("ensureUserAccess: %v", err)
			}
			if !slices.Equal(access, tc.wantAccess) {
				t.Errorf("access = %+v, want %+v", access, tc.wantAccess)
			}
			if !slices.Equal(fake.writes, tc.wantWrites) {
				t.Errorf("writes = %q, want %q", fake.writes, tc.wantWrites)
			}
			if got := stderr.String(); got != tc.wantLog {
				t.Errorf("stderr = %q, want %q", got, tc.wantLog)
			}
			if listed := slices.Contains(fake.reads, listBindingsCall); listed != (tc.applicationID != "") {
				t.Errorf("listed credentials = %t, want %t", listed, tc.applicationID != "")
			}
		})
	}
}

func connect(t *testing.T, fake *fakeZone) *Session {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	session, err := Connect(t.Context(), &Values{
		ManagementAPIURL:         server.URL,
		ZoneID:                   "zone-a",
		ManagementServiceAccount: ManagementServiceAccountValues{ClientID: "sa-1"},
	}, &Secrets{ManagementClientSecret: "sa-secret"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return session
}

func TestEnsureResources(t *testing.T) {
	t.Parallel()
	resources := []ResourceValues{
		{Identifier: "https://api.github.com", Name: "GitHub", CredentialProvider: "github"},
		{Identifier: "https://api.example.com", Name: "Example", SecretEnv: "EXAMPLE_TOKEN"},
	}
	cases := []struct {
		name          string
		dryRun        bool
		applicationID string
		bodies        map[string]string
		wantWrites    []string
		wantLog       string
		wantErr       bool
	}{
		{
			name: "a run creates what is missing", applicationID: applicationK,
			wantWrites: []string{
				postResources,
				storeCall,
				"PUT /zones/zone-a/applications/application-k/dependencies/resource-new",
			},
			wantLog: "found resource resource-gh for https://api.github.com\n" +
				"created resource resource-new for https://api.example.com\n" +
				"stored the EXAMPLE_TOKEN credential for https://api.example.com\n" +
				"found resource resource-gh as a dependency\n" +
				"added resource resource-new as a dependency\n",
		},
		{
			name: "a null secret response stops the run before the dependencies", applicationID: applicationK,
			bodies:     map[string]string{storeCall: "null"},
			wantWrites: []string{postResources, storeCall},
			wantLog: "found resource resource-gh for https://api.github.com\n" +
				"created resource resource-new for https://api.example.com\n",
			wantErr: true,
		},
		{
			name: "a listed secret without entity_id is refused before a create", applicationID: applicationK,
			bodies:     map[string]string{listStoredCall: `[{"id":"secret-x","type":"token"}]`},
			wantWrites: []string{postResources},
			wantLog: "found resource resource-gh for https://api.github.com\n" +
				"created resource resource-new for https://api.example.com\n",
			wantErr: true,
		},
		{
			name: "a password secret beside the token is refused", applicationID: applicationK,
			bodies: map[string]string{listStoredCall: `[` +
				`{"id":"secret-t","entity_id":"resource-new","type":"token"},` +
				`{"id":"secret-p","entity_id":"resource-new","type":"password"}]`},
			wantWrites: []string{postResources},
			wantLog: "found resource resource-gh for https://api.github.com\n" +
				"created resource resource-new for https://api.example.com\n",
			wantErr: true,
		},
		{
			name: "a dry run writes nothing", dryRun: true, applicationID: applicationK,
			wantLog: "found resource resource-gh for https://api.github.com\n" +
				"would create a resource for https://api.example.com\n" +
				"would store the EXAMPLE_TOKEN credential for https://api.example.com\n" +
				"found resource resource-gh as a dependency\n" +
				"would add the resource for https://api.example.com as a dependency\n",
		},
		{
			name: "a dry run without the application lists no dependencies", dryRun: true,
			wantLog: "found resource resource-gh for https://api.github.com\n" +
				"would create a resource for https://api.example.com\n" +
				"would store the EXAMPLE_TOKEN credential for https://api.example.com\n" +
				"would add the resource for https://api.github.com as a dependency\n" +
				"would add the resource for https://api.example.com as a dependency\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeZone{bodies: tc.bodies}
			session := connect(t, fake)
			var stderr bytes.Buffer
			opts := &Options{DryRun: tc.dryRun, Log: &stderr}
			tokens := map[string]string{"EXAMPLE_TOKEN": resourceToken}
			err := ensureResources(t.Context(), opts, session, tc.applicationID, resources, tokens)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ensureResources: err = %v, want an error: %t", err, tc.wantErr)
			}
			if !slices.Equal(fake.writes, tc.wantWrites) {
				t.Errorf("writes = %q, want %q", fake.writes, tc.wantWrites)
			}
			if got := stderr.String(); got != tc.wantLog || strings.Contains(got, resourceToken) {
				t.Errorf("stderr = %q, want %q", got, tc.wantLog)
			}
			listedDependencies := slices.ContainsFunc(fake.reads, func(call string) bool {
				return strings.HasSuffix(call, "/dependencies")
			})
			if want := tc.applicationID != "" && !tc.wantErr; listedDependencies != want {
				t.Errorf("listed dependencies = %t, want %t", listedDependencies, want)
			}
		})
	}
}

const (
	listActiveSetsCall = "GET /zones/zone-a/policy-sets?filter[active]=true"
	listSetsCall       = "GET /zones/zone-a/policy-sets"
	listPoliciesCall   = "GET /zones/zone-a/policies"
	postSetCall        = "POST /zones/zone-a/policy-sets"
	getManifestNewCall = "GET /zones/zone-a/policy-sets/set-new/manifest"
	applyNewCall       = "PUT /zones/zone-a/policy-sets/set-new/manifest"
	getManifestKCall   = "GET /zones/zone-a/policy-sets/set-k/manifest"
	applyKCall         = "PUT /zones/zone-a/policy-sets/set-k/manifest"
	latestETag         = `"etag-latest"`
)

// TestEnsurePolicies checks the writes and the applied content for each state
// of the user's policies and the policy set. An apply to an existing set sends
// the schema of the version it starts from.
func TestEnsurePolicies(t *testing.T) {
	t.Parallel()
	const (
		namespace = "agents"
		issuer    = "https://oidc.example.com"
		setK      = `{"id":"set-k","name":"Kubernetes Sandbox"`
		activeK   = setK + `,"active":true,"active_version_id":"set-version-k"}`
		setSchema = "2026-03-16"
		// userPolicies are the entries of the user's existing policies.
		userPolicies = `{"policy_id":"policy-k","policy_version_id":"version-k"},` +
			`{"policy_id":"access-k","policy_version_id":"access-version-k"}`
		ownPolicies = `{"id":"policy-k","name":"Sandbox impersonation: ada@example.com"},` +
			`{"id":"access-k","name":"Sandbox access: ada@example.com"}`
		// otherPolicies are live policies in the set that zone-setup does not manage.
		otherPolicies = `{"id":"policy-other","name":"Production"},{"id":"policy-old","name":"Production v1"}`
	)
	user := keycard.User{ID: adaID, Identifier: adaIdentifier, Email: "ada@example.com"}
	access := []UserAccess{{UserEmail: user.Email, UserID: user.ID, UserIdentifier: user.Identifier}}
	impersonation := ImpersonationPolicy(namespace, issuer, ApplicationName, user)
	accessPolicy := AccessPolicy(namespace, issuer, ApplicationName, user)
	placeholder := PlaceholderPolicy()
	existing := map[string]string{
		listPoliciesCall: page(ownPolicies + "," + otherPolicies),
		listSetsCall:     page(activeK),
		getManifestKCall: manifest(
			"set-version-k",
			setSchema,
			`{"policy_id":"policy-other","policy_version_id":"version-other"},`+
				userPolicies,
		),
		applyKCall: applied(true),
	}
	with := func(more map[string]string) map[string]string { return merged(existing, more) }
	archiveCall := func(policyID string) string { return "DELETE /zones/zone-a/policies/" + policyID }
	// pruned is an active set that holds a policy zone-setup does not manage,
	// one of another namespace, the user's policies, and the policies of a
	// user the values file does not list.
	pruned := with(map[string]string{
		listPoliciesCall: page(ownPolicies + "," + otherPolicies +
			`,{"id":"policy-gone","name":"Sandbox impersonation: bob@example.com"},` +
			`{"id":"access-gone","name":"Sandbox access: bob@example.com"},` +
			`{"id":"policy-ns2","name":"Sandbox (agents-2) impersonation: ada@example.com"}`),
		getManifestKCall: manifest("set-version-k", setSchema,
			`{"policy_id":"policy-other","policy_version_id":"version-other"},`+
				`{"policy_id":"policy-ns2","policy_version_id":"version-ns2"},`+userPolicies+`,`+
				`{"policy_id":"policy-gone","policy_version_id":"version-gone"},`+
				`{"policy_id":"access-gone","policy_version_id":"access-version-gone"}`),
		archiveCall("policy-k"):    archived("policy-k"),
		archiveCall("access-k"):    archived("access-k"),
		archiveCall("policy-gone"): archived("policy-gone"),
		archiveCall("access-gone"): archived("access-gone"),
	})
	// lastUser is an active set that holds only the user's policies.
	lastUser := with(map[string]string{
		getManifestKCall:        manifest("set-version-k", setSchema, userPolicies),
		archiveCall("policy-k"): archived("policy-k"),
		archiveCall("access-k"): archived("access-k"),
	})
	lastUserWrites := []string{applyKCall, archiveCall("access-k"), archiveCall("policy-k")}
	otherActive := map[string]string{
		listActiveSetsCall: page(`{"id":"set-other","name":"Production","active":true,"target_type":"zone"}`),
	}
	userEntries := []ApplyEntry{
		{PolicyID: "policy-k", CedarRaw: impersonation.Cedar},
		{PolicyID: "access-k", CedarRaw: accessPolicy.Cedar},
	}
	freshEntries := []ApplyEntry{
		{
			NewPolicy: &NewPolicy{Name: impersonation.Name, Description: impersonation.Description},
			CedarRaw:  impersonation.Cedar,
		},
		{
			NewPolicy: &NewPolicy{Name: accessPolicy.Name, Description: accessPolicy.Description},
			CedarRaw:  accessPolicy.Cedar,
		},
	}
	values := &Values{Namespace: namespace, IssuerURL: issuer, PolicySet: DefaultPolicySetName}
	cases := []ensurePoliciesCase{
		{
			name:       "a fresh zone gets a new set that creates both policies and is active",
			wantWrites: []string{postSetCall, applyNewCall},
			wantApply:  &applyBody{Entries: freshEntries},
			wantLog: []string{
				`created policy set set-new for "Kubernetes Sandbox"`,
				`policy policy-new for "Sandbox impersonation: ada@example.com": created_policy`,
				"published policy set set-new version set-version-new with 2 policies",
				"policy set set-new version set-version-new is active",
			},
		},
		{
			name:       "a new set takes the policies the zone already has",
			bodies:     map[string]string{listPoliciesCall: page(ownPolicies)},
			wantWrites: []string{postSetCall, applyNewCall},
			wantApply:  &applyBody{Entries: userEntries},
		},
		{
			name: "an unchanged set is applied under its own schema and keeps entries the run does not manage",
			bodies: with(map[string]string{
				applyKCall: applied(false),
			}),
			wantWrites: []string{applyKCall},
			wantApply: &applyBody{
				SchemaVersion: setSchema,
				Entries:       append([]ApplyEntry{pin("policy-other", "version-other")}, userEntries...),
			},
			wantLog: []string{"policy set set-k version set-version-l is active"},
		},
		{
			name: "an active version older than the latest is the base, with its own schema",
			bodies: with(map[string]string{
				getManifestKCall: manifest("set-version-m", "2026-06-18", userPolicies),
				getManifestKCall + "?version=set-version-k": manifest("set-version-k", setSchema,
					`{"policy_id":"policy-old","policy_version_id":"version-old"}`),
			}),
			wantWrites: []string{applyKCall},
			wantApply: &applyBody{
				SchemaVersion: setSchema,
				Entries:       append([]ApplyEntry{pin("policy-old", "version-old")}, userEntries...),
			},
		},
		{
			name: "an entry for an archived policy leaves the set",
			bodies: with(map[string]string{
				getManifestKCall: manifest("set-version-k", setSchema,
					`{"policy_id":"policy-archived","policy_version_id":"version-archived"},`+
						`{"policy_id":"policy-other","policy_version_id":"version-other"},`+userPolicies),
			}),
			wantWrites: []string{applyKCall},
			wantApply: &applyBody{
				SchemaVersion: setSchema,
				Entries:       append([]ApplyEntry{pin("policy-other", "version-other")}, userEntries...),
			},
			wantLog: []string{"dropping policy policy-archived from the set: the zone has no unarchived policy with that id"},
		},
		{
			name: "an archived policy of a listed user leaves the set and is created again",
			bodies: with(map[string]string{
				listPoliciesCall: page(`{"id":"access-k","name":"Sandbox access: ada@example.com"},` + otherPolicies),
			}),
			wantWrites: []string{applyKCall},
			wantApply: &applyBody{
				SchemaVersion: setSchema,
				Entries: []ApplyEntry{
					pin("policy-other", "version-other"),
					{PolicyID: "access-k", CedarRaw: accessPolicy.Cedar},
					freshEntries[0],
				},
			},
		},
		{
			name:       "a removed user's policies leave the set and are archived, and other namespaces stay",
			bodies:     pruned,
			wantWrites: []string{applyKCall, archiveCall("access-gone"), archiveCall("policy-gone")},
			wantApply: &applyBody{
				SchemaVersion: setSchema,
				Entries: append([]ApplyEntry{
					pin("policy-other", "version-other"), pin("policy-ns2", "version-ns2"),
				}, userEntries...),
			},
			wantLog: []string{
				`archived policy access-gone for "Sandbox access: bob@example.com"`,
				`archived policy policy-gone for "Sandbox impersonation: bob@example.com"`,
			},
		},
		{
			name: "a values file without users removes and archives the namespace's policies", noUsers: true,
			bodies: pruned,
			wantWrites: []string{
				applyKCall, archiveCall("access-k"), archiveCall("access-gone"),
				archiveCall("policy-k"), archiveCall("policy-gone"),
			},
			wantApply: &applyBody{
				SchemaVersion: setSchema,
				Entries:       []ApplyEntry{pin("policy-other", "version-other"), pin("policy-ns2", "version-ns2")},
			},
		},
		{
			name:       "removing the last user from a set of only the namespace's policies adds the placeholder",
			noUsers:    true,
			bodies:     lastUser,
			wantWrites: lastUserWrites,
			wantApply: &applyBody{
				SchemaVersion: setSchema,
				Entries: []ApplyEntry{{
					NewPolicy: &NewPolicy{Name: placeholder.Name, Description: placeholder.Description},
					CedarRaw:  placeholder.Cedar,
				}},
			},
		},
		{
			name: "the placeholder the zone already has is reused", noUsers: true,
			bodies: merged(lastUser, map[string]string{
				listPoliciesCall: page(ownPolicies + `,{"id":"placeholder-k","name":"Sandbox placeholder"}`),
			}),
			wantWrites: lastUserWrites,
			wantApply: &applyBody{
				SchemaVersion: setSchema,
				Entries:       []ApplyEntry{{PolicyID: "placeholder-k", CedarRaw: placeholder.Cedar}},
			},
		},
		{
			name:       "a failed apply archives nothing",
			bodies:     merged(pruned, map[string]string{applyKCall: ""}),
			wantWrites: []string{applyKCall},
			wantErr:    ErrAPI,
		},
		{
			name: "a dry run reports the apply and the archives it would make", dryRun: true, bodies: pruned,
			wantLog: []string{
				"would apply policy set set-k with 4 policies and activate it",
				`would archive policy access-gone for "Sandbox access: bob@example.com"`,
				`would archive policy policy-gone for "Sandbox impersonation: bob@example.com"`,
			},
		},
		{
			name:    "another active set is refused before any write",
			bodies:  otherActive,
			wantErr: ErrPolicySetActive,
		},
		{
			name: "a dry run reports the same refusal", dryRun: true, bodies: otherActive,
			wantErr: ErrPolicySetActive,
		},
		{
			name: "the flag replaces another active set", replace: true, bodies: otherActive,
			wantWrites: []string{postSetCall, applyNewCall},
			wantApply:  &applyBody{Entries: freshEntries},
			wantLog:    []string{`policy set "Production" (set-other) is active and will be replaced`},
		},
		{
			name: "a dry run over a fresh zone writes nothing", dryRun: true,
			wantLog: []string{`would create the policy set "Kubernetes Sandbox" with 2 policies and activate it`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runEnsurePolicies(t, tc, values, access)
		})
	}
}

// ensurePoliciesCase is one state of the zone for TestEnsurePolicies.
type ensurePoliciesCase struct {
	name       string
	dryRun     bool
	replace    bool
	noUsers    bool
	bodies     map[string]string
	wantWrites []string
	// wantApply is the body of the one apply, or nil when the run does not apply.
	wantApply *applyBody
	wantLog   []string
	wantErr   error
}

// runEnsurePolicies runs ensurePolicies for access, or for no users when
// tc.noUsers is set, against a fake zone that serves tc.bodies.
func runEnsurePolicies(t *testing.T, tc ensurePoliciesCase, values *Values, access []UserAccess) {
	t.Helper()
	fake := &fakeZone{bodies: tc.bodies}
	session := connect(t, fake)
	var stderr bytes.Buffer
	users := access
	if tc.noUsers {
		users = nil
	}
	opts := &Options{DryRun: tc.dryRun, ReplaceActive: tc.replace, Log: &stderr}
	if err := ensurePolicies(t.Context(), opts, session, values, ApplicationName, users); !errors.Is(err, tc.wantErr) {
		t.Fatalf("ensurePolicies: err = %v, want %v", err, tc.wantErr)
	}
	if !slices.Equal(fake.writes, tc.wantWrites) {
		t.Errorf("writes = %q, want %q", fake.writes, tc.wantWrites)
	}
	if tc.wantApply != nil {
		assertApply(t, fake, *tc.wantApply)
	}
	assertLogLines(t, stderr.String(), tc.wantLog)
}

// applyBody is the request body of a policy set apply.
type applyBody struct {
	SchemaVersion string       `json:"schema_version"`
	Activate      bool         `json:"activate"`
	Entries       []ApplyEntry `json:"entries"`
}

// assertLogLines requires each line to appear in the log as a whole line.
func assertLogLines(t *testing.T, log string, lines []string) {
	t.Helper()
	for _, line := range lines {
		if !slices.Contains(strings.Split(log, "\n"), line) {
			t.Errorf("stderr = %q, want the line %q", log, line)
		}
	}
}

// assertApply compares the one policy set apply the run sent. Every apply
// activates and sends the latest manifest's ETag as If-Match.
func assertApply(t *testing.T, fake *fakeZone, want applyBody) {
	t.Helper()
	var calls []string
	for call := range fake.sent {
		if strings.HasPrefix(call, "PUT ") && strings.HasSuffix(call, "/manifest") {
			calls = append(calls, call)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("applied %d policy sets, want 1", len(calls))
	}
	var got applyBody
	if err := json.Unmarshal([]byte(fake.sent[calls[0]]), &got); err != nil {
		t.Fatalf("apply body %s: %v", fake.sent[calls[0]], err)
	}
	want.Activate = true
	if !reflect.DeepEqual(got, want) {
		t.Errorf("apply body = %+v, want %+v", got, want)
	}
	if ifMatch := fake.ifMatch[calls[0]]; ifMatch != latestETag {
		t.Errorf("apply If-Match = %q, want %q", ifMatch, latestETag)
	}
}
