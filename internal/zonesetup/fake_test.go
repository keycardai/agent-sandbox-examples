//nolint:goconst,varnamelen // The double repeats API field names and ids so each expectation reads on its own.
package zonesetup_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// zoneAPI is an in-memory double of the management API routes Setup uses. It
// records every request, so a test pins the calls a run makes and does not
// make. Ids are deterministic. The n-th created entity of a kind is
// "<kind>-<n>".
type zoneAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	calls   []call
	counter map[string]int

	zone          map[string]any
	providers     []*record
	applications  []*record
	dependencies  map[string][]string
	resources     []*record
	secrets       []*record
	credentials   []*record
	users         []*record
	schemaVersion string
	policies      []*record
	policyVers    []*record
	sets          []*record
	setVers       []*record
	activeVersion string
}

type record = map[string]any

type call struct {
	method string
	path   string
	body   map[string]any
}

const (
	testZoneID        = "zone-1"
	testZoneURL       = "https://acme.keycard.cloud"
	testIssuer        = "https://oidc.eks.example.com/id/ABC123"
	testSchemaVersion = "2026-03-16"
	testToken         = "tok-1"
	testClientID      = "sa-1"
	testClientSecret  = "sa-secret"
	tokenLifetime     = 3600
	firstVersion      = 1
	applicationName   = "Sandbox egress proxy"

	githubProvider     = "github"
	githubURL          = "https://api.github.com/"
	keyItems           = "items"
	keyOAuth2          = "oauth2"
	keyPagination      = "pagination"
	keyProtocols       = "protocols"
	keyAfterCursor     = "after_cursor"
	keyIssuer          = "issuer"
	openRouterURL      = "https://openrouter.ai/api/v1"
	testResourceSecret = "or-secret"
)

func newZoneAPI(t *testing.T) *zoneAPI {
	t.Helper()
	f := &zoneAPI{
		t:             t,
		counter:       map[string]int{},
		dependencies:  map[string][]string{},
		schemaVersion: testSchemaVersion,
		zone: record{
			"id":         testZoneID,
			"name":       "Acme",
			"slug":       "acme",
			keyProtocols: record{keyOAuth2: record{keyIssuer: testZoneURL + "/"}, "openid": record{}},
		},
	}
	f.providers = append(
		f.providers,
		&record{
			"id":         "prov-vault",
			"identifier": "keycard-vault",
			"name":       "Vault",
			"type":       "keycard-vault",
			keyProtocols: nil,
		},
		&record{
			"id": "prov-github", "identifier": githubProvider, "name": "GitHub", "type": "external",
			keyProtocols: record{keyOAuth2: record{keyIssuer: "https://github.com"}},
		},
	)
	f.users = append(
		f.users,
		&record{"id": "usr-1", "email": "casey@example.com", "identifier": "usr-1", "email_verified": true},
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /service-account-token", f.token)
	f.routeZones(mux)
	f.routeProviders(mux)
	f.routeApplications(mux)
	f.routeResources(mux)
	f.routeSecrets(mux)
	f.routeUserAccess(mux)

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path != "/service-account-token" && r.Header.Get("Authorization") != "Bearer "+testToken {
			f.write(w, http.StatusUnauthorized, record{"error": "bad bearer token"})
			return
		}
		// Reads are recorded here. Writes are recorded by body, which every
		// mutating handler calls, so the record includes the body.
		if r.Method == http.MethodGet {
			f.record(r, nil)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *zoneAPI) routeZones(mux *http.ServeMux) {
	mux.HandleFunc("GET /zones", func(w http.ResponseWriter, _ *http.Request) { f.page(w, []*record{&f.zone}) })
	mux.HandleFunc("GET /zones/{z}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("z") != testZoneID {
			f.write(w, http.StatusNotFound, record{"error": "no such zone"})
			return
		}
		f.write(w, http.StatusOK, f.zone)
	})
}

func (f *zoneAPI) routeProviders(mux *http.ServeMux) {
	mux.HandleFunc("GET /zones/{z}/providers", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.page(w, filterRecords(f.providers, func(p *record) bool {
			return matchQuery(q.Get("identifier"), (*p)["identifier"]) && matchQuery(q.Get("type"), (*p)["type"])
		}))
	})
	mux.HandleFunc("POST /zones/{z}/providers", func(w http.ResponseWriter, r *http.Request) {
		body := f.body(r)
		p := &record{
			"id": f.id("prov"), "identifier": body["identifier"], "name": body["name"], "type": "external",
			keyProtocols: body[keyProtocols],
		}
		f.providers = append(f.providers, p)
		f.write(w, http.StatusCreated, p)
	})
	mux.HandleFunc("PATCH /zones/{z}/providers/{id}", func(w http.ResponseWriter, r *http.Request) {
		p := f.find(f.providers, r.PathValue("id"))
		if p == nil {
			f.write(w, http.StatusNotFound, nil)
			return
		}
		(*p)[keyProtocols] = f.body(r)[keyProtocols]
		f.write(w, http.StatusOK, p)
	})
}

