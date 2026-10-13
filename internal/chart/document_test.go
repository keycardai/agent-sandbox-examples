package chart_test

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// This file is a copy of the egress proxy's configuration rules, from
// internal/config/config.go and docs/configuration.md in egress-proxy. The
// tests check each document the sandbox chart renders against it.
// sandbox/README.md says why it is a copy.
//
// The decode is strict. A key the proxy does not read is an error. The copy
// leaves out the sections the chart does not set (ca, limits, credentials and
// netsetup) and every key of listen except transparent_port and dns_port. Add
// a section or key here when the chart sets it.

// document is the proxy's configuration document.
type document struct {
	Version   int        `json:"version"`
	Sandbox   sandboxID  `json:"sandbox"`
	Identity  *identity  `json:"identity,omitempty"`
	Listen    listenDoc  `json:"listen"`
	TLS       *tlsDoc    `json:"tls,omitempty"`
	Policy    policyDoc  `json:"policy"`
	Endpoints []endpoint `json:"endpoints"`
}

type sandboxID struct {
	ID string `json:"id"`
}

// listenDoc holds the listeners the capture redirects to: the transparent
// frontend and the resolver.
type listenDoc struct {
	TransparentPort int `json:"transparent_port"`
	DNSPort         int `json:"dns_port"`
}

type identity struct {
	ZoneURL           string `json:"zone_url"`
	ClientID          string `json:"client_id"`
	WorkloadTokenFile string `json:"workload_token_file"`
	ClientSecretFile  string `json:"client_secret_file"`
	Impersonate       struct {
		User string `json:"user"`
	} `json:"impersonate"`
}

type tlsDoc struct {
	UpstreamCAFiles []string `json:"upstream_ca_files"`
}

type policyDoc struct {
	ClusterCIDRs       []string `json:"cluster_cidrs"`
	DenyListExceptions []string `json:"deny_list_exceptions"`
}

type endpoint struct {
	URL        string      `json:"url"`
	Credential *credential `json:"credential,omitempty"`
}

// endpointURL holds the parts of an endpoint URL. The host is lowercase and
// has no trailing dot. The path is as written.
type endpointURL struct {
	scheme string
	host   string
	port   int
	path   string
}

type credential struct {
	Placement placement `json:"placement"`
}

type placement struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Template string `json:"template"`
	User     string `json:"user"`
}

const (
	// credentialToken is the placeholder in a header template that the proxy
	// replaces with the credential.
	credentialToken = "{credential}" //nolint:gosec // G101: a template placeholder, not a credential.
	// metadataAddress is the cloud metadata address. A deny list exception
	// cannot include it.
	metadataAddress = "169.254.169.254"
	maxDNSName      = 253
	maxDNSLabel     = 63
	maxPort         = 65535
	httpsPort       = 443
	httpPort        = 80

	// The reasons the proxy refuses an endpoint URL before it reads its parts.
	refusedScheme = "must be an absolute http or https URL"
	refusedExtras = "must not have userinfo, a query or a fragment"
	refusedPort   = "must name a port between 1 and 65535"

	// URLs of the demo endpoints in sandbox/values.example.yaml.
	githubAPIURL  = "https://api.github.com"
	openRouterURL = "https://openrouter.ai/api/v1"
)

var hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// parseDocument decodes a rendered document. It fails on a key the proxy
// does not read.
func parseDocument(t *testing.T, raw string) document {
	t.Helper()
	var doc document
	if err := yaml.UnmarshalStrict([]byte(raw), &doc); err != nil {
		t.Fatalf("the document does not parse: %v\n%s", err, raw)
	}
	return doc
}

// validateDocument returns one message for each rule the document breaks.
// It returns nothing for a document the proxy accepts.
func validateDocument(doc document) []string {
	var found []string
	if doc.Version != 1 {
		found = append(found, fmt.Sprintf("version is %d, the proxy reads version 1", doc.Version))
	}
	if strings.TrimSpace(doc.Sandbox.ID) == "" || strings.ContainsAny(doc.Sandbox.ID, " \t\r\n") {
		found = append(found, fmt.Sprintf("sandbox.id %q must be a non-empty string without whitespace", doc.Sandbox.ID))
	}
	if doc.Identity != nil {
		found = append(found, doc.Identity.problems()...)
	}
	found = append(found, doc.Listen.problems()...)
	if doc.TLS != nil {
		for i, file := range doc.TLS.UpstreamCAFiles {
			if !strings.HasPrefix(file, "/") {
				found = append(found, fmt.Sprintf("tls.upstream_ca_files[%d] %q must be an absolute path", i, file))
			}
		}
	}
	found = append(found, doc.Policy.problems()...)
	for i, entry := range doc.Endpoints {
		found = append(found, entry.problems(i)...)
	}
	return append(found, repeatProblems(doc.Endpoints)...)
}

