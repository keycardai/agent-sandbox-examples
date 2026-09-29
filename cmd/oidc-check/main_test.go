package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testAudience = "https://zone.example"
	testKeyID    = "cluster-key"
	testSubject  = "system:serviceaccount:agents:agent"
)

// issuerCase describes one issuer the test serves and the token it checks.
type issuerCase struct {
	name string
	// discoveredIssuer overrides the issuer the discovery document names.
	discoveredIssuer string
	// jwksPath is where the issuer publishes its keys.
	jwksPath string
	// redirect sends discovery to a plain http URL.
	redirect bool
	// httpIssuer passes the issuer to the command with an http scheme.
	httpIssuer bool
	// httpJWKS publishes a jwks_uri with an http scheme.
	httpJWKS bool
	// emptySubject passes an empty --subject.
	emptySubject bool
	// padDiscovery grows the discovery document past maxBody.
	padDiscovery bool
	signer       *rsa.PrivateKey
	claims       func(issuer string, now time.Time) map[string]any
	want         error
	wantText     string
}

func TestCheck(t *testing.T) {
	t.Parallel()
	cluster := newKey(t)
	other := newKey(t)
	cases := []issuerCase{
		{name: "valid token", signer: cluster, claims: validClaims},
		{name: "jwks_uri at /keys", jwksPath: "/keys", signer: cluster, claims: validClaims},
		{
			name: "signed by another key", signer: other, claims: validClaims,
			want: errToken, wantText: "failed to verify signature",
		},
		{
			name: "issuer mismatch", discoveredIssuer: "https://other.example", signer: cluster, claims: validClaims,
			want: errDiscovery, wantText: "did not match the issuer URL returned by provider",
		},
		{
			name: "iss differs from --issuer", signer: cluster, want: errToken, wantText: "iss is",
			claims: func(issuer string, now time.Time) map[string]any {
				return validClaims(issuer+"/", now)
			},
		},
		{
			name: "another audience", signer: cluster, want: errToken, wantText: "expected audience",
			claims: func(issuer string, now time.Time) map[string]any {
				claims := validClaims(issuer, now)
				claims["aud"] = []string{"https://other.example"}
				return claims
			},
		},
		{
			name: "oversized discovery", padDiscovery: true, signer: cluster, claims: validClaims,
			want: errDiscovery, wantText: "request body too large",
		},
		{
			name: "missing exp", signer: cluster, want: errToken, wantText: "token is expired",
			claims: func(issuer string, now time.Time) map[string]any {
				claims := validClaims(issuer, now)
				delete(claims, "exp")
				return claims
			},
		},
		{
			name: "future nbf", signer: cluster, want: errToken, wantText: "before the nbf",
			claims: func(issuer string, now time.Time) map[string]any {
				claims := validClaims(issuer, now)
				claims["nbf"] = now.Add(time.Hour).Unix()
				return claims
			},
		},
		{
			name: "redirect to http", redirect: true, signer: cluster, claims: validClaims,
			want: errInsecure, wantText: "redirect to http://",
		},
		{
			name: "http issuer", httpIssuer: true, signer: cluster, claims: validClaims,
			want: errInsecure, wantText: `issuer "http://`,
		},
		{
			name: "http jwks_uri", httpJWKS: true, signer: cluster, claims: validClaims,
			want: errToken, wantText: "non-https URL refused",
		},
		{
			name: "empty subject", emptySubject: true, signer: cluster, want: errFlag, wantText: "--subject is empty",
			claims: func(issuer string, now time.Time) map[string]any {
				claims := validClaims(issuer, now)
				delete(claims, "sub")
				return claims
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := runCase(t, cluster, tc)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				if !strings.Contains(out, "token for "+testSubject+" verifies") {
					t.Fatalf("stdout = %q, want the success line", out)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err = %v, want one naming %q", err, tc.wantText)
			}
		})
	}
}

// runCase serves an issuer over TLS whose JWKS holds published, and runs
// the command against it with the server's certificate as --ca-file.
func runCase(t *testing.T, published *rsa.PrivateKey, tc issuerCase) (string, error) {
	t.Helper()
	jwksPath := tc.jwksPath
	if jwksPath == "" {
		jwksPath = "/openid/v1/jwks"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if tc.redirect {
			http.Redirect(w, r, "http://issuer.example/.well-known/openid-configuration", http.StatusFound)
			return
		}
		issuer := "https://" + r.Host
		if tc.discoveredIssuer != "" {
			issuer = tc.discoveredIssuer
		}
		scheme := "https://"
		if tc.httpJWKS {
			scheme = "http://"
		}
		doc := map[string]string{"issuer": issuer, "jwks_uri": scheme + r.Host + jwksPath}
		if tc.padDiscovery {
			doc["padding"] = strings.Repeat("a", maxBody)
		}
		writeJSON(t, w, doc)
	})
	mux.HandleFunc(jwksPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": testKeyID,
			"n": b64(published.N.Bytes()),
			"e": b64(big.NewInt(int64(published.E)).Bytes()),
		}}})
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	tokenFile := filepath.Join(dir, "token")
	token := sign(t, tc.signer, tc.claims(server.URL, time.Now()))
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	issuer := server.URL
	if tc.httpIssuer {
		issuer = "http://" + strings.TrimPrefix(issuer, "https://")
	}
	subject := testSubject
	if tc.emptySubject {
		subject = ""
	}
	var out bytes.Buffer
	cmd := newCommand()
	cmd.Writer = &out
	err := cmd.Run(t.Context(), []string{
		"oidc-check", "--issuer", issuer, "--token-file", tokenFile, "--ca-file", caFile,
		"--audience", testAudience, "--subject", subject,
	})
	return out.String(), err
}

// A token larger than the cap is refused, from stdin or from a file, so
// nothing past the cap is silently dropped.
func TestOversizedToken(t *testing.T) {
	t.Parallel()
	oversized := bytes.Repeat([]byte("a"), maxBody+1)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"-", tokenFile} {
		cmd := newCommand()
		cmd.Reader = bytes.NewReader(oversized)
		err := cmd.Run(t.Context(), []string{
			"oidc-check", "--issuer", "https://issuer.example", "--token-file", path,
			"--audience", testAudience, "--subject", testSubject,
		})
		if !errors.Is(err, errToken) || !strings.Contains(err.Error(), "holds more than") {
			t.Fatalf("--token-file %s: err = %v, want errToken naming the size cap", path, err)
		}
	}
}

func validClaims(issuer string, now time.Time) map[string]any {
	return map[string]any{
		"iss": issuer,
		"sub": testSubject,
		"aud": []string{testAudience},
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

func newKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// sign returns an RS256 compact JWT over claims, with testKeyID as its kid.
func sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": testKeyID, "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := b64(header) + "." + b64(payload)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + b64(signature)
}

func b64(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Error(err)
	}
}
