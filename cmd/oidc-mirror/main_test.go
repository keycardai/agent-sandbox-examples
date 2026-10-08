package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testIssuer    = "https://oidc.example.com"
	testDiscovery = `{"issuer":"` + testIssuer + `","jwks_uri":"` + testIssuer + `/openid/v1/jwks"}`
	testJWKS      = `{"keys":[{"kty":"RSA","kid":"k","n":"AQAB","e":"AQAB"}]}`
)

func writeDocs(t *testing.T, discovery, jwks string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{discoveryFile: discovery, jwksFile: jwks} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadRefuses(t *testing.T) {
	t.Parallel()
	other := strings.ReplaceAll(testDiscovery, `"jwks_uri":"`+testIssuer, `"jwks_uri":"https://other.example`)
	cases := []struct {
		name, issuer, discovery, jwks string
		want                          error
	}{
		{"http issuer", "http://oidc.example.com", testDiscovery, testJWKS, errInsecure},
		{"missing discovery", testIssuer, "", testJWKS, errFile},
		{"oversized jwks", testIssuer, testDiscovery, strings.Repeat(" ", maxBody+1), errFile},
		{"discovery is an array", testIssuer, "[]", testJWKS, errDiscovery},
		{"discovery is null", testIssuer, "null", testJWKS, errDiscovery},
		{"issuer with trailing slash", testIssuer + "/", testDiscovery, testJWKS, errIssuer},
		{"jwks_uri on another host", testIssuer, other, testJWKS, errJWKSURI},
		{"no keys array", testIssuer, testDiscovery, `{}`, errKeys},
		{"zero keys", testIssuer, testDiscovery, `{"keys":[]}`, errKeys},
		{"key without kty or kid", testIssuer, testDiscovery, `{"keys":[{}]}`, errKeys},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := load(tc.issuer, writeDocs(t, tc.discovery, tc.jwks))
			if !errors.Is(err, tc.want) {
				t.Fatalf("load() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestServe(t *testing.T) {
	t.Parallel()
	const pathIssuer = testIssuer + "/oidc"
	pathDiscovery := strings.ReplaceAll(testDiscovery, testIssuer, pathIssuer)
	cases := map[string]map[string]string{
		testIssuer: {discoveryPath: testDiscovery, jwksPath: testJWKS, "/other": ""},
		pathIssuer: {
			"/oidc" + discoveryPath: pathDiscovery, "/oidc" + jwksPath: testJWKS,
			discoveryPath: "", jwksPath: "",
		},
	}
	for issuer, paths := range cases {
		discovery := testDiscovery
		if issuer == pathIssuer {
			discovery = pathDiscovery
		}
		snap, err := load(issuer, writeDocs(t, discovery, testJWKS))
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler(snap))
		t.Cleanup(server.Close)
		for path, want := range paths {
			status, contentType, body := get(t, server, path)
			if want == "" {
				if status != http.StatusNotFound {
					t.Errorf("%s: GET %s = %d, want 404", issuer, path, status)
				}
				continue
			}
			if body != want || contentType != "application/json" {
				t.Errorf("%s: GET %s = %q (%s), want %q (application/json)", issuer, path, body, contentType, want)
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
