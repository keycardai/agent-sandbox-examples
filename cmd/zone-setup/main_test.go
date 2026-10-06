package main

import (
	"bytes"
	"context"
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
	"github.com/urfave/cli/v3"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
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
}

func (f *fakeZone) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := r.Method + " " + r.URL.Path
	if r.URL.Query().Get("filter[active]") == "true" {
		call = listActiveSetsCall
	}
	sent, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	if r.Method == http.MethodGet {
		f.reads = append(f.reads, call)
	} else if r.URL.Path != "/service-account-token" {
		f.writes = append(f.writes, call)
		if f.sent == nil {
			f.sent = map[string]string{}
		}
		f.sent[call] = string(sent)
	}
	f.mu.Unlock()
	page := func(items string) string { return `{"items":[` + items + `],"pagination":{"after_cursor":""}}` }
	query := r.URL.Query()
	newPolicy := `{"id":"policy-new"}`
	if strings.Contains(string(sent), "Sandbox access") {
		newPolicy = `{"id":"access-new"}`
	}
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
		listBindingsCall:                   page(""),
		bindCall:                           `{"id":"credential-new","type":"token"}`,
		"GET /zones/zone-a/policy-schemas": page(`{"version":"2026-02-24","is_default":true}`),
		listPoliciesCall:                   page(""),
		postPolicyCall:                     newPolicy,
		postAccessVersionCall:              `{"id":"access-version-new","policy_id":"access-new"}`,
		postPolicyVersionCall:              `{"id":"version-new","policy_id":"policy-new"}`,
		listSetsCall:                       page(""),
		listActiveSetsCall:                 page(""),
		postSetCall:                        `{"id":"set-new"}`,
		postSetVersionCall:                 `{"id":"set-version-new"}`,
		activateCall:                       `{"id":"set-version-new","active":true}`,
	}[call]
	if override, ok := f.bodies[call]; ok {
		body = override
	}
	w.Header().Set("Content-Type", "application/json")
	if body == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_, _ = w.Write([]byte(body))
}

