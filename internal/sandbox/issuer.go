package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// LocaldevContext is the kubectl context of the localdev cluster.
const LocaldevContext = "k3d-localdev"

// managedBy labels everything the CLI creates outside its own cluster, so
// `sandbox down` can find it.
const managedBy = "keycard-sandbox"

// issuerRouteName names the HTTPRoute and its two filters in localdev's
// gateway namespace.
const issuerRouteName = "sandbox-oidc"

// PublishIssuer serves the sandbox cluster's discovery document and keys at
// the issuer URL through localdev's gateway. svc-sts fetches both whenever it
// verifies a workload token, and it already trusts the gateway's
// *.localdev.keycard.sh certificate, so svc-sts needs no change. The
// documents are snapshots, so this runs again after the cluster is recreated.
func PublishIssuer(ctx context.Context, sandbox, localdev Kube, issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" || u.Path != "" {
		return fmt.Errorf("the issuer must be an https URL with no path to be served by the gateway (got %q)", issuer)
	}
	discovery, err := compactJSON(sandbox.GetRaw(ctx, "/.well-known/openid-configuration"))
	if err != nil {
		return fmt.Errorf("read the discovery document: %w", err)
	}
	jwks, err := compactJSON(sandbox.GetRaw(ctx, "/openid/v1/jwks"))
	if err != nil {
		return fmt.Errorf("read the keys: %w", err)
	}
	return localdev.Apply(ctx, []map[string]any{
		directResponse(issuerRouteName+"-discovery", discovery),
		directResponse(issuerRouteName+"-jwks", jwks),
		{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata":   meta(issuerRouteName, "gateway"),
			"spec": map[string]any{
				"parentRefs": []any{map[string]any{"name": "gateway", "namespace": "gateway", "sectionName": "https"}},
				"hostnames":  []any{u.Host},
				"rules": []any{
					routeRule("/.well-known/openid-configuration", issuerRouteName+"-discovery"),
					routeRule("/openid/v1/jwks", issuerRouteName+"-jwks"),
				},
			},
		},
	})
}

// UnpublishIssuer removes the route and its filters from localdev.
func UnpublishIssuer(ctx context.Context, localdev Kube) error {
	return localdev.Delete(ctx, "gateway",
		"httproute/"+issuerRouteName,
		"httproutefilter.gateway.envoyproxy.io/"+issuerRouteName+"-discovery",
		"httproutefilter.gateway.envoyproxy.io/"+issuerRouteName+"-jwks")
}

func compactJSON(raw []byte, err error) (string, error) {
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", fmt.Errorf("not JSON: %w", err)
	}
	return buf.String(), nil
}

func meta(name, namespace string) map[string]any {
	return map[string]any{
		"name":      name,
		"namespace": namespace,
		"labels":    map[string]any{"app.kubernetes.io/managed-by": managedBy},
	}
}

func directResponse(name, body string) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.envoyproxy.io/v1alpha1",
		"kind":       "HTTPRouteFilter",
		"metadata":   meta(name, "gateway"),
		"spec": map[string]any{"directResponse": map[string]any{
			"contentType": "application/json",
			"statusCode":  200,
			"body":        map[string]any{"type": "Inline", "inline": body},
		}},
	}
}

func routeRule(path, filter string) map[string]any {
	return map[string]any{
		"matches": []any{map[string]any{"path": map[string]any{"type": "Exact", "value": path}}},
		"filters": []any{map[string]any{"type": "ExtensionRef", "extensionRef": map[string]any{
			"group": "gateway.envoyproxy.io", "kind": "HTTPRouteFilter", "name": filter,
		}}},
	}
}

// VerifyIssuer checks the published issuer the way svc-sts does: it fetches
// the discovery document over HTTPS, requires the issuer to match exactly,
// fetches the keys it names, and verifies a freshly minted token with them.
// The route takes a moment to program, so it retries for half a minute.
func VerifyIssuer(ctx context.Context, sandbox Kube, issuer string) error {
	pool, err := trustPool()
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig.RootCAs = pool
	ctx = oidc.ClientContext(ctx, &http.Client{Transport: transport, Timeout: 15 * time.Second})

	var provider *oidc.Provider
	deadline := time.Now().Add(30 * time.Second)
	for {
		provider, err = oidc.NewProvider(ctx, issuer)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("fetch the discovery document from %s: %w", issuer, err)
	}
	const audience = "keycard-sandbox-check"
	token, err := sandbox.CreateToken(ctx, "default", "default", audience)
	if err != nil {
		return fmt.Errorf("mint a token to check: %w", err)
	}
	if _, err := provider.Verifier(&oidc.Config{ClientID: audience}).Verify(ctx, token); err != nil {
		return fmt.Errorf("verify a minted token against %s: %w", issuer, err)
	}
	return nil
}
