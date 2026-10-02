package sandbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// pageLimit is the page size of every list call, the most both the
	// management API and svc-catalog take.
	pageLimit = "100"
	// maxResponse bounds how much of a response body the client reads.
	maxResponse = 8 << 20
)

var (
	errNoLocaldevCA  = errors.New("~/.localdev/devcert.json holds no root_certificate_pem")
	errNoAccessToken = errors.New("the token response has no access_token")
	errZoneNotReady  = errors.New("the zone is not ready")
)

// Keycard calls the management API and svc-catalog as an organization service
// account.
type Keycard struct {
	base         string
	clientID     string
	clientSecret string
	http         *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
	orgID   string
	// zonePoll is how often CreateZone checks a new zone, and zoneWait how
	// long it waits for one.
	zonePoll time.Duration
	zoneWait time.Duration
}

// APIError is a non-success response from the management API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

// NewKeycard returns a client for the management API at base, trusting the
// system roots plus the localdev root CA when it is installed.
func NewKeycard(base, clientID, clientSecret string) *Keycard {
	const (
		requestTimeout = 60 * time.Second
		zonePoll       = 5 * time.Second
		zoneWait       = 2 * time.Minute
	)
	return &Keycard{
		base:         strings.TrimRight(base, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		http:         newHTTPClient(requestTimeout),
		zonePoll:     zonePoll,
		zoneWait:     zoneWait,
	}
}

// newHTTPClient trusts the system roots plus the localdev root CA when it is
// installed.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{RootCAs: trustPool(), MinVersion: tls.VersionTLS12},
		},
	}
}

func trustPool() *x509.CertPool {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if pem, err := LocaldevCA(); err == nil {
		pool.AppendCertsFromPEM([]byte(pem))
	}
	return pool
}

// LocaldevCA returns the localdev root CA from ~/.localdev/devcert.json.
func LocaldevCA() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	//nolint:gosec // The file is localdev's own, under the user's home directory.
	data, err := os.ReadFile(filepath.Join(home, ".localdev", "devcert.json"))
	if err != nil {
		return "", fmt.Errorf("read the localdev CA: %w", err)
	}
	var cert struct {
		RootCertificatePEM string `json:"root_certificate_pem"`
	}
	if err := json.Unmarshal(data, &cert); err != nil || cert.RootCertificatePEM == "" {
		return "", errNoLocaldevCA
	}
	return cert.RootCertificatePEM, nil
}

// SignIn exchanges the service account credentials for a token and responds
// with the organization it belongs to.
func (k *Keycard) SignIn(ctx context.Context) (string, error) {
	if _, err := k.bearer(ctx); err != nil {
		return "", err
	}
	return k.orgID, nil
}

// Zone is a zone in the organization.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	// Protocols carries the zone's own URL as its OAuth issuer.
	Protocols struct {
		OAuth2 struct {
			Issuer string `json:"issuer"`
		} `json:"oauth2"`
	} `json:"protocols"`
}

// URL is the zone's base URL.
func (z Zone) URL() string { return strings.TrimRight(z.Protocols.OAuth2.Issuer, "/") }

// ListZones lists the organization's zones.
func (k *Keycard) ListZones(ctx context.Context) ([]Zone, error) {
	return list[Zone](ctx, k, "/zones", url.Values{})
}

// GetZone reads one zone.
func (k *Keycard) GetZone(ctx context.Context, zoneID string) (Zone, error) {
	var zone Zone
	err := k.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID), nil, &zone)
	return zone, err
}

// CreateZone creates a zone and waits until it is ready to use. A new zone
// gets its URL first, and the policy engine knows it only once svc-iam's zone
// worker has bootstrapped it there.
func (k *Keycard) CreateZone(ctx context.Context, name string) (Zone, error) {
	var zone Zone
	body := struct {
		Name string `json:"name"`
	}{Name: name}
	if err := k.do(ctx, http.MethodPost, "/zones", body, &zone); err != nil {
		return Zone{}, err
	}
	deadline := time.Now().Add(k.zoneWait)
	for {
		// The policy engine remembers a zone it does not know for a minute,
		// so each check comes after a pause that the bootstrap usually fits in.
		if !sleep(ctx, k.zonePoll) {
			return Zone{}, fmt.Errorf("wait for zone %s: %w", zone.ID, ctx.Err())
		}
		ready, err := k.zoneReady(ctx, &zone)
		if err != nil {
			return Zone{}, fmt.Errorf("wait for zone %s: %w", zone.ID, err)
		}
		if ready {
			return zone, nil
		}
		if time.Now().After(deadline) {
			return Zone{}, fmt.Errorf("%w after %s: %s", errZoneNotReady, k.zoneWait, zone.ID)
		}
	}
}

