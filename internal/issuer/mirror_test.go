package issuer_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/issuer"
)

const (
	wellKnownPath = "/.well-known/openid-configuration"
	keysPath      = "/openid/v1/jwks"
	testIssuer    = "https://oidc.example.com"
	testDiscovery = `{"issuer":"` + testIssuer + `","jwks_uri":"` + testIssuer + `/openid/v1/jwks"}`
	testJWKS      = `{"keys":[{"kty":"RSA","kid":"k","n":"AQAB","e":"AQAB"}]}`
)

func TestLoadRefuses(t *testing.T) {
	t.Parallel()
	other := strings.ReplaceAll(testDiscovery, `"jwks_uri":"`+testIssuer, `"jwks_uri":"https://other.example`)
	cases := []struct {
		name, issuer, discovery, jwks string
		want                          error
	}{
		{"http issuer", "http://oidc.example.com", testDiscovery, testJWKS, issuer.ErrInsecure},
		{"discovery is an array", testIssuer, "[]", testJWKS, issuer.ErrDiscovery},
		{"discovery is null", testIssuer, "null", testJWKS, issuer.ErrDiscovery},
		{"issuer with trailing slash", testIssuer + "/", testDiscovery, testJWKS, issuer.ErrIssuer},
		{"jwks_uri on another host", testIssuer, other, testJWKS, issuer.ErrJWKSURI},
		{"no keys array", testIssuer, testDiscovery, `{}`, issuer.ErrKeys},
		{"zero keys", testIssuer, testDiscovery, `{"keys":[]}`, issuer.ErrKeys},
		{"key without kty or kid", testIssuer, testDiscovery, `{"keys":[{}]}`, issuer.ErrKeys},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := issuer.Load(tc.issuer, []byte(tc.discovery), []byte(tc.jwks))
			if !errors.Is(err, tc.want) {
				t.Fatalf("issuer.Load() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestServe(t *testing.T) {
	t.Parallel()
	const pathIssuer = testIssuer + "/oidc"
	pathDiscovery := strings.ReplaceAll(testDiscovery, testIssuer, pathIssuer)
	cases := map[string]map[string]string{
		testIssuer: {wellKnownPath: testDiscovery, keysPath: testJWKS, "/other": ""},
		pathIssuer: {
			"/oidc" + wellKnownPath: pathDiscovery, "/oidc" + keysPath: testJWKS,
			wellKnownPath: "", keysPath: "",
		},
	}
	for issuerURL, paths := range cases {
		discovery := testDiscovery
		if issuerURL == pathIssuer {
			discovery = pathDiscovery
		}
		snap, err := issuer.Load(issuerURL, []byte(discovery), []byte(testJWKS))
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(snap)
		t.Cleanup(server.Close)
		for path, want := range paths {
			status, contentType, body := get(t, server, path)
			if want == "" {
				if status != http.StatusNotFound {
					t.Errorf("%s: GET %s = %d, want 404", issuerURL, path, status)
				}
				continue
			}
			if body != want || contentType != "application/json" {
				t.Errorf("%s: GET %s = %q (%s), want %q (application/json)", issuerURL, path, body, contentType, want)
			}
		}
	}
}

func get(t *testing.T, server *httptest.Server, path string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}
