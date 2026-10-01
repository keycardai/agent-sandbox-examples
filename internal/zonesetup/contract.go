package zonesetup

import "net/url"

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