// repeatProblems reports each endpoint with the same host, port and path as
// an earlier one.
func repeatProblems(endpoints []endpoint) []string {
	var found []string
	seen := map[string]int{}
	for i, entry := range endpoints {
		parsed, problem := parseEndpointURL(entry.URL)
		if problem != "" {
			continue
		}
		key := net.JoinHostPort(parsed.host, strconv.Itoa(parsed.port)) + parsed.path
		if previous, repeat := seen[key]; repeat {
			found = append(found, fmt.Sprintf("endpoints[%d]: url %q names the same endpoint as endpoints[%d]",
				i, entry.URL, previous))
		}
		seen[key] = i
	}
	return found
}

func (id identity) problems() []string {
	var found []string
	zone, err := url.Parse(id.ZoneURL)
	switch {
	case err != nil || zone.Scheme != "https" || zone.Host == "":
		found = append(found, fmt.Sprintf("identity.zone_url %q must be an absolute https URL", id.ZoneURL))
	case zone.User != nil:
		found = append(found, "identity.zone_url has userinfo")
	}
	if strings.TrimSpace(id.ClientID) == "" {
		found = append(found, "identity.client_id is empty")
	}
	if strings.TrimSpace(id.Impersonate.User) == "" {
		found = append(found, "identity.impersonate.user is empty")
	}
	return append(found, id.clientAuthProblems()...)
}

// clientAuthProblems checks that the identity sets exactly one of
// workload_token_file and client_secret_file, as an absolute path.
func (id identity) clientAuthProblems() []string {
	var found []string
	if (id.WorkloadTokenFile == "") == (id.ClientSecretFile == "") {
		found = append(found, "identity names none or both of workload_token_file and client_secret_file")
	}
	for key, file := range map[string]string{
		"identity.workload_token_file": id.WorkloadTokenFile,
		"identity.client_secret_file":  id.ClientSecretFile,
	} {
		if file != "" && !strings.HasPrefix(file, "/") {
			found = append(found, fmt.Sprintf("%s %q must be an absolute path", key, file))
		}
	}
	return found
}

// problems requires listen.transparent_port and listen.dns_port. The proxy
// accepts a document without them and then does not run a transparent
// frontend or a resolver. A sandbox pod always captures traffic, so the
// chart must set both.
func (l listenDoc) problems() []string {
	var found []string
	if l.TransparentPort < 1 || l.TransparentPort > maxPort {
		found = append(found, fmt.Sprintf("listen.transparent_port is %d, outside 1-%d", l.TransparentPort, maxPort))
	}
	if l.DNSPort < 1 || l.DNSPort > maxPort {
		found = append(found, fmt.Sprintf("listen.dns_port is %d, outside 1-%d", l.DNSPort, maxPort))
	}
	return found
}

func (p policyDoc) problems() []string {
	var found []string
	for i, raw := range p.ClusterCIDRs {
		if _, err := netip.ParsePrefix(raw); err != nil {
			found = append(found, fmt.Sprintf("policy.cluster_cidrs[%d] %q is not a CIDR", i, raw))
		}
	}
	for i, raw := range p.DenyListExceptions {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			found = append(found, fmt.Sprintf("policy.deny_list_exceptions[%d] %q is not a CIDR", i, raw))
			continue
		}
		if prefix.Contains(netip.MustParseAddr(metadataAddress)) {
			found = append(found, fmt.Sprintf("policy.deny_list_exceptions[%d] %q covers %s", i, raw, metadataAddress))
		}
	}
	return found
}

// parseEndpointURL reads an endpoint URL as the proxy does. It returns the
// reason the proxy refuses the URL, or an empty string.
func parseEndpointURL(raw string) (endpointURL, string) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil {
		return endpointURL{}, refusedScheme
	}
	if problem := endpointURLShape(parsed); problem != "" {
		return endpointURL{}, problem
	}
	port := schemePort(parsed.Scheme)
	if text := parsed.Port(); text != "" {
		port, err = strconv.Atoi(text)
		if err != nil || port < 1 || port > maxPort {
			return endpointURL{}, refusedPort
		}
	}
	// The path is everything from the first "/" after "scheme://".
	rest := raw[len(parsed.Scheme)+len("://"):]
	urlPath := "/"
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		urlPath = rest[i:]
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	return endpointURL{scheme: parsed.Scheme, host: host, port: port, path: urlPath}, ""
}

