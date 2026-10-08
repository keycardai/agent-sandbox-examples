package issuer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	// fetchTimeout bounds one HTTP request, redirects included.
	fetchTimeout = 15 * time.Second
	// MaxBody bounds a token, a fetched discovery document and a JWKS.
	MaxBody = 1 << 20
	// maxRedirects matches the net/http default limit.
	maxRedirects = 10
	https        = "https"
)

// Every error from Verify wraps ErrInsecure or one of these.
var (
	ErrCA        = errors.New("ca file refused")
	ErrFetch     = errors.New("discovery refused")
	ErrExpect    = errors.New("expectation refused")
	ErrSubject   = errors.New("subject refused")
	ErrToken     = errors.New("token refused")
	ErrTransport = errors.New("transport unavailable")
)

// Expect is what a token must hold to verify.
type Expect struct {
	// Issuer is the https issuer URL, exactly as the cluster names it.
	Issuer string
	// Audience is the audience the token must name.
	Audience string
	// Subject is the subject the token must name.
	Subject string
	// CAFile is a PEM file whose certificates join the system roots. Empty
	// trusts only the system roots.
	CAFile string
}

// Verify checks token against the keys the issuer publishes. It reads the
// issuer's discovery document and the JWKS that document names over https,
// as an external verifier does.
func Verify(ctx context.Context, want Expect, token string) error {
	if err := checkExpect(want); err != nil {
		return err
	}
	client, err := newClient(want.CAFile)
	if err != nil {
		return err
	}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, want.Issuer)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFetch, err)
	}
	verifier := provider.Verifier(&oidc.Config{
		ClientID: want.Audience,
		// Verify compares iss itself because go-oidc accepts a scheme-less
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
		return fmt.Errorf("%w: %w", ErrToken, err)
	}
	if verified.Issuer != want.Issuer {
		return fmt.Errorf("%w: iss is %q, want %q", ErrToken, verified.Issuer, want.Issuer)
	}
	if verified.Subject != want.Subject {
		return fmt.Errorf("%w: sub is %q, want %q", ErrSubject, verified.Subject, want.Subject)
	}
	return nil
}

// checkExpect refuses a non-https issuer and an empty audience or subject.
// An empty subject would match a token without sub.
func checkExpect(want Expect) error {
	if parsed, err := url.Parse(want.Issuer); err != nil || parsed.Scheme != https {
		return fmt.Errorf("%w: issuer %q", ErrInsecure, want.Issuer)
	}
	if want.Audience == "" {
		return fmt.Errorf("%w: the audience is empty", ErrExpect)
	}
	if want.Subject == "" {
		return fmt.Errorf("%w: the subject is empty", ErrExpect)
	}
	return nil
}

// newClient returns a client that trusts the system roots plus caFile,
// speaks only https, and caps every response body at MaxBody.
func newClient(caFile string) (*http.Client, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("%w: reading the system roots: %w", ErrCA, err)
	}
	if caFile != "" {
		pem, readErr := os.ReadFile(caFile) //nolint:gosec // G304: the path is the CA file the operator named.
		if readErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrCA, readErr)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%w: %s does not hold a PEM certificate", ErrCA, caFile)
		}
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("%w: the default transport is not an *http.Transport", ErrTransport)
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return &http.Client{
		Transport: httpsOnly{base: transport},
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != https {
				return fmt.Errorf("%w: redirect to %s", ErrInsecure, req.URL)
			}
			if len(via) >= maxRedirects {
				return fmt.Errorf("%w: more than %d redirects", ErrFetch, maxRedirects)
			}
			return nil
		},
	}, nil
}

// httpsOnly refuses any request that is not https, including the jwks_uri
// fetch, and caps each response body at MaxBody.
type httpsOnly struct{ base http.RoundTripper }

func (t httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != https {
		return nil, fmt.Errorf("%w: %s", ErrInsecure, req.URL)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", req.URL, err)
	}
	resp.Body = http.MaxBytesReader(nil, resp.Body, MaxBody)
	return resp, nil
}
