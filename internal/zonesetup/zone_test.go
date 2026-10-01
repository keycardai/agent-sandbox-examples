package zonesetup_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

type fakeZone struct {
	id, issuer string
}

type fakeAPI struct {
	zones      []fakeZone
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
	mux.HandleFunc("GET /zones", f.authed(f.list))
	mux.HandleFunc("GET /zones/{id}", f.authed(f.get))
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
	i := slices.IndexFunc(f.zones, func(z fakeZone) bool { return z.id == r.PathValue("id") })
	if i < 0 {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, zoneJSON(f.zones[i]))
}

func (f *fakeAPI) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	start := 0
	if after := query.Get("after"); after != "" {
		i := slices.IndexFunc(f.zones, func(z fakeZone) bool { return z.id == after })
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
	end := min(start+size, len(f.zones))
	items := []any{}
	for _, zone := range f.zones[start:end] {
		items = append(items, zoneJSON(zone))
	}
	cursor := ""
	if end < len(f.zones) {
		cursor = f.zones[end-1].id
	}
	writeJSON(w, map[string]any{"items": items, "pagination": map[string]any{"after_cursor": cursor}})
}

const redirectSecret = "redirect-secret"

func refuseLeaks(t *testing.T, err error) {
	t.Helper()
	for _, secret := range []string{fakeSecret, fakeToken, redirectSecret} {
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

func zoneJSON(zone fakeZone) map[string]any {
	return map[string]any{"id": zone.id, "protocols": map[string]any{"oauth2": map[string]any{"issuer": zone.issuer}}}
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func TestResolveZone(t *testing.T) {
	t.Parallel()
	zones := []fakeZone{
		{zoneA, issuerA},
		{zoneB, issuerB},
		{"zone-c", "https://c.keycard.cloud"},
		{zoneD, "https://d.keycard.cloud"},
		{"zone-e", "https://e.keycard.cloud"},
	}
	cases := []struct {
		name       string
		zones      []fakeZone
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
			zones: []fakeZone{{"zone-s", "https://s.keycard.cloud/"}}, zoneURL: "https://s.keycard.cloud", wantZone: "zone-s",
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
			zones:   []fakeZone{{"zone-1", "https://x.keycard.cloud"}, {"zone-2", "https://x.keycard.cloud/"}},
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
				"":  emptyPageToA,
				"A": `{"items":[],"pagination":{"after_cursor":"B"}}`,
				"B": emptyPageToA,
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
			zone, err := zonesetup.ResolveZone(t.Context(), values, &zonesetup.Secrets{ManagementClientSecret: secret})
			refuseLeaks(t, err)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want %v", err, tc.wantErr)
				}
			case err != nil:
				t.Errorf("err = %v", err)
			case zone.ID != tc.wantZone:
				t.Errorf("zone = %s, want %s", zone.ID, tc.wantZone)
			}
			for call, want := range tc.wantCalls {
				if got := fake.count(call); got != want {
					t.Errorf("%s sent %d times, want %d", call, got, want)
				}
			}
		})
	}
}
