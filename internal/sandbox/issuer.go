package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/keycardai/agent-sandbox-poc/internal/issuer"
)

const (
	checkAudience = "oidc-check"
	checkSubject  = "system:serviceaccount:default:default"
)

// Mirror reads the cluster's issuer discovery document and JWKS, then serves
// them over plain HTTP on listen until ctx ends. The tunnel step, or a
// TLS-terminating proxy of the operator's own, publishes listen at the
// issuer's https URL. Mirror only reads from the cluster.
//
// k3s keeps one signing key for the cluster's life, so the documents stay
// valid until the cluster is recreated. Restart Mirror after you recreate the
// cluster.
//
// Empty cluster and issuerURL default to the settings, as Local.cluster and
// Local.issuer describe.
func (l *Local) Mirror(ctx context.Context, cluster, issuerURL, listen string) error {
	cluster, err := l.k3dName("mirror", cluster)
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
	if err = issuer.Serve(ctx, snap, listen, l.Out); err != nil {
		return fmt.Errorf("mirror: %w", err)
	}
	return nil
}

// clusterIssuer returns flag when it is set. Otherwise it returns the issuer
// in an existing cluster's discovery document, or Local.issuer for a k3d
// cluster.
func (l *Local) clusterIssuer(ctx context.Context, cluster, flag string) (string, error) {
	if flag != "" || !l.Existing() {
		return l.issuer(flag)
	}
	raw, err := l.kubectl(ctx, cluster, nil, "get", "--raw", "/.well-known/openid-configuration")
	if err != nil {
		return "", fmt.Errorf("%w: reading the cluster's discovery document: %w", ErrIssuer, err)
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil || doc.Issuer == "" {
		return "", fmt.Errorf("%w: the cluster's discovery document does not name an issuer", ErrIssuer)
	}
	return doc.Issuer, nil
}

// CheckIssuer mints a ServiceAccount token in the cluster and verifies it
// against the issuer's https URL, as the Keycard zone does. It fails unless
// the issuer's discovery document and JWKS are served at that URL. It trusts
// the system roots, and the roots in KEYCARD_LOCAL_CA_FILE when that is set.
//
// Empty cluster and issuerURL default to the settings, as Local.cluster and
// Local.issuer describe.
func (l *Local) CheckIssuer(ctx context.Context, cluster, issuerURL string) error {
	cluster, err := l.cluster(cluster)
	if err != nil {
		return err
	}
	if issuerURL, err = l.clusterIssuer(ctx, cluster, issuerURL); err != nil {
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
