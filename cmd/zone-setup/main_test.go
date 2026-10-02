package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

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
)

type fakeZone struct {
	bodies map[string]string
	mu     sync.Mutex
	writes []string
	reads  []string
}

func (f *fakeZone) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	call := r.Method + " " + r.URL.Path
	f.mu.Lock()
	if r.Method == http.MethodGet {
		f.reads = append(f.reads, call)
	} else if r.URL.Path != "/service-account-token" {
		f.writes = append(f.writes, call)
	}
	f.mu.Unlock()
	page := func(items string) string { return `{"items":[` + items + `],"pagination":{"after_cursor":""}}` }
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
			"ada@example.com": `{"id":"user-1","email":"ada@example.com"}`,
		}[query.Get("filter[email]")]),
		listBindingsCall: page(""),
		bindCall:         `{"id":"credential-new","type":"token"}`,
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
	pending := []userAccess{{UserEmail: email, UserID: "user-1", ServiceAccount: subject, Namespace: namespace}}
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
				UserEmail: email, UserID: "user-1", ServiceAccount: subject,
				ClientID: "credential-new", Namespace: namespace,
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
