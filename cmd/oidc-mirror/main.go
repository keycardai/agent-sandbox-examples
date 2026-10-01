// Command oidc-mirror serves a snapshot of a cluster's ServiceAccount issuer
// documents over plain HTTP, for a TLS-terminating gateway to publish at the
// issuer URL. It serves the discovery document and the JWKS byte for byte as
// read from --dir, and refuses to start unless they name --issuer.
//
//	oidc-mirror --issuer "$ISSUER" --dir local/out/oidc
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
)

const (
	discoveryFile     = "openid-configuration.json"
	discoveryPath     = "/.well-known/openid-configuration"
	jwksFile          = "jwks.json"
	jwksPath          = "/openid/v1/jwks"
	maxBody           = 1 << 20
	readHeaderTimeout = 5 * time.Second
	shutdownTimeout   = 5 * time.Second
)

var (
	errDiscovery = errors.New("discovery document is not a JSON object")
	errFile      = errors.New("document file refused")
	errInsecure  = errors.New("non-https issuer refused")
	errIssuer    = errors.New("discovery issuer differs from --issuer")
	errJWKSURI   = errors.New("discovery jwks_uri differs from <issuer>/openid/v1/jwks")
	errKeys      = errors.New("jwks holds no usable keys")
)

type snapshot struct {
	docs map[string][]byte
	keys int
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newCommand().Run(ctx, os.Args)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "oidc-mirror: %v\n", err)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:            "oidc-mirror",
		Usage:           "serve a snapshot of a cluster's issuer discovery document and JWKS",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "issuer", Required: true, Usage: "the https issuer URL, exactly as the cluster names it"},
			&cli.StringFlag{
				Name: "dir", Required: true,
				Usage: "the directory that holds " + discoveryFile + " and " + jwksFile,
			},
			&cli.StringFlag{Name: "listen", Value: "127.0.0.1:8480", Usage: "the address to serve plain HTTP on"},
		},
		Action: serve,
	}
}

func serve(ctx context.Context, cmd *cli.Command) error {
	issuer := cmd.String("issuer")
	snap, err := load(issuer, cmd.String("dir"))
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cmd.String("listen"))
	if err != nil {
		return fmt.Errorf("listening: %w", err)
	}
	server := &http.Server{Handler: handler(snap), ReadHeaderTimeout: readHeaderTimeout}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	_, _ = fmt.Fprintf(
		cmd.ErrWriter,
		"oidc-mirror: serving %s on http://%s with %d keys\n",
		issuer,
		listener.Addr(),
		snap.keys,
	)

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

func load(issuer, dir string) (*snapshot, error) {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: %q", errInsecure, issuer)
	}
	discovery, err := readFile(filepath.Join(dir, discoveryFile))
	if err != nil {
		return nil, err
	}
	jwks, err := readFile(filepath.Join(dir, jwksFile))
	if err != nil {
		return nil, err
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
	return &snapshot{docs: docs, keys: keys}, nil
}

func checkDiscovery(issuer string, discovery []byte) error {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(discovery, &doc); err != nil || doc == nil {
		return fmt.Errorf("%w: %s", errDiscovery, discoveryFile)
	}
	if got := stringField(doc, "issuer"); got != issuer {
		return fmt.Errorf("%w: issuer is %q, want %q", errIssuer, got, issuer)
	}
	if got, want := stringField(doc, "jwks_uri"), issuer+jwksPath; got != want {
		return fmt.Errorf("%w: jwks_uri is %q, want %q", errJWKSURI, got, want)
	}
	return nil
}

// kube-apiserver always sets kid, which RFC 7517 makes optional.
func checkKeys(jwks []byte) (int, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &set); err != nil || len(set.Keys) == 0 {
		return 0, fmt.Errorf("%w: %s holds no keys array or zero keys", errKeys, jwksFile)
	}
	for i, raw := range set.Keys {
		var key map[string]json.RawMessage
		if err := json.Unmarshal(raw, &key); err != nil || stringField(key, "kty") == "" || stringField(key, "kid") == "" {
			return 0, fmt.Errorf("%w: key %d in %s is not an object with a kty and a kid", errKeys, i, jwksFile)
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

func readFile(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the path is under the directory the operator named.
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errFile, err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s: %w", errFile, path, err)
	}
	if len(raw) > maxBody {
		return nil, fmt.Errorf("%w: %s holds more than %d bytes", errFile, path, maxBody)
	}
	return raw, nil
}

func handler(snap *snapshot) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := snap.docs[r.URL.Path]
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
	})
}
