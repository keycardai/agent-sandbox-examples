package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
)

// k3sImage is a k3s stable channel release, verified with k3d 5.9.0.
const k3sImage = "rancher/k3s:v1.36.4-k3s1"

var (
	// ErrDocker means docker or k3d could not connect to Docker. Usually Docker
	// is not installed or not running.
	ErrDocker = errors.New("docker unavailable")
	// ErrConfirm means the operator did not confirm a deletion.
	ErrConfirm = errors.New("not confirmed")
	// ErrOtherIssuer is an existing cluster created with another issuer.
	// An error that wraps it also wraps ErrIssuer.
	ErrOtherIssuer = errors.New("cluster has another issuer")
	// ErrBrokenCluster is a k3d cluster that k3d lists but kubectl cannot
	// use, because the kubeconfig has no context for it.
	ErrBrokenCluster = errors.New("cluster is not usable")
)

var (
	// k3dColor is a terminal color code. k3d colors its log lines even when
	// its output is not a terminal.
	k3dColor = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	// k3dFailure is a k3d log line at the error or fatal level, such as
	// FATA[0000] followed by the message.
	k3dFailure = regexp.MustCompile(`^(?:ERRO|FATA)\[\d+\]\s*(.+)$`)
	// issuerHost matches https:// and a host of lowercase ASCII DNS labels.
	issuerHost = regexp.MustCompile(`^https://[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:|/|$)`)
	// issuerShape also allows a port and a path of unreserved segments.
	issuerShape = regexp.MustCompile(
		`^https://[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*(:[1-9][0-9]{0,4})?(/[A-Za-z0-9._~-]+)*$`,
	)
)

// Bootstrap creates the k3d cluster with the ServiceAccount issuer set on its
// API server. It then checks that a token the cluster mints has that issuer.
//
// Bootstrap keeps an existing cluster. It returns ErrOtherIssuer when that
// cluster's discovery document has another issuer or jwks_uri, and
// ErrBrokenCluster when the kubeconfig has no context for it. The error says
// how to delete the cluster.
//
// With dryRun, Bootstrap checks the settings and prints the k3d command. It
// does not run a tool.
//
// An empty cluster or issuerURL comes from the settings.
func (l *Local) Bootstrap(ctx context.Context, cluster, issuerURL string, dryRun bool) error {
	err := l.bootstrap(ctx, cluster, issuerURL, dryRun)
	if !errors.Is(err, ErrOtherIssuer) && !errors.Is(err, ErrBrokenCluster) {
		return err
	}
	advice := "Delete the cluster"
	if name, nameErr := l.cluster(cluster); nameErr == nil {
		advice += " with\n    go run ./cmd/sandbox down --cluster " + name
	}
	if errors.Is(err, ErrOtherIssuer) {
		advice += ", or bootstrap with its issuer"
	}
	return fmt.Errorf("%w. %s", err, advice)
}

// bootstrap is Bootstrap without the advice to delete the cluster. up uses
// it, because up asks to delete the cluster itself.
func (l *Local) bootstrap(ctx context.Context, cluster, issuerURL string, dryRun bool) error {
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
	// `@server:*` sends each flag to every server node. k3s passes
	// --kube-apiserver-arg to kube-apiserver. Traefik and servicelb are off
	// because the local cluster does not use an ingress or a LoadBalancer
	// Service.
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
		err = l.checkDiscovery(ctx, cluster, issuerURL, jwksURI)
	} else {
		err = l.createCluster(ctx, cluster, issuerURL, create)
	}
	if err != nil {
		return err
	}
	return l.checkTokenIssuer(ctx, cluster, issuerURL)
}

// createCluster removes what an earlier cluster of the same name left behind,
// then runs k3d with the create args.
func (l *Local) createCluster(ctx context.Context, cluster, issuerURL string, create []string) error {
	if err := l.removeLeftovers(ctx, cluster); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(l.Out, "Creating the k3d cluster %s (%s) with issuer %s.\n", cluster, k3sImage, issuerURL)
	if err := l.k3d(ctx, l.Out, create...); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(l.Out, "Created the k3d cluster %s, and added the context k3d-%s to your kubeconfig.\n",
		cluster, cluster)
	return nil
}

