// Command oidc-check verifies a ServiceAccount token against the keys its
// cluster's issuer publishes. It reads the issuer's discovery document and
// the JWKS that document names over https, as an external verifier does.
//
//	kubectl -n agents create token <sa> --audience "$AUDIENCE" |
//	  oidc-check --issuer "$ISSUER" --token-file - --audience "$AUDIENCE" \
//	    --subject system:serviceaccount:agents:<sa>
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/urfave/cli/v3"
)

const (
	// fetchTimeout bounds one HTTP request, redirects included.
	fetchTimeout = 15 * time.Second
	// maxBody bounds a token, a fetched discovery document and a JWKS.
	maxBody = 1 << 20
	// maxRedirects matches the net/http default limit.
	maxRedirects = 10
	https        = "https"
)

// The named refusals a check can end in.
var (
	errCA        = errors.New("ca file refused")
	errDiscovery = errors.New("discovery refused")
	errFlag      = errors.New("flag refused")
	errInsecure  = errors.New("non-https URL refused")
	errSubject   = errors.New("subject refused")
	errToken     = errors.New("token refused")
	errTransport = errors.New("transport unavailable")
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newCommand().Run(ctx, os.Args)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "oidc-check: %v\n", err)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:            "oidc-check",
		Usage:           "verify a ServiceAccount token against the keys its cluster's issuer publishes",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "issuer", Required: true, Usage: "the https issuer URL, exactly as the cluster names it"},
			&cli.StringFlag{Name: "token-file", Required: true, Usage: "the token to verify; - reads standard input"},
			&cli.StringFlag{Name: "audience", Required: true, Usage: "the audience the token must carry"},
			&cli.StringFlag{Name: "subject", Required: true, Usage: "the subject the token must carry"},
			&cli.StringFlag{Name: "ca-file", Usage: "a PEM file whose certificates join the system roots"},
		},
		Action: check,
	}
}

func check(ctx context.Context, cmd *cli.Command) error {
	issuer := cmd.String("issuer")
	if err := checkFlags(cmd, issuer); err != nil {
		return err
	}
	token, err := readToken(cmd.Reader, cmd.String("token-file"))
	if err != nil {
		return err
	}
	client, err := newClient(cmd.String("ca-file"))
	if err != nil {
		return err
	}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return fmt.Errorf("%w: %w", errDiscovery, err)
	}
	verifier := provider.Verifier(&oidc.Config{
		ClientID: cmd.String("audience"),
		// check compares iss itself because go-oidc accepts a scheme-less
		// Google issuer.
		SkipIssuerCheck: true,
		SupportedSigningAlgs: []string{
			oidc.RS256, oidc.RS384, oidc.RS512,
			oidc.ES256, oidc.ES384, oidc.ES512,
			oidc.PS256, oidc.PS384, oidc.PS512,
			oidc.EdDSA,
		},
	})
	verified, err := verifier.Verify(ctx, token)
	if err != nil {
		return fmt.Errorf("%w: %w", errToken, err)
	}
	if verified.Issuer != issuer {
		return fmt.Errorf("%w: iss is %q, want %q", errToken, verified.Issuer, issuer)
	}
	if want := cmd.String("subject"); verified.Subject != want {
		return fmt.Errorf("%w: sub is %q, want %q", errSubject, verified.Subject, want)
	}
	_, err = fmt.Fprintf(cmd.Writer, "oidc-check: token for %s verifies against %s\n", verified.Subject, issuer)
	if err != nil {
		return fmt.Errorf("writing the result: %w", err)
	}
	return nil
}

// checkFlags refuses an issuer that is not https and an empty audience or
// subject, which would let a token without that claim pass.
func checkFlags(cmd *cli.Command, issuer string) error {
	if parsed, err := url.Parse(issuer); err != nil || parsed.Scheme != https {
		return fmt.Errorf("%w: issuer %q", errInsecure, issuer)
	}
	for _, name := range []string{"audience", "subject"} {
		if cmd.String(name) == "" {
			return fmt.Errorf("%w: --%s is empty", errFlag, name)
		}
	}
	return nil
}

// readToken reads the token from a file, or from stdin when path is "-",
// and refuses one larger than maxBody.
func readToken(stdin io.Reader, path string) (string, error) {
	src := stdin
	if path != "-" {
		file, err := os.Open(path) //nolint:gosec // G304: the path is the token file the operator named.
		if err != nil {
			return "", fmt.Errorf("%w: reading %s: %w", errToken, path, err)
		}
		defer func() { _ = file.Close() }()
		src = file
	}
	raw, err := io.ReadAll(io.LimitReader(src, maxBody+1))
	if err != nil {
		return "", fmt.Errorf("%w: reading %s: %w", errToken, path, err)
	}
	if len(raw) > maxBody {
		return "", fmt.Errorf("%w: %s holds more than %d bytes", errToken, path, maxBody)
	}
	return strings.TrimSpace(string(raw)), nil
}

// newClient returns a client that trusts the system roots plus caFile,
// speaks only https, and caps every response body at maxBody.
func newClient(caFile string) (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("%w: reading the system roots: %w", errCA, err)
	}
	if caFile != "" {
		pem, readErr := os.ReadFile(caFile) //nolint:gosec // G304: the path is the CA file the operator named.
		if readErr != nil {
			return nil, fmt.Errorf("%w: %w", errCA, readErr)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%w: %s holds no PEM certificate", errCA, caFile)
		}
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("%w: the default transport is not an *http.Transport", errTransport)
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return &http.Client{
		Transport: httpsOnly{base: transport},
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != https {
				return fmt.Errorf("%w: redirect to %s", errInsecure, req.URL)
			}
			if len(via) >= maxRedirects {
				return fmt.Errorf("%w: more than %d redirects", errDiscovery, maxRedirects)
			}
			return nil
		},
	}, nil
}

// httpsOnly refuses any request that is not https, which covers a jwks_uri
// the discovery document names, and caps each response body.
type httpsOnly struct{ base http.RoundTripper }

func (t httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != https {
		return nil, fmt.Errorf("%w: %s", errInsecure, req.URL)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", req.URL, err)
	}
	resp.Body = http.MaxBytesReader(nil, resp.Body, maxBody)
	return resp, nil
}
