package sandbox

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/keycardai/agent-sandbox-poc/internal/pinned"
)

// k3sImage is a k3s stable channel release, verified with k3d 5.9.0.
const k3sImage = "rancher/k3s:v1.36.4-k3s1"

var (
	// ErrDocker is k3d failing to list its clusters, most often because
	// Docker is not running.
	ErrDocker = errors.New("k3d unavailable")
	// ErrConfirm is a deletion the operator did not confirm.
	ErrConfirm = errors.New("not confirmed")
	// ErrOtherIssuer is an existing cluster created with another issuer.
	// It comes wrapped with ErrIssuer.
	ErrOtherIssuer = errors.New("cluster has another issuer")
)

var (
	// issuerHost is https:// and a host of lowercase ASCII DNS labels.
	issuerHost = regexp.MustCompile(`^https://[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:|/|$)`)
	// issuerShape adds an optional port and a path of unreserved segments.
	issuerShape = regexp.MustCompile(
		`^https://[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:[1-9][0-9]{0,4})?(/[A-Za-z0-9._~-]+)*$`,
	)
)

// Bootstrap creates the k3d cluster with the ServiceAccount issuer set on its
// API server, and checks that a token the cluster mints names the issuer.
//
// k3d sets the issuer only when it creates a cluster, so Bootstrap does not
// recreate an existing cluster. It returns ErrOtherIssuer when the existing
// cluster's discovery document names another issuer or jwks_uri.
//
// With dryRun, Bootstrap checks the settings and prints the k3d command. It
// does not run any tool.
//
// Empty cluster and issuerURL default to the settings, as Local.cluster and
// Local.issuer describe.
func (l *Local) Bootstrap(ctx context.Context, cluster, issuerURL string, dryRun bool) error {
	cluster, err := l.k3dName("bootstrap", cluster)
	if err != nil {
		return err
	}
	issuerURL, err = l.issuer(issuerURL)
	if err != nil {
		return err
	}
	if err = checkClusterIssuer(issuerURL); err != nil {
		return err
	}
	jwksURI := issuerURL + "/openid/v1/jwks"
	// `@server:*` sends each flag to every server node, and k3s passes
	// --kube-apiserver-arg on to kube-apiserver. The local cluster serves
	// nothing through an ingress or a LoadBalancer Service, so k3s's Traefik
	// and servicelb are off.
	create := []string{
		"cluster", "create", cluster,
		"--image", k3sImage,
		"--kubeconfig-update-default=true",
		"--kubeconfig-switch-context=false",
	}
	for _, arg := range []string{
		"--kube-apiserver-arg=service-account-issuer=" + issuerURL + "@server:*",
		"--kube-apiserver-arg=service-account-jwks-uri=" + jwksURI + "@server:*",
		"--disable=traefik@server:*",
		"--disable=servicelb@server:*",
	} {
		create = append(create, "--k3s-arg", arg)
	}
	if dryRun {
		_, _ = fmt.Fprintf(l.Out, "would run: k3d %s\n", strings.Join(create, " "))
		return nil
	}

	exists, err := l.clusterExists(ctx, cluster)
	if err != nil {
		return err
	}
	if exists {
		_, _ = fmt.Fprintf(l.Out, "The k3d cluster %s exists. Checking its issuer.\n", cluster)
		if err = l.checkDiscovery(ctx, cluster, issuerURL, jwksURI); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintf(l.Out, "Creating the k3d cluster %s (%s) with issuer %s.\n", cluster, k3sImage, issuerURL)
		if err = l.k3d(ctx, l.Out, create...); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(l.Out, "Created the k3d cluster %s, and added the context k3d-%s to your kubeconfig.\n",
			cluster, cluster)
	}
	return l.checkTokenIssuer(ctx, cluster, issuerURL)
}

// Down deletes the k3d cluster. Unless yes is set, it asks first and reads
// the answer from answers. Only an answer that starts with y or Y deletes.
// A cluster that does not exist is not an error.
//
// An empty cluster defaults to the settings, as Local.cluster describes.
func (l *Local) Down(ctx context.Context, cluster string, yes bool, answers io.Reader) error {
	cluster, err := l.k3dName("down", cluster)
	if err != nil {
		return err
	}
	exists, err := l.clusterExists(ctx, cluster)
	if err != nil {
		return err
	}
	if !exists {
		_, _ = fmt.Fprintf(l.Out, "There is no k3d cluster named %s. Nothing to delete.\n", cluster)
		return nil
	}
	if !yes {
		_, _ = fmt.Fprintf(l.Out, "Delete the k3d cluster %s? [y/N] ", cluster)
		answer, readErr := bufio.NewReader(answers).ReadString('\n')
		if answer == "" && readErr != nil {
			return fmt.Errorf("%w: no answer. Pass --yes to delete without asking", ErrConfirm)
		}
		if !strings.HasPrefix(answer, "y") && !strings.HasPrefix(answer, "Y") {
			return fmt.Errorf("%w: nothing deleted", ErrConfirm)
		}
	}
	if err = l.k3d(ctx, l.Out, "cluster", "delete", cluster); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(l.Out, "Deleted the k3d cluster %s, and removed the context k3d-%s from your kubeconfig.\n",
		cluster, cluster)
	return nil
}