// Package is a catalog package and its form fields.
type Package struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Form struct {
		Fields []FormField `json:"fields"`
	} `json:"form"`
}

// FormField is one field of a package's install form.
type FormField struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// Field responds with the install form's field with this name.
func (p Package) Field(name string) (FormField, bool) {
	i := slices.IndexFunc(p.Form.Fields, func(f FormField) bool { return f.Name == name })
	if i < 0 {
		return FormField{}, false
	}
	return p.Form.Fields[i], true
}

// APIKeyPackages lists the API server packages whose install form takes an
// API key.
func (k *Keycard) APIKeyPackages(ctx context.Context, zoneID string) ([]Package, error) {
	packages, err := list[Package](ctx, k,
		"/catalog/zones/"+url.PathEscape(zoneID)+"/packages", url.Values{"filters[kind]": {"api-server"}})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(packages, func(p Package) bool {
		_, ok := p.Field("api_key")
		return !ok
	}), nil
}

// Install installs a catalog package by slug and responds with its outputs.
func (k *Keycard) Install(ctx context.Context, zoneID, slug string, fields map[string]any) (map[string]any, error) {
	var resp struct {
		Outputs map[string]any `json:"outputs"`
	}
	body := struct {
		Slug   string         `json:"slug"`
		Fields map[string]any `json:"fields"`
	}{Slug: slug, Fields: fields}
	if err := k.do(ctx, http.MethodPost, "/catalog/zones/"+url.PathEscape(zoneID)+"/install", body, &resp); err != nil {
		return nil, err
	}
	if resp.Outputs == nil {
		resp.Outputs = map[string]any{}
	}
	return resp.Outputs, nil
}

// ZoneUser is a user in a zone.
type ZoneUser struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Email      string `json:"email"`
	Status     string `json:"status"`
}

// ListUsers lists the zone's users.
func (k *Keycard) ListUsers(ctx context.Context, zoneID string) ([]ZoneUser, error) {
	return list[ZoneUser](ctx, k, "/zones/"+url.PathEscape(zoneID)+"/users", url.Values{})
}

// Binding is the zone's active policy set version.
type Binding struct {
	PolicySetID        string `json:"policy_set_id"`
	PolicySetVersionID string `json:"policy_set_version_id"`
}

// ActiveBinding responds with the zone's active policy set version, and false
// when the zone has none.
func (k *Keycard) ActiveBinding(ctx context.Context, zoneID string) (Binding, bool, error) {
	bindings, err := list[Binding](ctx, k,
		"/zones/"+url.PathEscape(zoneID)+"/policy-bindings", url.Values{"filter[mode]": {"active"}})
	if err != nil || len(bindings) == 0 {
		return Binding{}, false, err
	}
	return bindings[0], true, nil
}

// Activate makes a policy set version the zone's active one.
func (k *Keycard) Activate(ctx context.Context, zoneID string, binding Binding) error {
	path := fmt.Sprintf("/zones/%s/policy-sets/%s/versions/%s",
		url.PathEscape(zoneID), url.PathEscape(binding.PolicySetID), url.PathEscape(binding.PolicySetVersionID))
	body := struct {
		Active bool `json:"active"`
	}{Active: true}
	err := k.do(ctx, http.MethodPatch, path, body, nil)
	return err
}

// PolicySet is a policy set and its latest version.
type PolicySet struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	LatestVersionID string `json:"latest_version_id"`
}

// GetPolicySet reads one policy set.
func (k *Keycard) GetPolicySet(ctx context.Context, zoneID, setID string) (PolicySet, error) {
	var set PolicySet
	err := k.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/policy-sets/"+url.PathEscape(setID), nil, &set)
	return set, err
}

// FindPolicySet finds the policy set with exactly this name. The name query
// matches substrings, so the result is matched here.
func (k *Keycard) FindPolicySet(ctx context.Context, zoneID, name string) (PolicySet, bool, error) {
	sets, err := list[PolicySet](ctx, k, "/zones/"+url.PathEscape(zoneID)+"/policy-sets",
		url.Values{"query[name]": {name}})
	if err != nil {
		return PolicySet{}, false, err
	}
	i := slices.IndexFunc(sets, func(set PolicySet) bool { return set.Name == name })
	if i < 0 {
		return PolicySet{}, false, nil
	}
	return sets[i], true, nil
}

// TokenCredential is a token application credential.
type TokenCredential struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	ProviderID string `json:"provider_id"`
	Subject    string `json:"subject"`
}

