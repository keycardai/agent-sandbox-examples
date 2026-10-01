package zonesetup_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

const (
	fakeClientID = "sa-1"
	fakeSecret   = "sa-secret"
	fakeToken    = "sa-token"
	fakeMaxPage  = 2

	issuerA      = "https://a.keycard.cloud"
	issuerB      = "https://b.keycard.cloud"
	exchangeCall = "POST /service-account-token"
	listCall     = "GET /zones"
	getACall     = "GET /zones/zone-a"
	tokenPath    = "/service-account-token"
	listPath     = "/zones"
	getAPath     = "/zones/zone-a"
	emptyPageToA = `{"items":[],"pagination":{"after_cursor":"A"}}`
	zoneA        = "zone-a"
	zoneB        = "zone-b"
	zoneD        = "zone-d"
)

type fakeItem struct {
	id, issuer string
}

type fakeAPI struct {
	zones      []fakeItem
	providers  map[string][]fakeItem
	posts      [][]byte
	failPath   string
	failStatus int
	handlers   map[string]http.HandlerFunc

	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeAPI) serve(t *testing.T) string {
	t.Helper()
	f.calls = map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /service-account-token", f.token)
	mux.HandleFunc("GET /zones", f.authed(func(w http.ResponseWriter, r *http.Request) { writePage(w, r, f.zones) }))
	mux.HandleFunc("GET /zones/{id}", f.authed(f.get))
	mux.HandleFunc("GET /zones/{id}/providers", f.authed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writePage(w, r, f.providers[r.PathValue("id")])
	}))
	mux.HandleFunc("POST /zones/{id}/providers", f.authed(f.createProvider))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls[r.Method+" "+r.URL.Path]++
		f.mu.Unlock()
		if r.URL.Path == f.failPath {
			w.WriteHeader(f.failStatus)
			return
		}
		if h, ok := f.handlers[r.URL.Path]; ok {
			h(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func (f *fakeAPI) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[call]
}

func (f *fakeAPI) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	if !ok || id != fakeClientID || secret != fakeSecret || r.PostFormValue("grant_type") != "client_credentials" {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	writeJSON(w, map[string]any{"access_token": fakeToken, "token_type": "Bearer", "expires_in": 3600})
}

func (f *fakeAPI) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (f *fakeAPI) get(w http.ResponseWriter, r *http.Request) {
	i := slices.IndexFunc(f.zones, func(z fakeItem) bool { return z.id == r.PathValue("id") })
	if i < 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, itemJSON(f.zones[i]))
}

