package zonesetup_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

// TestCreateZone creates a zone that has an issuer URL only from the second
// read, and whose policy set list answers 404 a set number of times.
func TestCreateZone(t *testing.T) {
	t.Parallel()
	const issuer = "https://zone-new.keycard.cloud"
	tests := []struct {
		name string
		// unknown is how many policy set lists answer 404.
		unknown int32
		want    error
	}{
		{name: "ready after two 404s", unknown: 2},
		{name: "never ready", unknown: 1 << 30, want: zonesetup.ErrZoneNotReady},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gets, lists atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("POST /service-account-token", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"access_token":"t","token_type":"Bearer"}`))
			})
			mux.HandleFunc("POST /zones", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"id":"zone-new","name":"demo"}`))
			})
			mux.HandleFunc("GET /zones/zone-new", func(w http.ResponseWriter, _ *http.Request) {
				if gets.Add(1) == 1 {
					_, _ = w.Write([]byte(`{"id":"zone-new","name":"demo"}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"zone-new","name":"demo","protocols":{"oauth2":{"issuer":"` + issuer + `"}}}`))
			})
			mux.HandleFunc("GET /zones/zone-new/policy-sets", func(w http.ResponseWriter, _ *http.Request) {
				if lists.Add(1) <= tc.unknown {
					http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(`{"items":[],"pagination":{}}`))
			})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				mux.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)

			org, err := zonesetup.Login(t.Context(), server.URL, "sa-1", "secret")
			if err != nil {
				t.Fatal(err)
			}
			zone, err := org.CreateZone(t.Context(), "demo", time.Millisecond, 50*time.Millisecond)
			if !errors.Is(err, tc.want) {
				t.Fatalf("CreateZone = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				return
			}
			if zone.Protocols.Oauth2.Issuer != issuer || lists.Load() != 3 {
				t.Fatalf("zone issuer %q after %d lists, want %q after 3", zone.Protocols.Oauth2.Issuer, lists.Load(), issuer)
			}
		})
	}
}