func (f *zoneAPI) routeApplications(mux *http.ServeMux) {
	mux.HandleFunc("GET /zones/{z}/applications", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.page(w, filterRecords(f.applications, func(a *record) bool {
			return matchQuery(q.Get("filter[identifier]"), (*a)["identifier"])
		}))
	})
	mux.HandleFunc("POST /zones/{z}/applications", func(w http.ResponseWriter, r *http.Request) {
		body := f.body(r)
		consent, _ := body["consent"].(string)
		if consent == "" {
			consent = "required"
		}
		a := &record{"id": f.id("app"), "identifier": body["identifier"], "name": body["name"], "consent": consent}
		f.applications = append(f.applications, a)
		f.write(w, http.StatusCreated, a)
	})
	mux.HandleFunc("PATCH /zones/{z}/applications/{id}", func(w http.ResponseWriter, r *http.Request) {
		a := f.find(f.applications, r.PathValue("id"))
		if a == nil {
			f.write(w, http.StatusNotFound, nil)
			return
		}
		if c, ok := f.body(r)["consent"]; ok {
			(*a)["consent"] = c
		}
		f.write(w, http.StatusOK, a)
	})
	mux.HandleFunc("GET /zones/{z}/applications/{id}/dependencies", func(w http.ResponseWriter, r *http.Request) {
		var items []*record
		for _, id := range f.dependencies[r.PathValue("id")] {
			if res := f.find(f.resources, id); res != nil {
				items = append(items, res)
			}
		}
		f.page(w, items)
	})
	mux.HandleFunc("PUT /zones/{z}/applications/{id}/dependencies/{rid}", func(w http.ResponseWriter, r *http.Request) {
		f.body(r)
		app := r.PathValue("id")
		f.dependencies[app] = append(f.dependencies[app], r.PathValue("rid"))
		w.WriteHeader(http.StatusNoContent)
	})
}

func (f *zoneAPI) routeResources(mux *http.ServeMux) {
	mux.HandleFunc("GET /zones/{z}/resources", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.page(w, filterRecords(f.resources, func(x *record) bool {
			return matchQuery(q.Get("filter[identifier]"), (*x)["identifier"])
		}))
	})
	mux.HandleFunc("POST /zones/{z}/resources", func(w http.ResponseWriter, r *http.Request) {
		body := f.body(r)
		x := &record{
			"id": f.id("res"), "identifier": body["identifier"], "name": body["name"],
			"prefix": body["prefix"] == true, "credential_provider_id": body["credential_provider_id"],
		}
		f.resources = append(f.resources, x)
		f.write(w, http.StatusCreated, x)
	})
	mux.HandleFunc("PATCH /zones/{z}/resources/{id}", func(w http.ResponseWriter, r *http.Request) {
		x := f.find(f.resources, r.PathValue("id"))
		if x == nil {
			f.write(w, http.StatusNotFound, nil)
			return
		}
		body := f.body(r)
		if v, ok := body["prefix"]; ok {
			(*x)["prefix"] = v
		}
		if v, ok := body["credential_provider_id"]; ok {
			(*x)["credential_provider_id"] = v
		}
		f.write(w, http.StatusOK, x)
	})
}

// The secrets endpoint answers with a bare array where the rest of the API
// answers with an items wrapper.
func (f *zoneAPI) routeSecrets(mux *http.ServeMux) {
	mux.HandleFunc("GET /zones/{z}/secrets", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		items := filterRecords(f.secrets, func(s *record) bool {
			return matchQuery(q.Get("entity_id"), (*s)["entity_id"]) && matchQuery(q.Get("type"), (*s)["type"])
		})
		if items == nil {
			items = []*record{}
		}
		f.write(w, http.StatusOK, items)
	})
	mux.HandleFunc("POST /zones/{z}/secrets", func(w http.ResponseWriter, r *http.Request) {
		body := f.body(r)
		data, _ := body["data"].(map[string]any)
		s := &record{
			"id": f.id("sec"), "entity_id": body["entity_id"], "name": body["name"], "type": "token",
			"version": firstVersion, "zone_id": testZoneID, "token": data["token"],
		}
		f.secrets = append(f.secrets, s)
		f.write(w, http.StatusCreated, withoutToken(s))
	})
	mux.HandleFunc("PATCH /zones/{z}/secrets/{id}", func(w http.ResponseWriter, r *http.Request) {
		s := f.find(f.secrets, r.PathValue("id"))
		if s == nil {
			f.write(w, http.StatusNotFound, nil)
			return
		}
		data, _ := f.body(r)["data"].(map[string]any)
		(*s)["token"] = data["token"]
		version, _ := (*s)["version"].(int)
		(*s)["version"] = version + 1
		f.write(w, http.StatusOK, withoutToken(s))
	})
}