// checkClusterIssuer refuses an issuer that kube-apiserver would refuse or
// that a zone could not fetch: https only, a host of lowercase ASCII DNS
// labels, an optional port, and a path of unreserved segments without a
// trailing slash, query or fragment.
func checkClusterIssuer(issuerURL string) error {
	switch {
	case !strings.HasPrefix(issuerURL, "https://"):
		return fmt.Errorf("%w: the issuer must be https (got %q)", ErrIssuer, issuerURL)
	case strings.ContainsAny(issuerURL, "?#"):
		return fmt.Errorf("%w: the issuer must not have a query or fragment (got %q)", ErrIssuer, issuerURL)
	case strings.HasSuffix(issuerURL, "/"):
		return fmt.Errorf("%w: the issuer must not end in a slash (got %q)", ErrIssuer, issuerURL)
	case !issuerHost.MatchString(issuerURL):
		return fmt.Errorf("%w: the issuer must name a host of lowercase ASCII DNS labels (got %q)", ErrIssuer, issuerURL)
	case !issuerShape.MatchString(issuerURL):
		return fmt.Errorf("%w: after the host, the issuer may have only a port and a path of A-Z a-z 0-9 . _ ~ - (got %q)",
			ErrIssuer, issuerURL)
	}
	parsed, err := url.Parse(issuerURL)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrIssuer, err)
	}
	if port := parsed.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n > 65535 {
			return fmt.Errorf("%w: the issuer port must be 1-65535 (got %q)", ErrIssuer, issuerURL)
		}
	}
	return nil
}

// clusterExists reports whether k3d lists a cluster named exactly cluster. A
// listing that fails, or that is not a list of named clusters, is an error,
// so that a stopped Docker does not look like a missing cluster.
func (l *Local) clusterExists(ctx context.Context, cluster string) (bool, error) {
	cmd, err := l.Tool(ctx, "k3d", "cluster", "list", "-o", "json")
	if err != nil {
		return false, err
	}
	out, err := pinned.Run(cmd, nil)
	if err != nil {
		return false, fmt.Errorf("%w: cannot list k3d clusters. Is Docker running? %w", ErrDocker, err)
	}
	var list []struct {
		Name *string `json:"name"`
	}
	if err = json.Unmarshal(out, &list); err != nil || list == nil {
		return false, fmt.Errorf("%w: k3d cluster list printed something other than a cluster list", ErrDocker)
	}
	found := false
	for _, entry := range list {
		if entry.Name == nil {
			return false, fmt.Errorf("%w: k3d cluster list printed something other than a cluster list", ErrDocker)
		}
		found = found || *entry.Name == cluster
	}
	return found, nil
}

// checkDiscovery returns ErrOtherIssuer when the cluster's discovery document
// names another issuer or jwks_uri than issuerURL and jwksURI.
func (l *Local) checkDiscovery(ctx context.Context, cluster, issuerURL, jwksURI string) error {
	raw, err := l.kubectl(ctx, cluster, nil, "get", "--raw", "/.well-known/openid-configuration")
	if err != nil {
		return fmt.Errorf("reading the cluster's discovery document to check its issuer: %w", err)
	}
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%w: the cluster's discovery document is not JSON: %w", ErrIssuer, err)
	}
	if doc.Issuer != issuerURL || doc.JWKSURI != jwksURI {
		return fmt.Errorf("%w: %w: cluster %s was created with other settings:\n\n"+
			"    issuer   : %s (wanted %s)\n"+
			"    jwks_uri : %s (wanted %s)\n\n"+
			"    k3d sets them only when it creates a cluster. Delete the cluster with\n"+
			"    go run ./cmd/sandbox down --cluster %s, or bootstrap with its issuer",
			ErrIssuer, ErrOtherIssuer, cluster, doc.Issuer, issuerURL, doc.JWKSURI, jwksURI, cluster)
	}
	return nil
}

// checkTokenIssuer mints a token for the default ServiceAccount and returns
// ErrIssuer when its iss is not issuerURL.
func (l *Local) checkTokenIssuer(ctx context.Context, cluster, issuerURL string) error {
	_, err := l.kubectl(ctx, cluster, nil, "--namespace", "default",
		"wait", "--for=create", "serviceaccount/default", "--timeout=120s")
	if err != nil {
		return err
	}
	token, err := l.kubectl(
		ctx,
		cluster,
		nil,
		"--namespace",
		"default",
		"create",
		"token",
		"default",
		"--duration",
		"10m",
	)
	if err != nil {
		return err
	}
	iss := tokenIssuer(strings.TrimSpace(string(token)))
	if iss != issuerURL {
		return fmt.Errorf("%w: the cluster mints tokens with iss %q, want %s", ErrIssuer, iss, issuerURL)
	}
	_, _ = fmt.Fprintf(l.Out, "%s mints tokens with iss %s. Use kubectl --context k3d-%s.\n", cluster, iss, cluster)
	return nil
}

// tokenIssuer returns the iss claim of a JWT without verifying it, or "" when
// the token has no readable iss.
func tokenIssuer(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 { //nolint:mnd // A compact JWS has three parts.
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err = json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Iss
}

// k3d runs k3d with its output on out.
func (l *Local) k3d(ctx context.Context, out io.Writer, args ...string) error {
	cmd, err := l.Tool(ctx, "k3d", args...)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = out, out
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("k3d %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
