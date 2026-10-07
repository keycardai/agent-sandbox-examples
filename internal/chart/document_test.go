package chart_test

import (
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
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
// netsetup) and every key of listen except transparent_port. Add a section or
// key here when the chart sets it.

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

// listenDoc is the frontend the capture redirects to.
type listenDoc struct {
	TransparentPort int `json:"transparent_port"`
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
	Host       string      `json:"host"`
	Port       int         `json:"port"`
	PathPrefix string      `json:"path_prefix"`
	Resource   string      `json:"resource"`
	Credential *credential `json:"credential,omitempty"`
}

type credential struct {
	Placement placement `json:"placement"`
	Env       string    `json:"env"`
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
	defaultPort     = 443
)

var (
	envNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

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
	seen := map[string]int{}
	for i, e := range doc.Endpoints {
		found = append(found, e.problems(i)...)
		key := strings.ToLower(e.Host) + e.prefix()
		if previous, repeat := seen[key]; repeat {
			found = append(found, fmt.Sprintf("endpoints[%d] repeats the host and path prefix of endpoints[%d]",
				i, previous))
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

// problems requires listen.transparent_port. The proxy accepts 0 and then
// does not run a transparent frontend. A sandbox pod always captures traffic,
// so the chart must set the port.
func (l listenDoc) problems() []string {
	if l.TransparentPort < 1 || l.TransparentPort > maxPort {
		return []string{fmt.Sprintf("listen.transparent_port is %d, outside 1-%d", l.TransparentPort, maxPort)}
	}
	return nil
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

// prefix returns the path prefix the proxy matches, or "/" when it is unset.
func (e endpoint) prefix() string {
	if e.PathPrefix == "" {
		return "/"
	}
	return e.PathPrefix
}

// resource returns the exchange's resource parameter, or https://<host>/
// when it is unset.
func (e endpoint) resource() string {
	if e.Resource == "" {
		return "https://" + strings.ToLower(e.Host) + "/"
	}
	return e.Resource
}

// port returns the endpoint's port, or the HTTPS port when it is unset.
func (e endpoint) port() int {
	if e.Port == 0 {
		return defaultPort
	}
	return e.Port
}

func (e endpoint) problems(i int) []string {
	found := e.hostProblems(i)
	if port := e.port(); port < 1 || port > maxPort {
		found = append(found, fmt.Sprintf("endpoints[%d]: port %d is out of range", i, port))
	}
	if prefix := e.prefix(); !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "?#") ||
		!shortestForm(prefix) {
		found = append(found, fmt.Sprintf(
			"endpoints[%d]: path_prefix %q must be an absolute path in its shortest form", i, prefix,
		))
	}
	found = append(found, e.resourceProblems(i)...)
	if e.Credential != nil {
		found = append(found, e.Credential.problems(i)...)
	}
	return found
}

func (e endpoint) resourceProblems(i int) []string {
	resource, err := url.Parse(e.resource())
	switch {
	case err != nil || resource.Scheme != "https" || resource.Host == "":
		return []string{fmt.Sprintf("endpoints[%d]: resource %q must be an absolute https URL", i, e.resource())}
	case resource.User != nil || resource.RawQuery != "" || resource.Fragment != "":
		return []string{fmt.Sprintf(
			"endpoints[%d]: resource %q has userinfo, a query or a fragment", i, e.resource(),
		)}
	}
	return nil
}

// hostProblems checks that the name is valid as a TLS SNI host name: ASCII
// labels, and a last label that is not all digits. The digit rule refuses an
// address written as 0177.0.0.1.
func (e endpoint) hostProblems(i int) []string {
	host := strings.ToLower(e.Host)
	if host == "" {
		return []string{fmt.Sprintf("endpoints[%d]: host is empty", i)}
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

func (c credential) problems(i int) []string {
	var found []string
	if !envNameRE.MatchString(c.Env) {
		found = append(found, fmt.Sprintf("endpoints[%d]: credential.env %q is not a variable name", i, c.Env))
	}
	return append(found, c.Placement.problems(i)...)
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

// shortestForm reports whether path.Clean leaves the prefix unchanged, apart
// from one trailing slash.
func shortestForm(prefix string) bool {
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
policy:
  cluster_cidrs: ["10.0.0.0/16"]
  deny_list_exceptions: []
endpoints:
  - host: api.github.com
    resource: https://api.github.com/
    credential:
      placement: {kind: header, name: Authorization, template: "Bearer {credential}"}
      env: GITHUB_TOKEN
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
			strings.Replace(validDocument, "listen:\n  transparent_port: 1015\n", "", 1),
			"listen.transparent_port is 0, outside 1-65535",
		},
		{
			"exception uncovering the metadata address",
			strings.Replace(validDocument, "deny_list_exceptions: []", `deny_list_exceptions: ["169.254.0.0/16"]`, 1),
			"covers 169.254.169.254",
		},
		{
			"host is an IP literal",
			strings.Replace(validDocument, "host: api.github.com", "host: 10.0.0.1", 1),
			"is an IP literal",
		},
		{
			"host ends in an all-digit label",
			strings.Replace(validDocument, "host: api.github.com", `host: "2852039166"`, 1),
			"ends in an all-digit label",
		},
		{
			"path prefix has dot segments",
			strings.Replace(validDocument, "    resource:", "    path_prefix: /api/../secret\n    resource:", 1),
			"shortest form",
		},
		{
			"resource has a query",
			strings.Replace(validDocument, "resource: https://api.github.com/", "resource: https://api.github.com/?a=1", 1),
			"userinfo, a query or a fragment",
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
			"env is not a variable name",
			strings.Replace(validDocument, "env: GITHUB_TOKEN", "env: 1TOKEN", 1),
			"is not a variable name",
		},
		{
			"relative upstream CA file",
			validDocument + "tls:\n  upstream_ca_files: [zone-ca.pem]\n",
			`tls.upstream_ca_files[0] "zone-ca.pem" must be an absolute path`,
		},
		{
			"repeated host and prefix",
			validDocument + "  - host: api.github.com\n",
			"repeats the host and path prefix",
		},
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
// read is an error. The copy reads one key of listen, so an unknown key
// inside listen is a separate case.
func TestDocumentMirrorRefusesUnknownKey(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, doc string }{
		{"a section the copy does not read", validDocument + "ca:\n  out_dir: /shared-ca/out\n"},
		{
			"a key inside a section the copy reads",
			strings.Replace(validDocument, "listen:\n", "listen:\n  connect: 127.0.0.1:3128\n", 1),
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
