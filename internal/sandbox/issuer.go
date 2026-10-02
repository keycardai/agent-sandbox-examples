package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocaldevContext is the kubectl context of the localdev cluster.
const LocaldevContext = "k3d-localdev"

// The paths a ServiceAccount issuer serves its discovery document and keys at.
const (
	discoveryPath = "/.well-known/openid-configuration"
	jwksPath      = "/openid/v1/jwks"
)

// issuerRouteName names the HTTPRoute and its two filters in localdev's
// gateway namespace.
const (
	issuerRouteName  = "sandbox-oidc"
	gatewayNamespace = "gateway"
	envoyGateway     = "gateway.envoyproxy.io"
	routeFilterKind  = "HTTPRouteFilter"
)

var errIssuerURL = errors.New("the issuer must be an https URL with no path to be served by the gateway")

// PublishIssuer serves the sandbox cluster's discovery document and keys at
// the issuer URL through localdev's gateway. svc-sts fetches both whenever it
// verifies a workload token, and it already trusts the gateway's
// *.localdev.keycard.sh certificate, so svc-sts needs no change. The
// documents are snapshots, so this runs again after the cluster is recreated.
func PublishIssuer(ctx context.Context, sandbox, localdev Kube, issuer string) error {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" {
		return fmt.Errorf("%w (got %q)", errIssuerURL, issuer)
	}
	discovery, err := compactJSON(sandbox.GetRaw(ctx, discoveryPath))
	if err != nil {
		return fmt.Errorf("read the discovery document: %w", err)
	}
	jwks, err := compactJSON(sandbox.GetRaw(ctx, jwksPath))
	if err != nil {
		return fmt.Errorf("read the keys: %w", err)
	}
	route := &httpRoute{
		APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute",
		ObjectMeta: managedMeta(issuerRouteName, gatewayNamespace, nil),
		Spec: httpRouteSpec{
			ParentRefs: []parentRef{{Name: "gateway", Namespace: gatewayNamespace, SectionName: "https"}},
			Hostnames:  []string{parsed.Host},
			Rules: []routeRule{
				exactRoute(discoveryPath, issuerRouteName+"-discovery"),
				exactRoute(jwksPath, issuerRouteName+"-jwks"),
			},
		},
	}
	return localdev.Apply(ctx,
		directResponse(issuerRouteName+"-discovery", discovery),
		directResponse(issuerRouteName+"-jwks", jwks),
		route)
}

// UnpublishIssuer removes the route and its filters from localdev.
func UnpublishIssuer(ctx context.Context, localdev Kube) error {
	filter := "httproutefilter." + envoyGateway + "/" + issuerRouteName
	return localdev.Delete(ctx, gatewayNamespace,
		"httproute/"+issuerRouteName, filter+"-discovery", filter+"-jwks")
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

// httpRouteFilter is Envoy Gateway's filter that answers a route with a fixed
// body.
type httpRouteFilter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`

	Spec struct {
		DirectResponse struct {
			ContentType string `json:"contentType"`
			StatusCode  int    `json:"statusCode"`
			Body        struct {
				Type   string `json:"type"`
				Inline string `json:"inline"`
			} `json:"body"`
		} `json:"directResponse"`
	} `json:"spec"`
}

func directResponse(name, body string) *httpRouteFilter {
	filter := &httpRouteFilter{
		APIVersion: envoyGateway + "/v1alpha1", Kind: routeFilterKind,
		ObjectMeta: managedMeta(name, gatewayNamespace, nil),
	}
	answer := &filter.Spec.DirectResponse
	answer.ContentType = "application/json"
	answer.StatusCode = http.StatusOK
	answer.Body.Type = "Inline"
	answer.Body.Inline = body
	return filter
}

// httpRoute is the part of a Gateway API HTTPRoute the issuer route uses.
type httpRoute struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`

	Spec httpRouteSpec `json:"spec"`
}

type httpRouteSpec struct {
	ParentRefs []parentRef `json:"parentRefs"`
	Hostnames  []string    `json:"hostnames"`
	Rules      []routeRule `json:"rules"`
}

type parentRef struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	SectionName string `json:"sectionName"`
}

type routeRule struct {
	Matches []routeMatch     `json:"matches"`
	Filters []routeFilterRef `json:"filters"`
}

type routeMatch struct {
	Path struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"path"`
}

type routeFilterRef struct {
	Type         string `json:"type"`
	ExtensionRef struct {
		Group string `json:"group"`
		Kind  string `json:"kind"`
		Name  string `json:"name"`
	} `json:"extensionRef"`
}

// exactRoute answers exactly this path with the filter's fixed body.
func exactRoute(path, filter string) routeRule {
	var match routeMatch
	match.Path.Type = "Exact"
	match.Path.Value = path
	var ref routeFilterRef
	ref.Type = "ExtensionRef"
	ref.ExtensionRef.Group = envoyGateway
	ref.ExtensionRef.Kind = routeFilterKind
	ref.ExtensionRef.Name = filter
	return routeRule{Matches: []routeMatch{match}, Filters: []routeFilterRef{ref}}
}

// VerifyIssuer checks the published issuer the way svc-sts does: it fetches
// the discovery document over HTTPS, requires the issuer to match exactly,
// fetches the keys it names, and verifies a freshly minted token with them.
// The route takes a moment to program, so it retries for half a minute.
func VerifyIssuer(ctx context.Context, sandbox Kube, issuer string) error {
	const (
		requestTimeout = 15 * time.Second
		programTime    = 30 * time.Second
		retryEvery     = 2 * time.Second
		audience       = "keycard-sandbox-check"
	)
	ctx = oidc.ClientContext(ctx, newHTTPClient(requestTimeout))

	var provider *oidc.Provider
	var err error
	deadline := time.Now().Add(programTime)
	for {
		provider, err = oidc.NewProvider(ctx, issuer)
		if err == nil || time.Now().After(deadline) {
			break
		}
		if !sleep(ctx, retryEvery) {
			return fmt.Errorf("wait for the issuer route: %w", ctx.Err())
		}
	}
	if err != nil {
		return fmt.Errorf("fetch the discovery document from %s: %w", issuer, err)
	}
	token, err := sandbox.CreateToken(ctx, "default", "default", audience)
	if err != nil {
		return fmt.Errorf("mint a token to check: %w", err)
	}
	if _, err := provider.Verifier(&oidc.Config{ClientID: audience}).Verify(ctx, token); err != nil {
		return fmt.Errorf("verify a minted token against %s: %w", issuer, err)
	}
	return nil
}
