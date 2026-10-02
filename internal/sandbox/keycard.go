package sandbox

import (
	"bytes"
	"context"
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
	"strings"
	"sync"
	"time"
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
}

// APIError is a non-success response from the management API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

// IsStatus reports whether err is an APIError with this status.
func IsStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

// NewKeycard returns a client for the management API at base, trusting the
// system roots plus the localdev root CA when it is installed.
func NewKeycard(base, clientID, clientSecret string) (*Keycard, error) {
	pool, err := trustPool()
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig.RootCAs = pool
	return &Keycard{
		base:         strings.TrimRight(base, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		http:         &http.Client{Transport: transport, Timeout: 60 * time.Second},
	}, nil
}

// trustPool is the system roots plus the localdev root CA.
func trustPool() (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if pem, err := LocaldevCA(); err == nil {
		pool.AppendCertsFromPEM([]byte(pem))
	}
	return pool, nil
}

// LocaldevCA returns the localdev root CA from ~/.localdev/devcert.json.
func LocaldevCA() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".localdev", "devcert.json")) //nolint:gosec // localdev's own file
	if err != nil {
		return "", fmt.Errorf("read the localdev CA: %w", err)
	}
	var cert struct {
		RootCertificatePEM string `json:"root_certificate_pem"`
	}
	if err := json.Unmarshal(b, &cert); err != nil || cert.RootCertificatePEM == "" {
		return "", errors.New("~/.localdev/devcert.json holds no root_certificate_pem")
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

func (k *Keycard) bearer(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Until(k.expires) > time.Minute {
		return k.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		k.base+"/service-account-token",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(k.clientID, k.clientSecret)
	resp, err := k.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach %s: %w", k.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sign in as the service account: %w", apiError(resp.StatusCode, b))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &tok); err != nil || tok.AccessToken == "" {
		return "", errors.New("sign in as the service account: no access_token in the response")
	}
	k.token = tok.AccessToken
	k.expires = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	k.orgID = orgFromToken(tok.AccessToken)
	return k.token, nil
}

// orgFromToken reads the organization claim of the service account token.
func orgFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
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

// do sends a JSON request to the management API and decodes the response.
func (k *Keycard) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	token, err := k.bearer(ctx)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %w", method, path, apiError(resp.StatusCode, b))
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return nil, fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return resp.Header, nil
}

func apiError(status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		msg = e.Message
	}
	return &APIError{Status: status, Message: msg}
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
	var page struct {
		Items []Zone `json:"items"`
	}
	if _, err := k.do(ctx, http.MethodGet, "/zones?limit=100", nil, &page); err != nil {
		return nil, err
	}
	return page.Items, nil
}

// GetZone reads one zone.
func (k *Keycard) GetZone(ctx context.Context, zoneID string) (Zone, error) {
	var z Zone
	_, err := k.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID), nil, &z)
	return z, err
}

// CreateZone creates a zone. A new zone bootstraps asynchronously, so this
// waits until its OAuth metadata names it.
func (k *Keycard) CreateZone(ctx context.Context, name string) (Zone, error) {
	var z Zone
	if _, err := k.do(ctx, http.MethodPost, "/zones", map[string]string{"name": name}, &z); err != nil {
		return Zone{}, err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for z.URL() == "" && time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if got, err := k.GetZone(ctx, z.ID); err == nil {
			z = got
		}
	}
	if z.URL() == "" {
		return Zone{}, fmt.Errorf("zone %s has no URL after two minutes", z.ID)
	}
	return z, nil
}

// Package is a catalog package and its form fields.
type Package struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Form struct {
		Fields []struct {
			Name  string `json:"name"`
			Label string `json:"label"`
		} `json:"fields"`
	} `json:"form"`
}

// FieldLabel is the label of the install form's field, or "" without one.
func (p Package) FieldLabel(name string) string {
	for _, f := range p.Form.Fields {
		if f.Name == name {
			return f.Label
		}
	}
	return ""
}

