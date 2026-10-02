package sandbox_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

const (
	fakeClientID = "sa-1"
	fakeSecret   = "sa-secret"
	testZone     = "zone_1"
	bobSubject   = "system:serviceaccount:agents:usr_bob"
)

// fakeToken is a service account token whose payload names org_1.
func fakeToken() string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.keycard.sh/organization_id":"org_1"}`))
	return "header." + payload + ".signature"
}

// keycardServer answers the management API and svc-catalog calls the CLI
// makes, the way those services do.
func keycardServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /service-account-token", signIn)
	mux.HandleFunc("GET /catalog/zones/zone_1/packages", authorized(listPackages))
	mux.HandleFunc("POST /catalog/zones/zone_1/install", authorized(install))
	mux.HandleFunc("GET /zones/zone_1/policies", authorized(listPolicies))
	mux.HandleFunc("POST /zones/zone_1/application-credentials", authorized(createCredential))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func signIn(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != fakeClientID || secret != fakeSecret || r.FormValue("grant_type") != "client_credentials" {
		http.Error(w, `{"message":"bad credentials"}`, http.StatusUnauthorized)
		return
	}
	_, _ = io.WriteString(w, `{"access_token":"`+fakeToken()+`","expires_in":3600}`)
}

func authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken() {
			http.Error(w, `{"message":"no token"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// listPackages answers two pages, and only for the api-server kind.
func listPackages(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("filters[kind]") != "api-server" {
		_, _ = io.WriteString(w, `{"items":[],"pagination":{}}`)
		return
	}
	if r.URL.Query().Get("after") == "" {
		_, _ = io.WriteString(w, `{"items":[
			{"slug":"github","name":"GitHub API","form":{"fields":[{"name":"api_key","label":"Personal Access Token"}]}},
			{"slug":"gmail","name":"Gmail API","form":{"fields":[{"name":"client_id","label":"Client ID"}]}}
		],"pagination":{"after_cursor":"page2"}}`)
		return
	}
	_, _ = io.WriteString(w, `{"items":[
		{"slug":"linear","name":"Linear API","form":{"fields":[{"name":"api_key","label":"Personal API Key"}]}}
	],"pagination":{"after_cursor":null}}`)
}

func install(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug   string         `json:"slug"`
		Fields map[string]any `json:"fields"`
	}
	if err := json.NewDecoder(r.Body).
		Decode(&body); err != nil || body.Slug != "github" ||
		body.Fields["api_key"] != "pat" {
		http.Error(w, `{"message":"bad install"}`, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, `{"outputs":{"api_url":"https://api.github.com"}}`)
}

// listPolicies answers the substring search with a near miss and an archived
// policy, never the exact live one.
func listPolicies(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, `{"items":[
		{"name":"Sandbox access: alice@acme.example.org"},
		{"name":"Sandbox access: alice@acme.example","archived_at":"2026-01-01T00:00:00Z"}
	],"pagination":{}}`)
}

func createCredential(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body["type"] != "token" || body["application_id"] != "app_1" || body["provider_id"] != "prv_1" {
		http.Error(w, `{"message":"bad credential"}`, http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, `{"id":"cred_1","type":"token","provider_id":"prv_1","subject":"`+body["subject"]+`"}`)
}

func TestKeycardSignsInAndReadsTheOrganization(t *testing.T) {
	t.Parallel()
	server := keycardServer(t)

	org, err := sandbox.NewKeycard(server.URL, fakeClientID, fakeSecret).SignIn(context.Background())
	if err != nil || org != "org_1" {
		t.Errorf("SignIn = %q, %v, want org_1", org, err)
	}
	if _, err := sandbox.NewKeycard(server.URL, fakeClientID, "wrong").SignIn(context.Background()); err == nil {
		t.Error("a wrong secret signed in")
	}
}

func TestKeycardListsOnlyPackagesThatTakeAnAPIKey(t *testing.T) {
	t.Parallel()
	server := keycardServer(t)

	packages, err := sandbox.NewKeycard(server.URL, fakeClientID, fakeSecret).
		APIKeyPackages(context.Background(), testZone)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) != 2 || packages[0].Slug != sandbox.SlugGitHub || packages[1].Slug != sandbox.SlugLinear {
		t.Fatalf("APIKeyPackages = %+v, want github and linear from both pages", packages)
	}
	if field, ok := packages[1].Field("api_key"); !ok || field.Label != "Personal API Key" {
		t.Errorf("linear's key field = %+v", field)
	}
}

func TestKeycardInstallsAPackageBySlug(t *testing.T) {
	t.Parallel()
	server := keycardServer(t)

	out, err := sandbox.NewKeycard(server.URL, fakeClientID, fakeSecret).
		Install(context.Background(), testZone, sandbox.SlugGitHub, map[string]any{"api_key": "pat"})
	if err != nil || out["api_url"] != "https://api.github.com" {
		t.Errorf("Install = %v, %v", out, err)
	}
}

// TestKeycardPolicyExistsMatchesExactly checks the name match: the policy
// search returns substrings, and an archived policy is no longer there.
func TestKeycardPolicyExistsMatchesExactly(t *testing.T) {
	t.Parallel()
	server := keycardServer(t)

	exists, err := sandbox.NewKeycard(server.URL, fakeClientID, fakeSecret).
		PolicyExists(context.Background(), testZone, "Sandbox access: alice@acme.example")
	if err != nil || exists {
		t.Errorf("PolicyExists = %v, %v, want false", exists, err)
	}
}

func TestKeycardCreatesATokenCredential(t *testing.T) {
	t.Parallel()
	server := keycardServer(t)

	cred, err := sandbox.NewKeycard(server.URL, fakeClientID, fakeSecret).
		CreateTokenCredential(context.Background(), testZone, "app_1", "prv_1", bobSubject)
	if err != nil || cred.ID != "cred_1" || cred.Subject != bobSubject {
		t.Errorf("CreateTokenCredential = %+v, %v", cred, err)
	}
}

// newZoneServer answers zone creation the way a new zone bootstraps: the
// zone gets its URL on the first read, and the policy engine answers 404
// until the bootstrap has run, which is never when ready is zero.
func newZoneServer(t *testing.T, ready int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var checks atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /service-account-token", signIn)
	mux.HandleFunc("POST /zones", authorized(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"zone_new","name":"demo"}`)
	}))
	mux.HandleFunc("GET /zones/zone_new", authorized(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(
			w,
			`{"id":"zone_new","name":"demo","protocols":{"oauth2":{"issuer":"https://zone-new.example/"}}}`,
		)
	}))
	mux.HandleFunc("GET /zones/zone_new/policy-bindings", authorized(func(w http.ResponseWriter, _ *http.Request) {
		if count := checks.Add(1); ready == 0 || count < ready {
			http.Error(w, `{"message":"zone not found"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"items":[],"pagination":{}}`)
	}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &checks
}

func TestKeycardCreateZoneWaitsForThePolicyEngine(t *testing.T) {
	t.Parallel()
	server, checks := newZoneServer(t, 3)
	keycard := sandbox.NewKeycard(server.URL, fakeClientID, fakeSecret)
	keycard.SetZonePolling(time.Millisecond, time.Second)

	zone, err := keycard.CreateZone(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if zone.URL() != "https://zone-new.example" || checks.Load() != 3 {
		t.Errorf("zone URL %q after %d checks, want the zone's URL after the third", zone.URL(), checks.Load())
	}
}

func TestKeycardCreateZoneGivesUpOnAZoneThatNeverBootstraps(t *testing.T) {
	t.Parallel()
	server, _ := newZoneServer(t, 0)
	keycard := sandbox.NewKeycard(server.URL, fakeClientID, fakeSecret)
	keycard.SetZonePolling(time.Millisecond, 20*time.Millisecond)

	if _, err := keycard.CreateZone(context.Background(), "demo"); !errors.Is(err, sandbox.ErrZoneNotReady) {
		t.Errorf("err = %v, want ErrZoneNotReady", err)
	}
}
