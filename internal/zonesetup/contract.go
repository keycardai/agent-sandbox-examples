package zonesetup

import (
	"fmt"
	"net/url"
)

// applicationName is the display name of the egress proxy Application.
const applicationName = "Sandbox egress proxy"

// ApplicationIdentifier is the well-known identifier of the egress proxy
// Application. The per-user package finds the per-zone install's Application
// by this identifier, so no install id has to be passed between the two.
func ApplicationIdentifier(namespace string) string {
	return "urn:keycard:sandbox-proxy:" + namespace
}

// ProviderIdentifier is the identifier of the OIDC Provider for a cluster
// issuer. It is the issuer URL exactly as the values file gives it.
func ProviderIdentifier(issuerURL string) string {
	return issuerURL
}

// providerName is the display name of the Provider for a cluster issuer.
func providerName(issuerURL string) string {
	host := issuerURL
	if u, err := url.Parse(issuerURL); err == nil && u.Host != "" {
		host = u.Host
	}
	return "Kubernetes cluster " + host
}

// ServiceAccountSubject is the `sub` claim of a projected ServiceAccount
// token, and so the subject the workload-identity credential is bound to.
// The ServiceAccount is named for the user's public id.
func ServiceAccountSubject(namespace, userID string) string {
	return fmt.Sprintf("system:serviceaccount:%s:%s", namespace, userID)
}

// PolicyName names the impersonation policy for one user.
func PolicyName(userID string) string {
	return "Sandbox impersonation: " + userID
}

// ImpersonationPolicy is the Cedar policy that lets exactly one
// ServiceAccount subject impersonate exactly one user. The API server signs
// the ServiceAccount token, so a pod cannot forge the actor half. A pod that
// names another user in its configuration fails the exchange with
// access_denied.
//
// Every context attribute the policy reads is optional in the zone's Cedar
// schema, so each one is guarded with `has` first. Reading an optional
// attribute unguarded fails validation when the version is published, with
// "unable to guarantee safety of access to optional attribute". The working
// shape is `cedarPermitImpersonationIssuerClaims` in
// svc-pdp/integration/policies_test.go.
func ImpersonationPolicy(namespace, userID string) string {
	return fmt.Sprintf(`permit (
  principal is Keycard::Application,
  action,
  resource
) when {
  context has impersonate && context.impersonate == true &&
  context has subject &&
  context.subject.identifier == %q &&
  context has actor_claims &&
  context.actor_claims has issuer_claims &&
  context.actor_claims.issuer_claims.contains({
    "key": "sub", "value": %q
  })
};
`, userID, ServiceAccountSubject(namespace, userID))
}