// endpointURLShape returns refusedScheme for a URL that is not absolute http
// or https, and refusedExtras for a URL with userinfo, a query or a fragment.
func endpointURLShape(parsed *url.URL) string {
	switch {
	case schemePort(parsed.Scheme) == 0 || parsed.Host == "" || parsed.Opaque != "":
		return refusedScheme
	case parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "":
		return refusedExtras
	default:
		return ""
	}
}

// schemePort returns the default port of http or https. It returns 0 for
// any other scheme.
func schemePort(scheme string) int {
	switch scheme {
	case "https":
		return httpsPort
	case "http":
		return httpPort
	default:
		return 0
	}
}

// resource returns the canonical URL that the token exchange sends as its
// resource parameter. It leaves out the port when the port is the scheme's
// default, and leaves out a root path. Any other path stays as written.
func (u endpointURL) resource() string {
	authority := u.host
	if u.port != schemePort(u.scheme) {
		authority = net.JoinHostPort(u.host, strconv.Itoa(u.port))
	}
	urlPath := u.path
	if urlPath == "/" {
		urlPath = ""
	}
	return u.scheme + "://" + authority + urlPath
}

func (e endpoint) problems(i int) []string {
	parsed, problem := parseEndpointURL(e.URL)
	if problem != "" {
		return []string{fmt.Sprintf("endpoints[%d]: url %q %s", i, e.URL, problem)}
	}
	found := hostProblems(i, parsed.host)
	if !normalizedPrefix(parsed.path) {
		found = append(found, fmt.Sprintf(
			"endpoints[%d]: url path %q must be an absolute path in its shortest form", i, parsed.path,
		))
	}
	if e.Credential != nil {
		if parsed.scheme == "http" {
			found = append(found, fmt.Sprintf("endpoints[%d]: url %q is http and cannot have a credential", i, e.URL))
		}
		found = append(found, e.Credential.Placement.problems(i)...)
	}
	return found
}

// hostProblems checks that the name is valid as a TLS SNI host name: ASCII
// labels, and a last label that is not all digits. The digit rule refuses an
// address written as 0177.0.0.1.
func hostProblems(i int, host string) []string {
	if host == "" {
		return []string{fmt.Sprintf("endpoints[%d]: url does not have a host", i)}
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return []string{fmt.Sprintf("endpoints[%d]: host %q is an IP literal", i, host)}
	}
	if len(host) > maxDNSName {
		return []string{fmt.Sprintf("endpoints[%d]: host %q is longer than 253 characters", i, host)}
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) > maxDNSLabel || !hostLabelRE.MatchString(label) {
			return []string{fmt.Sprintf("endpoints[%d]: host %q is not a DNS name of ASCII labels", i, host)}
		}
	}
	if strings.TrimLeft(labels[len(labels)-1], "0123456789") == "" {
		return []string{fmt.Sprintf("endpoints[%d]: host %q ends in an all-digit label", i, host)}
	}
	return nil
}

// problems checks that the placement has the fields its kind needs and does
// not set fields its kind does not take.
func (p placement) problems(i int) []string {
	ok := map[string]bool{
		"header": p.Name != "" && p.User == "",
		"query":  p.Name != "" && p.Template == "" && p.User == "",
		"basic":  p.User != "" && p.Name == "" && p.Template == "",
	}
	shape, known := ok[p.Kind]
	if !known {
		return []string{fmt.Sprintf("endpoints[%d]: placement.kind %q must be header, query or basic", i, p.Kind)}
	}
	var found []string
	if !shape {
		found = append(found, fmt.Sprintf("endpoints[%d]: a %s placement has the wrong fields", i, p.Kind))
	}
	if p.Kind == "header" {
		found = append(found, p.headerProblems(i)...)
	}
	return found
}

// headerProblems checks the rules for a header placement. The template must
// contain credentialToken. The name must not be a header that controls the
// connection, the message framing or the proxy, or one that an upstream
// reads as the client's address. Authorization is allowed.
func (p placement) headerProblems(i int) []string {
	banned := []string{
		"connection", "proxy-connection", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade",
		"proxy-authenticate", "proxy-authorization", "content-length", "host", "forwarded",
	}
	var found []string
	if !strings.Contains(p.Template, credentialToken) {
		found = append(found, fmt.Sprintf("endpoints[%d]: a header template must contain %s", i, credentialToken))
	}
	lower := strings.ToLower(p.Name)
	if strings.HasPrefix(lower, "x-forwarded-") || strings.HasPrefix(lower, "sec-") || slices.Contains(banned, lower) {
		found = append(found, fmt.Sprintf("endpoints[%d]: a header placement cannot name %s", i, p.Name))
	}
	return found
}