func (f *fakeAPI) createProvider(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Protocols struct {
			Oauth2 struct {
				Issuer string `json:"issuer"`
			} `json:"oauth2"`
		} `json:"protocols"`
	}
	raw, err := io.ReadAll(r.Body)
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	if err != nil {
		http.Error(w, `{"error":"bad body"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, raw)
	provider := fakeItem{"provider-new-" + strconv.Itoa(len(f.posts)), body.Protocols.Oauth2.Issuer}
	zone := r.PathValue("id")
	f.providers[zone] = append(f.providers[zone], provider)
	writeJSON(w, itemJSON(provider))
}

func writePage(w http.ResponseWriter, r *http.Request, all []fakeItem) {
	query := r.URL.Query()
	start := 0
	if after := query.Get("after"); after != "" {
		i := slices.IndexFunc(all, func(z fakeItem) bool { return z.id == after })
		if i < 0 {
			http.Error(w, `{"error":"bad cursor"}`, http.StatusBadRequest)
			return
		}
		start = i + 1
	}
	size := fakeMaxPage
	if limit, err := strconv.Atoi(query.Get("limit")); err == nil && limit < size {
		size = limit
	}
	end := min(start+size, len(all))
	items := []any{}
	for _, item := range all[start:end] {
		items = append(items, itemJSON(item))
	}
	cursor := ""
	if end < len(all) {
		cursor = all[end-1].id
	}
	writeJSON(w, map[string]any{"items": items, "pagination": map[string]any{"after_cursor": cursor}})
}

const (
	cursorSecret   = "cursor-secret"
	redirectSecret = "redirect-secret"
)

func refuseLeaks(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{fakeSecret, fakeToken, redirectSecret, cursorSecret} {
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("err = %v, which holds %q", err, secret)
		}
	}
}

func redirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/redirected?key="+redirectSecret, http.StatusTemporaryRedirect)
}

func badRedirect(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Location", "/redirected?key="+redirectSecret+"%zz")
	w.WriteHeader(http.StatusTemporaryRedirect)
}

func echoToken(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, `{"error":"`+fakeToken+`"}`, http.StatusInternalServerError)
}

func rawBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func pages(byCursor map[string]string) http.HandlerFunc {
	var served atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if served.Add(1) > 10 {
			http.Error(w, `{"error":"too many pages"}`, http.StatusTooManyRequests)
			return
		}
		rawBody(byCursor[r.URL.Query().Get("after")])(w, r)
	}
}

func itemJSON(item fakeItem) map[string]any {
	return map[string]any{"id": item.id, "protocols": map[string]any{"oauth2": map[string]any{"issuer": item.issuer}}}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func TestConnect(t *testing.T) {
	t.Parallel()
	zones := []fakeItem{
		{zoneA, issuerA},
		{zoneB, issuerB},
		{"zone-c", "https://c.keycard.cloud"},
		{zoneD, "https://d.keycard.cloud"},
		{"zone-e", "https://e.keycard.cloud"},
	}
	cases := []struct {
		name       string
		zones      []fakeItem
		zoneURL    string
		zoneID     string
		secret     string
		failPath   string
		failStatus int
		handlers   map[string]http.HandlerFunc
		wantZone   string
		wantErr    error
		wantCalls  map[string]int
	}{
		{
			name:  "a trailing slash on the issuer still matches",
			zones: []fakeItem{{"zone-s", "https://s.keycard.cloud/"}}, zoneURL: "https://s.keycard.cloud", wantZone: "zone-s",
		},
		{name: "a trailing slash on zone_url still matches", zoneURL: "https://c.keycard.cloud/", wantZone: "zone-c"},
		{name: "by id", zoneID: zoneD, wantZone: zoneD},
		{name: "by id and url that match", zoneID: zoneB, zoneURL: issuerB, wantZone: zoneB},
		{
			name: "by url", zoneURL: issuerA, wantZone: zoneA,
			wantCalls: map[string]int{exchangeCall: 1, listCall: 3},
		},
		{name: "by url on the last page", zoneURL: "https://e.keycard.cloud", wantZone: "zone-e"},
		{
			name: "id and url that disagree", zoneID: zoneB, zoneURL: issuerA,
			wantErr: zonesetup.ErrZoneMismatch,
		},
		{
			name:    "two zones issue from zone_url",
			zones:   []fakeItem{{"zone-1", "https://x.keycard.cloud"}, {"zone-2", "https://x.keycard.cloud/"}},
			zoneURL: "https://x.keycard.cloud", wantErr: zonesetup.ErrZoneAmbiguous,
		},
		{name: "unknown zone id", zoneID: "zone-z", wantErr: zonesetup.ErrZoneNotFound},
		{name: "unknown zone url", zoneURL: "https://nope.keycard.cloud", wantErr: zonesetup.ErrZoneNotFound},
		{
			name: "a failed list request is sent once", zoneURL: issuerA,
			failPath: listPath, failStatus: http.StatusServiceUnavailable, wantErr: zonesetup.ErrAPI,
			wantCalls: map[string]int{listCall: 1},
		},
		{
			name: "a failed token request is sent once", zoneID: zoneA,
			failPath: tokenPath, failStatus: http.StatusBadGateway, wantErr: zonesetup.ErrAuth,
			wantCalls: map[string]int{exchangeCall: 1},
		},
		{
			name: "a failed zone request is sent once", zoneID: zoneA,
			failPath: getAPath, failStatus: http.StatusBadGateway, wantErr: zonesetup.ErrAPI,
			wantCalls: map[string]int{getACall: 1},
		},
		{
			name: "wrong secret", zoneID: zoneA, secret: "wrong", wantErr: zonesetup.ErrAuth,
			wantCalls: map[string]int{exchangeCall: 1, getACall: 0},
		},
		{
			name: "a malformed redirect on the token request is refused", zoneID: zoneA,
			handlers: map[string]http.HandlerFunc{tokenPath: badRedirect}, wantErr: zonesetup.ErrAuth,
		},
		{
			name: "a malformed redirect on the zone request is refused", zoneID: zoneA,
			handlers: map[string]http.HandlerFunc{getAPath: badRedirect}, wantErr: zonesetup.ErrAPI,
		},
		{
			name: "a redirected token request is refused", zoneID: zoneA,
			handlers: map[string]http.HandlerFunc{tokenPath: redirect}, wantErr: zonesetup.ErrAuth,
			wantCalls: map[string]int{exchangeCall: 1, "POST /redirected": 0},
		},
		{
			name: "a redirected zone request is refused", zoneID: zoneA,
			handlers: map[string]http.HandlerFunc{getAPath: redirect}, wantErr: zonesetup.ErrAPI,
			wantCalls: map[string]int{getACall: 1, "GET /redirected": 0},
		},
		{
			name: "a list error body that repeats the token", zoneURL: issuerA,
			handlers: map[string]http.HandlerFunc{listPath: echoToken}, wantErr: zonesetup.ErrAPI,
		},
		{
			name: "a zone error body that repeats the token", zoneID: zoneA,
			handlers: map[string]http.HandlerFunc{getAPath: echoToken}, wantErr: zonesetup.ErrAPI,
		},
		{
			name: "a null zone", zoneID: zoneA, wantErr: zonesetup.ErrAPI,
			handlers: map[string]http.HandlerFunc{getAPath: rawBody("null")},
		},
		{
			name: "a null zone page", zoneURL: issuerA, wantErr: zonesetup.ErrAPI,
			handlers: map[string]http.HandlerFunc{listPath: rawBody("null")},
		},
		{
			name: "a lowercase bearer token_type is accepted", zoneID: zoneA, wantZone: zoneA,
			handlers: map[string]http.HandlerFunc{
				tokenPath: rawBody(`{"access_token":"sa-token","token_type":"bearer"}`),
			},
		},
		{
			name: "a token response with a second object past the size limit", zoneID: zoneA, wantErr: zonesetup.ErrAuth,
			handlers: map[string]http.HandlerFunc{tokenPath: rawBody(
				`{"access_token":"sa-token","token_type":"Bearer"}` + strings.Repeat(" ", 1<<20) + `{"x":1}`,
			)},
		},
		{
			name: "a token response with another token_type", zoneID: zoneA, wantErr: zonesetup.ErrAuth,
			handlers: map[string]http.HandlerFunc{
				tokenPath: rawBody(`{"access_token":"sa-token","token_type":"mac"}`),
			},
		},
		{
			name: "a token response with trailing bytes", zoneID: zoneA, wantErr: zonesetup.ErrAuth,
			handlers: map[string]http.HandlerFunc{
				tokenPath: rawBody(`{"access_token":"sa-token","token_type":"bearer"} x`),
			},
		},
		{
			name: "a token response without token_type", zoneID: zoneA, wantErr: zonesetup.ErrAuth,
			handlers: map[string]http.HandlerFunc{tokenPath: rawBody(`{"access_token":"sa-token"}`)},
		},
		{
			name: "a token_type that repeats the secret", zoneID: zoneA, wantErr: zonesetup.ErrAuth,
			handlers: map[string]http.HandlerFunc{
				tokenPath: rawBody(`{"access_token":"sa-token","token_type":"` + fakeSecret + `"}`),
			},
		},
		{
			name: "a cursor cycle is refused", zoneURL: issuerA, wantErr: zonesetup.ErrAPI,
			handlers: map[string]http.HandlerFunc{listPath: pages(map[string]string{
				"":           `{"items":[],"pagination":{"after_cursor":"` + cursorSecret + `"}}`,
				cursorSecret: emptyPageToA,
				"A":          `{"items":[],"pagination":{"after_cursor":"` + cursorSecret + `"}}`,
			})},
			wantCalls: map[string]int{listCall: 3},
		},
		{
			name: "a zone after an empty page", zoneURL: issuerA, wantZone: zoneA,
			handlers: map[string]http.HandlerFunc{listPath: pages(map[string]string{
				"": emptyPageToA,
				"A": `{"items":[{"id":"zone-a","protocols":{"oauth2":{"issuer":"` + issuerA +
					`"}}}],"pagination":{"after_cursor":""}}`,
			})},
		},
		{
			name: "zone_id returns an error body with status 200", zoneID: zoneA, wantErr: zonesetup.ErrAPI,
			handlers: map[string]http.HandlerFunc{getAPath: rawBody(`{"error":"server failed"}`)},
		},
		{
			name: "zone_id returns another zone", zoneID: zoneA, wantErr: zonesetup.ErrAPI,
			handlers: map[string]http.HandlerFunc{getAPath: rawBody(`{"id":"zone-b"}`)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{zones: zones, failPath: tc.failPath, failStatus: tc.failStatus, handlers: tc.handlers}
			if tc.zones != nil {
				fake.zones = tc.zones
			}
			values := &zonesetup.Values{
				ManagementAPIURL:         fake.serve(t),
				ZoneURL:                  tc.zoneURL,
				ZoneID:                   tc.zoneID,
				ManagementServiceAccount: zonesetup.ManagementServiceAccountValues{ClientID: fakeClientID},
			}
			secret := fakeSecret
			if tc.secret != "" {
				secret = tc.secret
			}
			session, err := zonesetup.Connect(t.Context(), values, &zonesetup.Secrets{ManagementClientSecret: secret})
			refuseLeaks(t, err)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want %v", err, tc.wantErr)
				}
			case err != nil:
				t.Errorf("err = %v", err)
			case session.Zone.ID != tc.wantZone:
				t.Errorf("zone = %s, want %s", session.Zone.ID, tc.wantZone)
			}
			for call, want := range tc.wantCalls {
				if got := fake.count(call); got != want {
					t.Errorf("%s sent %d times, want %d", call, got, want)
				}
			}
		})
	}
}