func (f *zoneAPI) token(w http.ResponseWriter, r *http.Request) {
	f.record(r, nil)
	id, secret, ok := r.BasicAuth()
	if !ok || id != testClientID || secret != testClientSecret {
		f.write(w, http.StatusUnauthorized, record{"error": "invalid_client"})
		return
	}
	f.write(w, http.StatusOK, record{"access_token": testToken, "token_type": "Bearer", "expires_in": tokenLifetime})
}

// body decodes a JSON request body and records the call with it. Every
// mutating handler calls it, so every write is on the record.
func (f *zoneAPI) body(r *http.Request) map[string]any {
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			f.t.Fatalf("%s %s: body is not JSON: %v", r.Method, r.URL.Path, err)
		}
	}
	f.record(r, body)
	return body
}

func (f *zoneAPI) record(r *http.Request, body map[string]any) {
	f.calls = append(f.calls, call{method: r.Method, path: r.URL.Path, body: body})
}

func (f *zoneAPI) id(kind string) string {
	f.counter[kind]++
	return fmt.Sprintf("%s-%d", kind, f.counter[kind])
}

func (f *zoneAPI) find(items []*record, id string) *record {
	for _, it := range items {
		if (*it)["id"] == id {
			return it
		}
	}
	return nil
}

func (f *zoneAPI) page(w http.ResponseWriter, items []*record) {
	if items == nil {
		items = []*record{}
	}
	f.write(w, http.StatusOK, record{
		keyItems:      items,
		"page_info":   record{"has_next_page": false, "has_previous_page": false},
		keyPagination: record{keyAfterCursor: "", "before_cursor": ""},
	})
}

func (f *zoneAPI) write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			f.t.Errorf("encoding response: %v", err)
		}
	}
}

// writes are the recorded mutating calls, "METHOD /path", in order. The
// token request is not a write to the zone and is left out.
func (f *zoneAPI) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.method == http.MethodGet || c.path == "/service-account-token" {
			continue
		}
		out = append(out, c.method+" "+c.path)
	}
	return out
}

// reads are the recorded GET paths, in order.
func (f *zoneAPI) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.method == http.MethodGet {
			out = append(out, c.path)
		}
	}
	return out
}

// bodyOf is the decoded body of the first recorded call matching "METHOD /path".
func (f *zoneAPI) bodyOf(methodPath string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.method+" "+c.path == methodPath {
			return c.body
		}
	}
	f.t.Fatalf("no call %s was made", methodPath)
	return nil
}

// seedComplete puts the zone in the state a completed run leaves it in, for
// the values testValues returns.
func (f *zoneAPI) seedComplete() {
	f.providers = append(f.providers, &record{
		"id": "prov-1", "identifier": testIssuer, "name": "Kubernetes cluster", "type": "external",
		keyProtocols: record{keyOAuth2: record{keyIssuer: testIssuer}},
	})
	f.applications = append(f.applications, &record{
		"id": "app-1", "identifier": "urn:keycard:sandbox-proxy:agents", "name": applicationName, "consent": "implicit",
	})
	f.resources = append(f.resources,
		//nolint:gosec // G101: credential_provider_id holds a provider id, not a credential.
		&record{
			"id": "res-1", "identifier": githubURL, "name": "GitHub API",
			"prefix": true, "credential_provider_id": "prov-github",
		},
		&record{
			"id": "res-2", "identifier": openRouterURL, "name": openRouterURL,
			"prefix": true, "credential_provider_id": "prov-vault",
		},
	)
	f.secrets = append(f.secrets, &record{
		"id": "sec-1", "entity_id": "res-2", "name": openRouterKey, "type": "token",
		"version": firstVersion, "zone_id": testZoneID, "token": "old",
	})
	f.dependencies["app-1"] = []string{"res-1", "res-2"}
	f.seedCompleteUsers()
	f.counter["prov"], f.counter["app"], f.counter["res"], f.counter["sec"] = 1, 1, 2, 1
}

func filterRecords(items []*record, keep func(*record) bool) []*record {
	var out []*record
	for _, it := range items {
		if keep(it) {
			out = append(out, it)
		}
	}
	return out
}

// matchQuery is true when the query parameter is absent or equals the value.
func matchQuery(param string, value any) bool {
	if param == "" {
		return true
	}
	s, _ := value.(string)
	return s == param
}

func withoutToken(s *record) record {
	out := record{}
	for k, v := range *s {
		if k != "token" {
			out[k] = v
		}
	}
	return out
}