// normalizedPrefix reports whether the path is absolute, has no query or
// fragment, is in the escaped form a client sends, and is unchanged by
// path.Clean apart from one trailing slash.
func normalizedPrefix(prefix string) bool {
	if !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "?#") {
		return false
	}
	parsed, err := url.Parse(prefix)
	if err != nil || parsed.EscapedPath() != prefix {
		return false
	}
	cleaned := path.Clean(prefix)
	return cleaned == prefix || cleaned+"/" == prefix
}

// validDocument has the shape the chart renders. Each case below changes it
// to break one rule.
const validDocument = `version: 1
sandbox:
  id: sbx-1
identity:
  zone_url: https://acme.keycard.cloud
  client_id: cred_1
  workload_token_file: /var/run/secrets/keycard/token
  impersonate:
    user: igmua2g0
listen:
  transparent_port: 1015
  dns_port: 53
policy:
  cluster_cidrs: ["10.0.0.0/16"]
  deny_list_exceptions: []
endpoints:
  - url: https://api.github.com
    credential:
      placement: {kind: header, name: Authorization, template: "Bearer {credential}"}
`

// TestDocumentMirrorRefuses checks that the copy of the proxy's rules
// refuses one broken document per rule. Without it, a copy that accepts
// every document would still pass the chart tests.
func TestDocumentMirrorRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"valid", validDocument, ""},
		{"wrong version", strings.Replace(validDocument, "version: 1", "version: 2", 1), "the proxy reads version 1"},
		{"no sandbox id", strings.Replace(validDocument, "id: sbx-1", `id: ""`, 1), "sandbox.id"},
		{
			"zone URL is not https",
			strings.Replace(validDocument, "https://acme.keycard.cloud", "http://acme.keycard.cloud", 1),
			"must be an absolute https URL",
		},
		{
			"two client authentications",
			strings.Replace(validDocument, "  impersonate:", "  client_secret_file: /run/secret\n  impersonate:", 1),
			"none or both of workload_token_file and client_secret_file",
		},
		{
			"relative token path",
			strings.Replace(validDocument, "/var/run/secrets/keycard/token", "secrets/token", 1),
			"must be an absolute path",
		},
		{
			"no transparent listener",
			strings.Replace(validDocument, "  transparent_port: 1015\n", "", 1),
			"listen.transparent_port is 0, outside 1-65535",
		},
		{
			"no resolver",
			strings.Replace(validDocument, "  dns_port: 53\n", "", 1),
			"listen.dns_port is 0, outside 1-65535",
		},
		{
			"resolver port out of range",
			strings.Replace(validDocument, "dns_port: 53", "dns_port: 65536", 1),
			"listen.dns_port is 65536, outside 1-65535",
		},
		{
			"exception uncovering the metadata address",
			strings.Replace(validDocument, "deny_list_exceptions: []", `deny_list_exceptions: ["169.254.0.0/16"]`, 1),
			"covers 169.254.169.254",
		},
		{
			"url is not http or https",
			strings.Replace(validDocument, "url: https://api.github.com", "url: ftp://api.github.com", 1),
			refusedScheme,
		},
		{
			"url has userinfo",
			strings.Replace(validDocument, "url: https://api.github.com", "url: https://git@api.github.com", 1),
			refusedExtras,
		},
		{
			"url has a query",
			strings.Replace(validDocument, "url: https://api.github.com", "url: https://api.github.com/?a=1", 1),
			refusedExtras,
		},
		{
			"url has a fragment",
			strings.Replace(validDocument, "url: https://api.github.com", "url: https://api.github.com/#a", 1),
			refusedExtras,
		},
		{
			"port out of range",
			strings.Replace(validDocument, "url: https://api.github.com", "url: https://api.github.com:65536", 1),
			refusedPort,
		},
		{
			"http endpoint with a credential",
			strings.Replace(validDocument, "url: https://api.github.com", "url: http://api.github.com", 1),
			"is http and cannot have a credential",
		},
		{
			"host is an IP literal",
			strings.Replace(validDocument, "url: https://api.github.com", "url: https://10.0.0.1", 1),
			"is an IP literal",
		},
		{
			"host ends in an all-digit label",
			strings.Replace(validDocument, "url: https://api.github.com", "url: https://2852039166", 1),
			"ends in an all-digit label",
		},
		{
			"path has dot segments",
			strings.Replace(validDocument, "url: https://api.github.com", "url: https://api.github.com/api/../secret", 1),
			"shortest form",
		},
		{
			"path holds a character a client escapes",
			strings.Replace(validDocument, "url: https://api.github.com", `url: "https://api.github.com/a b"`, 1),
			"shortest form",
		},
		{
			"header template has nowhere for the credential",
			strings.Replace(validDocument, `template: "Bearer {credential}"`, `template: Bearer`, 1),
			"must contain {credential}",
		},
		{
			"header placement names Host",
			strings.Replace(validDocument, "name: Authorization", "name: Host", 1),
			"cannot name Host",
		},
		{
			"basic placement has a name",
			strings.Replace(validDocument, "kind: header", "kind: basic, user: git", 1),
			"has the wrong fields",
		},
		{
			"placement kind the proxy does not write",
			strings.Replace(validDocument, "kind: header", "kind: cookie", 1),
			"must be header, query or basic",
		},
		{
			"relative upstream CA file",
			validDocument + "tls:\n  upstream_ca_files: [zone-ca.pem]\n",
			`tls.upstream_ca_files[0] "zone-ca.pem" must be an absolute path`,
		},
		{
			"repeated host, port and path",
			validDocument + "  - url: https://API.github.com.:443/\n",
			"names the same endpoint as endpoints[0]",
		},
		{"same host and path on another port", validDocument + "  - url: https://api.github.com:8443\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			found := validateDocument(parseDocument(t, tc.doc))
			if tc.want == "" {
				if len(found) > 0 {
					t.Fatalf("the fixture is refused: %v", found)
				}
				return
			}
			if !slices.ContainsFunc(found, func(problem string) bool { return strings.Contains(problem, tc.want) }) {
				t.Fatalf("problems = %v, want one naming %q", found, tc.want)
			}
		})
	}
}