// TokenCredentials lists an application's token credentials.
func (k *Keycard) TokenCredentials(ctx context.Context, zoneID, applicationID string) ([]TokenCredential, error) {
	return list[TokenCredential](ctx, k, "/zones/"+url.PathEscape(zoneID)+"/application-credentials",
		url.Values{"applicationId": {applicationID}, "filter[type]": {"token"}})
}

// CreateTokenCredential creates a token credential: a workload with a token
// from the provider, with the subject, authenticates as the application.
func (k *Keycard) CreateTokenCredential(
	ctx context.Context, zoneID, applicationID, providerID, subject string,
) (TokenCredential, error) {
	var cred TokenCredential
	body := struct {
		ApplicationID string `json:"application_id"`
		Type          string `json:"type"`
		ProviderID    string `json:"provider_id"`
		Subject       string `json:"subject"`
	}{ApplicationID: applicationID, Type: "token", ProviderID: providerID, Subject: subject}
	err := k.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/application-credentials", body, &cred)
	return cred, err
}

// PolicyExists reports whether the zone has an unarchived policy with exactly
// this name. The name query matches substrings, so the result is matched here.
func (k *Keycard) PolicyExists(ctx context.Context, zoneID, name string) (bool, error) {
	type policy struct {
		Name       string  `json:"name"`
		ArchivedAt *string `json:"archived_at"`
	}
	policies, err := list[policy](ctx, k, "/zones/"+url.PathEscape(zoneID)+"/policies",
		url.Values{"query[name]": {name}})
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(policies, func(p policy) bool { return p.Name == name && p.ArchivedAt == nil }), nil
}

// list follows the cursor pagination the management API and svc-catalog share,
// from the first page to the last.
func list[T any](ctx context.Context, k *Keycard, path string, query url.Values) ([]T, error) {
	query.Set("limit", pageLimit)
	var all []T
	for {
		var page struct {
			Items      []T `json:"items"`
			Pagination struct {
				AfterCursor string `json:"after_cursor"`
			} `json:"pagination"`
		}
		if err := k.do(ctx, http.MethodGet, path+"?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.Pagination.AfterCursor == "" {
			return all, nil
		}
		query.Set("after", page.Pagination.AfterCursor)
	}
}

// zoneReady reports whether the zone has its URL and the policy engine knows
// it, reading the zone again until it has the URL.
func (k *Keycard) zoneReady(ctx context.Context, zone *Zone) (bool, error) {
	if zone.URL() == "" {
		got, err := k.GetZone(ctx, zone.ID)
		if err != nil {
			return false, err
		}
		if *zone = got; zone.URL() == "" {
			return false, nil
		}
	}
	_, _, err := k.ActiveBinding(ctx, zone.ID)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// bearer responds with a service account token, signing in again a minute
// before the current one expires.
func (k *Keycard) bearer(ctx context.Context) (string, error) {
	const refreshMargin = time.Minute
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Until(k.expires) > refreshMargin {
		return k.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.base+"/service-account-token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(k.clientID, k.clientSecret)
	status, body, err := k.send(req)
	if err != nil {
		return "", fmt.Errorf("sign in as the service account: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("sign in as the service account: %w", apiError(status, body))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("sign in as the service account: %w", errNoAccessToken)
	}
	k.token = tok.AccessToken
	k.expires = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	k.orgID = orgFromToken(tok.AccessToken)
	return k.token, nil
}

// do sends a JSON request to the management API and decodes the response.
func (k *Keycard) do(ctx context.Context, method, path string, in, out any) error {
	token, err := k.bearer(ctx)
	if err != nil {
		return err
	}
	req, err := jsonRequest(ctx, method, k.base+path, in)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	status, body, err := k.send(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	if status >= http.StatusMultipleChoices {
		return fmt.Errorf("%s %s: %w", method, path, apiError(status, body))
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return nil
}

// jsonRequest builds a request whose body, when there is one, is payload as
// JSON.
func jsonRequest(ctx context.Context, method, target string, payload any) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// send sends the request and reads the response body.
func (k *Keycard) send(req *http.Request) (int, []byte, error) {
	resp, err := k.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("reach %s: %w", k.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return 0, nil, fmt.Errorf("read the response: %w", err)
	}
	return resp.StatusCode, body, nil
}

func apiError(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Message != "" {
		msg = parsed.Message
	}
	return &APIError{Status: status, Message: msg}
}

// orgFromToken reads the organization claim of the service account token.
func orgFromToken(token string) string {
	const jwtParts = 3
	parts := strings.Split(token, ".")
	if len(parts) != jwtParts {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	org, _ := claims["https://api.keycard.sh/organization_id"].(string)
	return org
}