// HasField reports whether the package's install form has the field.
func (p Package) HasField(name string) bool {
	for _, f := range p.Form.Fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// APIKeyPackages lists the API server packages whose install form takes an
// API key.
func (k *Keycard) APIKeyPackages(ctx context.Context, zoneID string) ([]Package, error) {
	var all []Package
	q := url.Values{"filters[kind]": {"api-server"}, "limit": {"100"}}
	for {
		var page struct {
			Items      []Package `json:"items"`
			Pagination struct {
				AfterCursor string `json:"after_cursor"`
			} `json:"pagination"`
		}
		path := "/catalog/zones/" + url.PathEscape(zoneID) + "/packages?" + q.Encode()
		if _, err := k.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		for _, p := range page.Items {
			if p.HasField("api_key") {
				all = append(all, p)
			}
		}
		if page.Pagination.AfterCursor == "" {
			return all, nil
		}
		q.Set("after", page.Pagination.AfterCursor)
	}
}

// Install installs a catalog package by slug and responds with its outputs.
func (k *Keycard) Install(ctx context.Context, zoneID, slug string, fields map[string]any) (map[string]any, error) {
	var resp struct {
		Outputs map[string]any `json:"outputs"`
	}
	body := map[string]any{"slug": slug, "fields": fields}
	if _, err := k.do(
		ctx,
		http.MethodPost,
		"/catalog/zones/"+url.PathEscape(zoneID)+"/install",
		body,
		&resp,
	); err != nil {
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
	var all []ZoneUser
	q := url.Values{"limit": {"100"}}
	for {
		var page struct {
			Items      []ZoneUser `json:"items"`
			Pagination struct {
				AfterCursor string `json:"after_cursor"`
			} `json:"pagination"`
		}
		if _, err := k.do(
			ctx,
			http.MethodGet,
			"/zones/"+url.PathEscape(zoneID)+"/users?"+q.Encode(),
			nil,
			&page,
		); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.Pagination.AfterCursor == "" {
			return all, nil
		}
		q.Set("after", page.Pagination.AfterCursor)
	}
}

// Binding is the zone's active policy set version.
type Binding struct {
	PolicySetID        string `json:"policy_set_id"`
	PolicySetVersionID string `json:"policy_set_version_id"`
}

// ActiveBinding responds with the zone's active policy set version.
func (k *Keycard) ActiveBinding(ctx context.Context, zoneID string) (*Binding, error) {
	var page struct {
		Items []Binding `json:"items"`
	}
	if _, err := k.do(
		ctx,
		http.MethodGet,
		"/zones/"+url.PathEscape(zoneID)+"/policy-bindings?filter%5Bmode%5D=active",
		nil,
		&page,
	); err != nil {
		return nil, err
	}
	if len(page.Items) == 0 {
		return nil, nil
	}
	return &page.Items[0], nil
}

// Activate makes a policy set version the zone's active one.
func (k *Keycard) Activate(ctx context.Context, zoneID string, b Binding) error {
	path := fmt.Sprintf("/zones/%s/policy-sets/%s/versions/%s",
		url.PathEscape(zoneID), url.PathEscape(b.PolicySetID), url.PathEscape(b.PolicySetVersionID))
	_, err := k.do(ctx, http.MethodPatch, path, map[string]bool{"active": true}, nil)
	return err
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
	var all []TokenCredential
	q := url.Values{"applicationId": {applicationID}, "filter[type]": {"token"}, "limit": {"100"}}
	for {
		var page struct {
			Items      []TokenCredential `json:"items"`
			Pagination struct {
				AfterCursor string `json:"after_cursor"`
			} `json:"pagination"`
		}
		path := "/zones/" + url.PathEscape(zoneID) + "/application-credentials?" + q.Encode()
		if _, err := k.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.Pagination.AfterCursor == "" {
			return all, nil
		}
		q.Set("after", page.Pagination.AfterCursor)
	}
}

// CreateTokenCredential creates a token credential: a workload with a token
// from the provider, with the subject, authenticates as the application.
func (k *Keycard) CreateTokenCredential(
	ctx context.Context,
	zoneID, applicationID, providerID, subject string,
) (TokenCredential, error) {
	var cred TokenCredential
	body := map[string]string{
		"application_id": applicationID,
		"type":           "token",
		"provider_id":    providerID,
		"subject":        subject,
	}
	_, err := k.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/application-credentials", body, &cred)
	return cred, err
}

// PolicyExists reports whether the zone has an unarchived policy with exactly
// this name.
func (k *Keycard) PolicyExists(ctx context.Context, zoneID, name string) (bool, error) {
	var page struct {
		Items []struct {
			Name       string  `json:"name"`
			ArchivedAt *string `json:"archived_at"`
		} `json:"items"`
	}
	q := url.Values{"query[name]": {name}, "limit": {"100"}}
	if _, err := k.do(
		ctx,
		http.MethodGet,
		"/zones/"+url.PathEscape(zoneID)+"/policies?"+q.Encode(),
		nil,
		&page,
	); err != nil {
		return false, err
	}
	for _, p := range page.Items {
		if p.Name == name && p.ArchivedAt == nil {
			return true, nil
		}
	}
	return false, nil
}