// removeLeftovers deletes the containers of a k3d cluster that k3d does not
// list, once the operator confirms. k3d lists a cluster only while it has a
// server, agent or load balancer container, but refuses to create a cluster
// while any container has its k3d.cluster label. The default answer keeps
// the containers.
func (l *Local) removeLeftovers(ctx context.Context, cluster string) error {
	cmd, err := l.Tool(ctx, "docker", "ps", "--all", "--filter", "label=k3d.cluster="+cluster, "--format", "{{.Names}}")
	if err != nil {
		return err
	}
	out, err := pinned.Run(cmd, nil)
	if err != nil {
		return fmt.Errorf("%w: cannot list the containers of the k3d cluster %s: %w", ErrDocker, cluster, err)
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return nil
	}
	leftovers := strings.Join(names, ", ")
	yes, err := l.confirm(ctx, fmt.Sprintf("Remove %s, left over from an earlier k3d cluster %s?",
		leftovers, cluster), false)
	switch {
	case errors.Is(err, ErrNoTerminal):
		return fmt.Errorf("%w: %w. k3d cannot create the cluster %s while %s exists. Remove it with\n"+
			"    k3d cluster delete %s", ErrConfirm, err, cluster, leftovers, cluster)
	case err != nil:
		return fmt.Errorf("%w: nothing removed: %w", ErrConfirm, err)
	case !yes:
		return fmt.Errorf("%w: nothing removed. k3d cannot create the cluster %s while %s exists",
			ErrConfirm, cluster, leftovers)
	}
	_, _ = fmt.Fprintf(l.Out, "Removing %s, left over from an earlier k3d cluster %s.\n", leftovers, cluster)
	return l.k3d(ctx, l.Out, "cluster", "delete", cluster)
}

// Down deletes the k3d cluster. Unless yes is set, it asks first, and it
// deletes only when the operator answers yes. A missing cluster is not an
// error.
//
// An empty cluster comes from the settings.
func (l *Local) Down(ctx context.Context, cluster string, yes bool) error {
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
		if err = l.confirmDelete(ctx, cluster); err != nil {
			return err
		}
	}
	if err = l.k3d(ctx, l.Out, "cluster", "delete", cluster); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(l.Out, "Deleted the k3d cluster %s, and removed the context k3d-%s from your kubeconfig.\n",
		cluster, cluster)
	return nil
}

// confirmDelete asks whether to delete cluster. The default answer is no. It
// returns ErrConfirm unless the operator answers yes.
func (l *Local) confirmDelete(ctx context.Context, cluster string) error {
	yes, err := l.confirm(ctx, "Delete the k3d cluster "+cluster+"?", false)
	switch {
	case errors.Is(err, ErrNoTerminal):
		return fmt.Errorf("%w: %w. Pass --yes to delete without asking", ErrConfirm, err)
	case err != nil:
		return fmt.Errorf("%w: nothing deleted: %w", ErrConfirm, err)
	case !yes:
		return fmt.Errorf("%w: nothing deleted", ErrConfirm)
	}
	return nil
}

// checkClusterIssuer accepts an https URL with a host of lowercase ASCII DNS
// labels, an optional port, and a path of unreserved segments. It refuses a
// trailing slash, query or fragment. kube-apiserver and the zone both need
// this form.
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

// clusterExists reports whether k3d lists a cluster named exactly cluster.
// It returns ErrDocker when the listing fails or is not a list of named
// clusters, so a stopped Docker never looks like a missing cluster.
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

// checkDiscovery returns ErrIssuer when the cluster's discovery document has
// an issuer other than issuerURL or a jwks_uri other than jwksURI. It returns
// ErrBrokenCluster when the kubeconfig has no context for the cluster.
func (l *Local) checkDiscovery(ctx context.Context, cluster, issuerURL, jwksURI string) error {
	raw, err := l.kubectl(ctx, cluster, nil, "get", "--raw", "/.well-known/openid-configuration")
	if err != nil && strings.Contains(err.Error(), fmt.Sprintf("context %q does not exist", l.kubeContext(cluster))) {
		return fmt.Errorf("%w: k3d lists the cluster %s, but your kubeconfig has no context %s",
			ErrBrokenCluster, cluster, l.kubeContext(cluster))
	}
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
			"    k3d sets them only when it creates a cluster",
			ErrIssuer, ErrOtherIssuer, cluster, doc.Issuer, issuerURL, doc.JWKSURI, jwksURI)
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

// k3d runs k3d with its output on out. When k3d fails, the error names the
// command and has the first error k3d logged.
func (l *Local) k3d(ctx context.Context, out io.Writer, args ...string) error {
	cmd, err := l.Tool(ctx, "k3d", args...)
	if err != nil {
		return err
	}
	var logged bytes.Buffer
	both := io.MultiWriter(out, &logged)
	cmd.Stdout, cmd.Stderr = both, both
	if err = cmd.Run(); err != nil {
		if line := k3dError(logged.String()); line != "" {
			return fmt.Errorf("k3d %s: %s: %w", k3dCommand(args), line, err)
		}
		return fmt.Errorf("k3d %s: %w", k3dCommand(args), err)
	}
	return nil
}

// k3dCommand returns the words of args before the first flag, such as
// "cluster create asp".
func k3dCommand(args []string) string {
	end := slices.IndexFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "-") })
	if end < 0 {
		end = len(args)
	}
	return strings.Join(args[:end], " ")
}

// k3dError returns the first error or fatal line in k3d's output, without its
// colors and level, or "" when there is none. The first one names the cause.
// After a failed create, k3d rolls back and then logs a fatal line without
// the cause.
func k3dError(logged string) string {
	for line := range strings.Lines(k3dColor.ReplaceAllString(logged, "")) {
		if match := k3dFailure.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
			return match[1]
		}
	}
	return ""
}
