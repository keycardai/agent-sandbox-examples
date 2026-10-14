package sandbox

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/keycardai/agent-sandbox-examples/internal/issuer"
)

const (
	checkAudience = "oidc-check"
	checkSubject  = "system:serviceaccount:default:default"
)

// Mirror reads the cluster's discovery document and JWKS once, then serves
// them over plain HTTP on listen until ctx ends. The tunnel step, or the
// operator's own TLS-terminating proxy, publishes listen at the issuer's
// https URL. Mirror only reads from the cluster.
//
// k3s creates the service-account signing key with the cluster. Restarts and
// `k3s certificate rotate` keep it. Only `k3s certificate rotate-ca` installs
// a new one (https://docs.k3s.io/cli/certificate). Restart Mirror after you
// recreate the cluster or rotate the key.
//
// Empty cluster and issuerURL default to the settings, as Local.cluster and
// Local.issuer describe.
func (l *Local) Mirror(ctx context.Context, cluster, issuerURL, listen string) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return fmt.Errorf("mirror: %w", err)
	}
	return l.mirror(ctx, cluster, issuerURL, listener)
}

// mirror reads the cluster's discovery document and JWKS once, then serves
// them on listener until ctx ends. It closes listener.
func (l *Local) mirror(ctx context.Context, cluster, issuerURL string, listener net.Listener) error {
	defer func() { _ = listener.Close() }()
	cluster, err := l.cluster(cluster)
	if err != nil {
		return err
	}
	issuerURL, err = l.issuer(issuerURL)
	if err != nil {
		return err
	}
	discovery, err := l.kubectl(ctx, cluster, nil, "get", "--raw", "/.well-known/openid-configuration")
	if err != nil {
		return err
	}
	jwks, err := l.kubectl(ctx, cluster, nil, "get", "--raw", "/openid/v1/jwks")
	if err != nil {
		return err
	}
	snap, err := issuer.Load(issuerURL, discovery, jwks)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrIssuer, err)
	}
	if err = issuer.Serve(ctx, snap, listener, l.Out); err != nil {
		return fmt.Errorf("mirror: %w", err)
	}
	return nil
}

// CheckIssuer mints a ServiceAccount token in the cluster and verifies it
// against the issuer's https URL, as the Keycard zone does. It trusts the
// system roots, and the roots in KEYCARD_LOCAL_CA_FILE when that is set.
//
// Empty cluster and issuerURL default to the settings, as Local.cluster and
// Local.issuer describe.
func (l *Local) CheckIssuer(ctx context.Context, cluster, issuerURL string) error {
	cluster, err := l.cluster(cluster)
	if err != nil {
		return err
	}
	issuerURL, err = l.issuer(issuerURL)
	if err != nil {
		return err
	}
	caFile, err := l.caFile()
	if err != nil {
		return err
	}
	token, err := l.kubectl(ctx, cluster, nil, "--namespace", "default",
		"create", "token", "default", "--audience", checkAudience, "--duration", "10m")
	if err != nil {
		return err
	}
	want := issuer.Expect{Issuer: issuerURL, Audience: checkAudience, Subject: checkSubject, CAFile: caFile}
	if err = issuer.Verify(ctx, want, strings.TrimSpace(string(token))); err != nil {
		return fmt.Errorf("%w: %w", ErrIssuer, err)
	}
	_, _ = fmt.Fprintf(l.Out, "a token for %s verifies against %s\n", checkSubject, issuerURL)
	return nil
}