const postProviderCall = "POST /zones/zone-a/providers"

func createProvider(t *testing.T, session *zonesetup.Session, fake *fakeAPI, issuer string) string {
	t.Helper()
	providerID, err := session.CreateProvider(t.Context(), issuer)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if got := fake.count(postProviderCall); got != 1 {
		t.Fatalf("CreateProvider sent %d POSTs, want 1", got)
	}
	want := map[string]any{
		"identifier":  issuer,
		"name":        "Kubernetes cluster oidc.example.com",
		"description": "OIDC issuer of the Kubernetes cluster that runs the sandboxes",
		"protocols":   map[string]any{"oauth2": map[string]any{"issuer": issuer}},
	}
	var got map[string]any
	if err := json.Unmarshal(fake.posts[0], &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("created provider %s, want %v", fake.posts[0], want)
	}
	return providerID
}

func TestProvider(t *testing.T) {
	t.Parallel()
	const (
		cluster   = "https://oidc.example.com/id/X"
		providerK = "provider-k"
		created1  = "provider-new-1"
	)
	other := []fakeItem{{"provider-1", "https://github.com"}, {"provider-2", "https://other.example.com"}}
	cases := []struct {
		name      string
		providers []fakeItem
		handlers  map[string]http.HandlerFunc
		create    bool
		wantID    string
		wantErr   error
	}{
		{
			name: "one match on a later page", providers: append(other[:2:2], fakeItem{providerK, cluster}),
			wantID: providerK,
		},
		{name: "FindProvider with none returns no id", providers: other},
		{name: "none is created", providers: other, create: true, wantID: created1},
		{
			name:      "two matches are refused",
			providers: []fakeItem{{providerK, cluster}, {"provider-l", cluster}}, wantErr: zonesetup.ErrProviderAmbiguous,
		},
		{
			name: "a provider page of {} is refused", wantErr: zonesetup.ErrAPI,
			handlers: map[string]http.HandlerFunc{"/zones/zone-a/providers": rawBody("{}")},
		},
		{
			name: "a match without an id is refused", providers: []fakeItem{{"", cluster}},
			wantErr: zonesetup.ErrAPI,
		},
		{
			name: "a trailing slash is another issuer", providers: []fakeItem{{"provider-s", cluster + "/"}},
			create: true, wantID: created1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeAPI{
				zones: []fakeItem{{zoneA, issuerA}}, providers: map[string][]fakeItem{zoneA: tc.providers},
				handlers: tc.handlers,
			}
			values := &zonesetup.Values{
				ManagementAPIURL:         fake.serve(t),
				ZoneID:                   zoneA,
				ManagementServiceAccount: zonesetup.ManagementServiceAccountValues{ClientID: fakeClientID},
			}
			session, err := zonesetup.Connect(t.Context(), values, &zonesetup.Secrets{ManagementClientSecret: fakeSecret})
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			providerID, err := session.FindProvider(t.Context(), cluster)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := fake.count(postProviderCall); got != 0 {
				t.Fatalf("FindProvider sent %d POSTs", got)
			}
			if tc.create {
				if providerID != "" {
					t.Fatalf("FindProvider = %q, want no id", providerID)
				}
				providerID = createProvider(t, session, fake, cluster)
			}
			if providerID != tc.wantID {
				t.Errorf("id = %q, want %q", providerID, tc.wantID)
			}
		})
	}
}