func TestEnsureUserAccess(t *testing.T) {
	t.Parallel()
	const (
		email     = "ada@example.com"
		namespace = "agents"
		subject   = "system:serviceaccount:agents:user-1"
	)
	values := &zonesetup.Values{Namespace: namespace, Users: []zonesetup.UserValues{{Email: email}}}
	pending := []userAccess{{
		UserEmail: email, UserID: adaID, ServiceAccount: subject, Namespace: namespace,
		UserIdentifier: adaIdentifier,
	}}
	cases := []struct {
		name          string
		dryRun        bool
		applicationID string
		wantAccess    []userAccess
		wantWrites    []string
		wantLog       string
	}{
		{
			name: "a run creates the missing credential", applicationID: applicationK,
			wantAccess: []userAccess{{
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
			cmd := newCommand()
			cmd.ErrWriter = &stderr
			var access []userAccess
			cmd.Action = func(ctx context.Context, cmd *cli.Command) error {
				var err error
				access, err = ensureUserAccess(ctx, cmd, session, tc.applicationID, "provider-k", values)
				return err
			}
			args := []string{"zone-setup", "--values", "unused"}
			if tc.dryRun {
				args = append(args, "--dry-run")
			}
			if err := cmd.Run(t.Context(), args); err != nil {
				t.Fatalf("Run: %v", err)
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

func connect(t *testing.T, fake *fakeZone) *zonesetup.Session {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	session, err := zonesetup.Connect(t.Context(), &zonesetup.Values{
		ManagementAPIURL:         server.URL,
		ZoneID:                   "zone-a",
		ManagementServiceAccount: zonesetup.ManagementServiceAccountValues{ClientID: "sa-1"},
	}, &zonesetup.Secrets{ManagementClientSecret: "sa-secret"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return session
}

func TestEnsureResources(t *testing.T) {
	t.Parallel()
	resources := []zonesetup.ResourceValues{
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
			var stdout, stderr bytes.Buffer
			cmd := newCommand()
			cmd.Writer, cmd.ErrWriter = &stdout, &stderr
			cmd.Action = func(ctx context.Context, cmd *cli.Command) error {
				tokens := map[string]string{"EXAMPLE_TOKEN": resourceToken}
				return ensureResources(ctx, cmd, session, tc.applicationID, resources, tokens)
			}
			args := []string{"zone-setup", "--values", "unused"}
			if tc.dryRun {
				args = append(args, "--dry-run")
			}
			if err := cmd.Run(t.Context(), args); (err != nil) != tc.wantErr {
				t.Fatalf("Run: err = %v, want an error: %t", err, tc.wantErr)
			}
			if !slices.Equal(fake.writes, tc.wantWrites) {
				t.Errorf("writes = %q, want %q", fake.writes, tc.wantWrites)
			}
			if got := stderr.String(); got != tc.wantLog || strings.Contains(got+stdout.String(), resourceToken) {
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
	listActiveSetsCall      = "GET /zones/zone-a/policy-sets?filter[active]=true"
	listSetsCall            = "GET /zones/zone-a/policy-sets"
	postPolicyCall          = "POST /zones/zone-a/policies"
	postPolicyVersionCall   = "POST /zones/zone-a/policies/policy-new/versions"
	postAccessVersionCall   = "POST /zones/zone-a/policies/access-new/versions"
	postSetCall             = "POST /zones/zone-a/policy-sets"
	postSetVersionCall      = "POST /zones/zone-a/policy-sets/set-new/versions"
	activateCall            = "PATCH /zones/zone-a/policy-sets/set-new/versions/set-version-new"
	listPoliciesCall        = "GET /zones/zone-a/policies"
	getSetVersionKCall      = "GET /zones/zone-a/policy-sets/set-k/versions/set-version-k"
	postSetKVersionCall     = "POST /zones/zone-a/policy-sets/set-k/versions"
	activateSetVersionLCall = "PATCH /zones/zone-a/policy-sets/set-k/versions/set-version-l"
)

// TestEnsurePolicies checks the writes and the published manifest for each
// state of the user's policies and the policy set. Stored versions hold Cedar
// JSON and no cedar_raw, as a zone returns them for format=json.
func TestEnsurePolicies(t *testing.T) {
	t.Parallel()
	const (
		namespace = "agents"
		issuer    = "https://oidc.example.com"
		setK      = `{"id":"set-k","name":"Kubernetes Sandbox"`
		// setVersionL is the response to publishing or activating set-version-l.
		setVersionL = `{"id":"set-version-l"}`
		// userPolicies are the entries of the user's existing policies.
		userPolicies = `{"policy_id":"policy-k","policy_version_id":"version-k"},` +
			`{"policy_id":"access-k","policy_version_id":"access-version-k"}`
	)
	user := keycard.User{ID: adaID, Identifier: adaIdentifier}
	access := []userAccess{{UserEmail: "ada@example.com", UserID: user.ID, UserIdentifier: user.Identifier}}
	impersonation := zonesetup.ImpersonationPolicy(namespace, issuer, user)
	accessPolicy := zonesetup.AccessPolicy(namespace, issuer, user)
	page := func(items string) string { return `{"items":[` + items + `],"pagination":{"after_cursor":""}}` }
	storedVersion := func(policyID, versionID, digest string) string {
		return page(`{"id":"` + versionID + `","policy_id":"` + policyID + `","cedar_json":{"staticPolicies":{` +
			`"p":{"effect":"permit","annotations":{"id":"p","sandbox_digest":"` + digest + `"}}},` +
			`"templates":{},"templateLinks":[]}}`)
	}
	ownPolicies := `{"id":"policy-k","name":"Sandbox impersonation: agents/user-1"},` +
		`{"id":"access-k","name":"Sandbox access: agents/user-1"}`
	existing := map[string]string{
		listPoliciesCall: page(ownPolicies),
		"GET /zones/zone-a/policies/policy-k/versions": storedVersion("policy-k", "version-k", impersonation.Digest),
		"GET /zones/zone-a/policies/access-k/versions": storedVersion("access-k", "access-version-k", accessPolicy.Digest),
		getSetVersionKCall: `{"id":"set-version-k","policy_set_id":"set-k","manifest":{"entries":[` + userPolicies + `]}}`,
	}
	merged := func(base, more map[string]string) map[string]string {
		out := maps.Clone(base)
		maps.Copy(out, more)
		return out
	}
	with := func(more map[string]string) map[string]string { return merged(existing, more) }
	archiveCall := func(policyID string) string { return "DELETE /zones/zone-a/policies/" + policyID }
	// pruned is an active set that holds a policy zone-setup does not manage,
	// one of another namespace, the user's policies, and the policies of a
	// user the values file does not list.
	pruned := with(map[string]string{
		listPoliciesCall: page(ownPolicies + `,{"id":"policy-gone","name":"Sandbox impersonation: agents/user-2"},` +
			`{"id":"access-gone","name":"Sandbox access: agents/user-2"},` +
			`{"id":"policy-ns2","name":"Sandbox impersonation: agents-2/user-1"}`),
		listSetsCall: page(setK + `,"active":true,"active_version_id":"set-version-k"}`),
		getSetVersionKCall: `{"id":"set-version-k","policy_set_id":"set-k","manifest":{"entries":[` +
			`{"policy_id":"policy-other","policy_version_id":"version-other"},` +
			`{"policy_id":"policy-ns2","policy_version_id":"version-ns2"},` + userPolicies + `,` +
			`{"policy_id":"policy-gone","policy_version_id":"version-gone"},` +
			`{"policy_id":"access-gone","policy_version_id":"access-version-gone"}]}}`,
		postSetKVersionCall:        setVersionL,
		activateSetVersionLCall:    setVersionL,
		archiveCall("policy-k"):    `{"id":"policy-k","archived_at":"2026-10-06T00:00:00Z"}`,
		archiveCall("access-k"):    `{"id":"access-k","archived_at":"2026-10-06T00:00:00Z"}`,
		archiveCall("policy-gone"): `{"id":"policy-gone","archived_at":"2026-10-06T00:00:00Z"}`,
		archiveCall("access-gone"): `{"id":"access-gone","archived_at":"2026-10-06T00:00:00Z"}`,
	})
	// The set update comes first, and archives follow in policy name order.
	prunedWrites := []string{
		postSetKVersionCall, activateSetVersionLCall, archiveCall("access-gone"), archiveCall("policy-gone"),
	}
	otherActive := map[string]string{
		listActiveSetsCall: page(`{"id":"set-other","name":"Production","active":true,"target_type":"zone"}`),
	}
	freshWrites := []string{
		postPolicyCall, postPolicyVersionCall, postPolicyCall, postAccessVersionCall,
		postSetCall, postSetVersionCall, activateCall,
	}
	freshManifest := `[{"policy_id":"policy-new","policy_version_id":"version-new"},` +
		`{"policy_id":"access-new","policy_version_id":"access-version-new"}]`
	cases := []struct {
		name         string
		dryRun       bool
		replace      bool
		noUsers      bool
		bodies       map[string]string
		wantWrites   []string
		wantManifest string
		wantLog      []string
		wantErr      error
		// wantCedar maps a policy version write to the Cedar text it sends.
		wantCedar map[string]string
	}{
		{
			name:         "a fresh zone gets both policies and an active set that holds them",
			wantWrites:   freshWrites,
			wantManifest: freshManifest,
			wantLog: []string{
				`created policy policy-new for "Sandbox impersonation: agents/user-1"`,
				"published policy policy-new version version-new",
				`created policy access-new for "Sandbox access: agents/user-1"`,
				`created policy set set-new for "Kubernetes Sandbox"`,
				"published policy set set-new version set-version-new with 2 policies",
				"activated policy set set-new version set-version-new",
			},
			wantCedar: map[string]string{
				postPolicyVersionCall: impersonation.Cedar,
				postAccessVersionCall: accessPolicy.Cedar,
			},
		},
		{
			name: "a complete zone is not written",
			bodies: with(map[string]string{
				listSetsCall: page(setK + `,"active":true,"active_version_id":"set-version-k"}`),
			}),
			wantLog: []string{
				`found policy policy-k version version-k for "Sandbox impersonation: agents/user-1"`,
				`found policy access-k version access-version-k for "Sandbox access: agents/user-1"`,
				"found policy set set-k version set-version-k active with every policy",
			},
		},
		{
			name: "a complete but inactive set is activated without a new version",
			bodies: with(map[string]string{
				listSetsCall: page(setK + `,"latest_version_id":"set-version-k"}`),
				"PATCH /zones/zone-a/policy-sets/set-k/versions/set-version-k": `{"id":"set-version-k"}`,
			}),
			wantWrites: []string{"PATCH /zones/zone-a/policy-sets/set-k/versions/set-version-k"},
		},
		{
			name: "a changed policy publishes new versions and keeps entries the run does not manage",
			bodies: with(map[string]string{
				"GET /zones/zone-a/policies/policy-k/versions":  storedVersion("policy-k", "version-k", "an-older-digest"),
				"POST /zones/zone-a/policies/policy-k/versions": `{"id":"version-l","policy_id":"policy-k"}`,
				listSetsCall: page(setK + `,"active":true,"active_version_id":"set-version-k"}`),
				getSetVersionKCall: `{"id":"set-version-k","policy_set_id":"set-k","manifest":{"entries":[` +
					`{"policy_id":"policy-other","policy_version_id":"version-other"},` + userPolicies + `]}}`,
				postSetKVersionCall:     setVersionL,
				activateSetVersionLCall: setVersionL,
			}),
			wantWrites: []string{
				"POST /zones/zone-a/policies/policy-k/versions",
				postSetKVersionCall,
				activateSetVersionLCall,
			},
			wantManifest: `[{"policy_id":"policy-other","policy_version_id":"version-other"},` +
				`{"policy_id":"policy-k","policy_version_id":"version-l"},` +
				`{"policy_id":"access-k","policy_version_id":"access-version-k"}]`,
		},
		{
			name:       "a removed user's policies leave the set and are archived, and other namespaces stay",
			bodies:     pruned,
			wantWrites: prunedWrites,
			wantManifest: `[{"policy_id":"policy-other","policy_version_id":"version-other"},` +
				`{"policy_id":"policy-ns2","policy_version_id":"version-ns2"},` + userPolicies + `]`,
			wantLog: []string{
				`archived policy access-gone for "Sandbox access: agents/user-2"`,
				`archived policy policy-gone for "Sandbox impersonation: agents/user-2"`,
			},
		},
		{
			name: "a values file without users removes and archives the namespace's policies", noUsers: true,
			bodies: pruned,
			wantWrites: []string{
				postSetKVersionCall, activateSetVersionLCall, archiveCall("access-k"), archiveCall("access-gone"),
				archiveCall("policy-k"), archiveCall("policy-gone"),
			},
			wantManifest: `[{"policy_id":"policy-other","policy_version_id":"version-other"},` +
				`{"policy_id":"policy-ns2","policy_version_id":"version-ns2"}]`,
		},
		{
			name:       "a failed set update archives nothing",
			bodies:     merged(pruned, map[string]string{postSetKVersionCall: ""}),
			wantWrites: []string{postSetKVersionCall},
			wantErr:    zonesetup.ErrAPI,
		},
		{
			name: "a dry run reports the archives it would make", dryRun: true, bodies: pruned,
			wantLog: []string{
				`would archive policy access-gone for "Sandbox access: agents/user-2"`,
				`would archive policy policy-gone for "Sandbox impersonation: agents/user-2"`,
			},
		},
		{
			name:    "another active set is refused before any write",
			bodies:  otherActive,
			wantErr: zonesetup.ErrPolicySetActive,
		},
		{
			name: "a dry run reports the same refusal", dryRun: true, bodies: otherActive,
			wantErr: zonesetup.ErrPolicySetActive,
		},
		{
			name: "the flag replaces another active set", replace: true, bodies: otherActive,
			wantWrites:   freshWrites,
			wantManifest: freshManifest,
			wantLog:      []string{`policy set "Production" (set-other) is active and will be replaced`},
		},
		{
			name: "a dry run over a fresh zone writes nothing", dryRun: true,
			wantLog: []string{
				`would create the policy "Sandbox impersonation: agents/user-1"`,
				`would create the policy "Sandbox access: agents/user-1"`,
				`would publish a version of policy set "Kubernetes Sandbox" with 2 policies and activate it`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeZone{bodies: tc.bodies}
			session := connect(t, fake)
			var stderr bytes.Buffer
			cmd := newCommand()
			cmd.ErrWriter = &stderr
			cmd.Action = func(ctx context.Context, cmd *cli.Command) error {
				values := &zonesetup.Values{
					Namespace: namespace, IssuerURL: issuer, PolicySet: zonesetup.DefaultPolicySetName,
				}
				users := access
				if tc.noUsers {
					users = nil
				}
				return ensurePolicies(ctx, cmd, session, values, users)
			}
			args := []string{
				cmd.Name, "--values=unused", "--dry-run=" + strconv.FormatBool(tc.dryRun),
				"--" + replaceActiveFlag + "=" + strconv.FormatBool(tc.replace),
			}
			if err := cmd.Run(t.Context(), args); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Run: err = %v, want %v", err, tc.wantErr)
			}
			if !slices.Equal(fake.writes, tc.wantWrites) {
				t.Errorf("writes = %q, want %q", fake.writes, tc.wantWrites)
			}
			if tc.wantManifest != "" {
				assertManifest(t, fake, tc.wantManifest)
			}
			assertLogLines(t, stderr.String(), tc.wantLog)
			for call, want := range tc.wantCedar {
				var version struct {
					CedarRaw string `json:"cedar_raw"`
				}
				if err := json.Unmarshal([]byte(fake.sent[call]), &version); err != nil || version.CedarRaw != want {
					t.Errorf("%s sent cedar_raw %q, want %q", call, version.CedarRaw, want)
				}
			}
		})
	}
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

// assertManifest compares the entries of the one policy set version the run
// published.
func assertManifest(t *testing.T, fake *fakeZone, want string) {
	t.Helper()
	var published []string
	for call, body := range fake.sent {
		if strings.HasPrefix(call, "POST /zones/zone-a/policy-sets/") && strings.HasSuffix(call, "/versions") {
			published = append(published, body)
		}
	}
	if len(published) != 1 {
		t.Fatalf("published %d policy set versions, want 1", len(published))
	}
	var got struct {
		Manifest struct {
			Entries json.RawMessage `json:"entries"`
		} `json:"manifest"`
	}
	if err := json.Unmarshal([]byte(published[0]), &got); err != nil {
		t.Fatalf("policy set version body %s: %v", published[0], err)
	}
	var gotEntries, wantEntries any
	if json.Unmarshal(got.Manifest.Entries, &gotEntries) != nil || json.Unmarshal([]byte(want), &wantEntries) != nil ||
		!reflect.DeepEqual(gotEntries, wantEntries) {
		t.Errorf("manifest entries = %s, want %s", got.Manifest.Entries, want)
	}
}