// TestDocumentMirrorRefusesUnknownKey checks that a key the proxy does not
// read is an error. The copy reads only some keys of listen, so an unknown
// key inside listen is a separate case.
func TestDocumentMirrorRefusesUnknownKey(t *testing.T) {
	t.Parallel()
	endpoint := "  - url: https://api.github.com\n"
	cases := []struct{ name, doc string }{
		{"a section the copy does not read", validDocument + "ca:\n  out_dir: /shared-ca/out\n"},
		{
			"a key inside a section the copy reads",
			strings.Replace(validDocument, "listen:\n", "listen:\n  connect: 127.0.0.1:3128\n", 1),
		},
		// The proxy reads an endpoint's host, port, path and resource from its
		// url. It refuses each of them as a key.
		{"endpoint host", strings.Replace(validDocument, endpoint, endpoint+"    host: api.github.com\n", 1)},
		{"endpoint port", strings.Replace(validDocument, endpoint, endpoint+"    port: 443\n", 1)},
		{"endpoint path_prefix", strings.Replace(validDocument, endpoint, endpoint+"    path_prefix: /\n", 1)},
		{
			"endpoint resource",
			strings.Replace(validDocument, endpoint, endpoint+"    resource: https://api.github.com/\n", 1),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var doc document
			if err := yaml.UnmarshalStrict([]byte(tc.doc), &doc); err == nil {
				t.Fatal("a key outside the mirror was accepted")
			}
		})
	}
}

// TestEndpointResource checks the canonical resource the copy computes from
// an endpoint URL. The zone matches a Resource identifier against it as an
// exact string.
func TestEndpointResource(t *testing.T) {
	t.Parallel()
	cases := []struct{ url, want string }{
		{githubAPIURL + "/", githubAPIURL},
		{"https://GitHub.com./", "https://github.com"},
		{githubAPIURL + ":443", githubAPIURL},
		{"https://example.com:80/", "https://example.com:80"},
		{"http://example.com:80/", "http://example.com"},
		{openRouterURL, openRouterURL},
		{"https://registry.internal:5000/v2/", "https://registry.internal:5000/v2/"},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()
			u, problem := parseEndpointURL(tc.url)
			if problem != "" {
				t.Fatalf("url %q is refused: %s", tc.url, problem)
			}
			if got := u.resource(); got != tc.want {
				t.Errorf("resource = %q, want %q", got, tc.want)
			}
		})
	}
}
