// Package issuer publishes and verifies a cluster's ServiceAccount issuer:
// its OIDC discovery document and the JWKS that document names.
package issuer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

const (
	discoveryPath     = "/.well-known/openid-configuration"
	jwksPath          = "/openid/v1/jwks"
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// Every error from Load wraps one of these.
var (
	ErrDiscovery = errors.New("discovery document is not a JSON object")
	ErrInsecure  = errors.New("non-https URL refused")
	ErrIssuer    = errors.New("discovery issuer differs from the issuer URL")
	ErrJWKSURI   = errors.New("discovery jwks_uri differs from <issuer>/openid/v1/jwks")
	ErrKeys      = errors.New("jwks has no usable key")
)

// Snapshot holds an issuer's discovery document and JWKS, checked against
// the issuer URL. It serves them byte for byte.
type Snapshot struct {
	issuer string
	docs   map[string][]byte
	keys   int
}

// Load checks that discovery names issuer and its JWKS path, and that jwks
// holds at least one key with a kty and a kid. issuer must be https.
func Load(issuer string, discovery, jwks []byte) (*Snapshot, error) {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: %q", ErrInsecure, issuer)
	}
	if err = checkDiscovery(issuer, discovery); err != nil {
		return nil, err
	}
	keys, err := checkKeys(jwks)
	if err != nil {
		return nil, err
	}
	// OIDC Discovery 1.0 §4 appends the well-known path to the issuer, path included.
	docs := map[string][]byte{parsed.Path + discoveryPath: discovery, parsed.Path + jwksPath: jwks}
	return &Snapshot{issuer: issuer, docs: docs, keys: keys}, nil
}

// Serve serves snap over plain HTTP on listen until ctx ends, for a
// TLS-terminating proxy to publish at the issuer URL. It writes one line to
// log once it listens.
func Serve(ctx context.Context, snap *Snapshot, listen string, log io.Writer) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}
	server := &http.Server{Handler: snap, ReadHeaderTimeout: readHeaderTimeout}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	_, _ = fmt.Fprintf(log, "serving %s on http://%s with %d keys\n", snap.issuer, listener.Addr(), snap.keys)

	select {
	case err = <-served:
		return fmt.Errorf("serving: %w", err)
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err = server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

// ServeHTTP answers GET and HEAD for the two documents, and 404 for any
// other path.
func (s *Snapshot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, ok := s.docs[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func checkDiscovery(issuer string, discovery []byte) error {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(discovery, &doc); err != nil || doc == nil {
		return ErrDiscovery
	}
	if got := stringField(doc, "issuer"); got != issuer {
		return fmt.Errorf("%w: issuer is %q, want %q", ErrIssuer, got, issuer)
	}
	if got, want := stringField(doc, "jwks_uri"), issuer+jwksPath; got != want {
		return fmt.Errorf("%w: jwks_uri is %q, want %q", ErrJWKSURI, got, want)
	}
	return nil
}

// kube-apiserver always sets kid, which RFC 7517 makes optional.
func checkKeys(jwks []byte) (int, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &set); err != nil || len(set.Keys) == 0 {
		return 0, fmt.Errorf("%w: no keys array or zero keys", ErrKeys)
	}
	for i, raw := range set.Keys {
		var key map[string]json.RawMessage
		if err := json.Unmarshal(raw, &key); err != nil || stringField(key, "kty") == "" || stringField(key, "kid") == "" {
			return 0, fmt.Errorf("%w: key %d is not an object with a kty and a kid", ErrKeys, i)
		}
	}
	return len(set.Keys), nil
}

func stringField(doc map[string]json.RawMessage, name string) string {
	var value string
	if err := json.Unmarshal(doc[name], &value); err != nil {
		return ""
	}
	return value
}
