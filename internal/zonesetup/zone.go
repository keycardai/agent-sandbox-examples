package zonesetup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/keycardai/keycard-go"
	"github.com/keycardai/keycard-go/option"
)

const (
	maxTokenResponseBytes = 1 << 20
	requestTimeout        = 30 * time.Second
	zonePageSize          = 100
)

// ResolveZone authenticates as the organization service account and returns
// the zone the values file names by zone_url, zone_id or both. It writes
// nothing to the zone. Every error wraps ErrAuth, ErrAPI, ErrZoneNotFound,
// ErrZoneAmbiguous or ErrZoneMismatch.
func ResolveZone(ctx context.Context, values *Values, secrets *Secrets) (*keycard.Zone, error) {
	httpClient, err := newHTTPClient()
	if err != nil {
		return nil, err
	}
	token, err := serviceAccountToken(ctx, httpClient, values, secrets.ManagementClientSecret)
	if err != nil {
		return nil, err
	}
	// NewZoneService reads no KEYCARD_API_* variable, unlike keycard.NewClient.
	zones := keycard.NewZoneService(
		option.WithBaseURL(values.ManagementAPIURL),
		option.WithHTTPClient(httpClient),
		option.WithAPIKey(token),
		option.WithMaxRetries(0),
	)
	if values.ZoneID != "" {
		return zoneByID(ctx, &zones, values)
	}
	return zoneByURL(ctx, &zones, values.ZoneURL)
}

// newHTTPClient returns the client for the management API. NEVER reuse a
// connection or speak HTTP/2. net/http resends a request whose reused
// connection closes before the response, and HTTP/2 resends a refused stream.
func newHTTPClient() (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("%w: the default HTTP transport is not an *http.Transport", ErrAPI)
	}
	transport = transport.Clone()
	transport.DisableKeepAlives = true
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	// Clone preserves h2 in the TLS configuration.
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return &http.Client{Timeout: requestTimeout, CheckRedirect: refuseRedirect, Transport: transport}, nil
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return errRedirect
}

// requestError returns what a failed request may safely report. A response
// body can repeat the bearer token and a redirect target can carry a
// credential, so neither reaches the caller.
func requestError(err error) error {
	var apiErr *keycard.Error
	var opErr *net.OpError
	var urlErr *url.Error
	switch {
	case errors.Is(err, errRedirect):
		return errRedirect
	case errors.As(err, &apiErr):
		return fmt.Errorf("%w %d", errStatus, apiErr.StatusCode)
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &urlErr) && urlErr.Timeout():
		return context.DeadlineExceeded
	case errors.As(err, &opErr):
		return opErr
	default:
		return errUnreadable
	}
}

var (
	errRedirect   = errors.New("refused a redirect")
	errStatus     = errors.New("returned HTTP status")
	errUnreadable = errors.New("the request failed or its response could not be read")
)

// keycard-go sends this exchange to https://api.keycard.ai whatever the base
// URL, so the exchange happens here.
func serviceAccountToken(
	ctx context.Context,
	httpClient *http.Client,
	values *Values,
	secret string,
) (string, error) {
	base, err := url.Parse(values.ManagementAPIURL)
	if err != nil {
		return "", fmt.Errorf("%w: management_api_url: %w", ErrAuth, err)
	}
	body := strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode())
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, base.JoinPath("service-account-token").String(), body,
	)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAuth, err)
	}
	req.SetBasicAuth(values.ManagementServiceAccount.ClientID, secret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAuth, requestError(err))
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: the token endpoint returned HTTP %d for client %s. "+
			"The client must be an organization service account",
			ErrAuth, res.StatusCode, values.ManagementServiceAccount.ClientID)
	}
	return tokenFromResponse(res.Body)
}

func tokenFromResponse(body io.Reader) (string, error) {
	var payload struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxTokenResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: token response: %w", ErrAuth, err)
	}
	if len(raw) > maxTokenResponseBytes {
		return "", fmt.Errorf("%w: token response exceeds %d bytes", ErrAuth, maxTokenResponseBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&payload); err != nil {
		return "", fmt.Errorf("%w: token response: %w", ErrAuth, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("%w: token response has data after the JSON object", ErrAuth)
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("%w: token response has no access_token", ErrAuth)
	}
	// RFC 6749 section 7.1 makes the token type case-insensitive.
	if !strings.EqualFold(payload.TokenType, "Bearer") {
		return "", fmt.Errorf("%w: token response has a token_type other than Bearer", ErrAuth)
	}
	return payload.AccessToken, nil
}

func zoneByID(ctx context.Context, zones *keycard.ZoneService, values *Values) (*keycard.Zone, error) {
	zone, err := zones.Get(ctx, values.ZoneID, keycard.ZoneGetParams{})
	var apiErr *keycard.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: zone_id %s", ErrZoneNotFound, values.ZoneID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: zone %s: %w", ErrAPI, values.ZoneID, requestError(err))
	}
	if zone == nil {
		return nil, fmt.Errorf("%w: zone %s: the response holds no zone", ErrAPI, values.ZoneID)
	}
	if zone.ID != values.ZoneID {
		return nil, fmt.Errorf("%w: zone_id %s returned zone %q", ErrAPI, values.ZoneID, zone.ID)
	}
	if values.ZoneURL != "" && !sameIssuer(zone.Protocols.Oauth2.Issuer, values.ZoneURL) {
		return nil, fmt.Errorf("%w: zone_id %s issues %s, not zone_url %s",
			ErrZoneMismatch, zone.ID, zone.Protocols.Oauth2.Issuer, values.ZoneURL)
	}
	return zone, nil
}

// The list API has no issuer filter, so the match happens here.
func zoneByURL(ctx context.Context, zones *keycard.ZoneService, zoneURL string) (*keycard.Zone, error) {
	all, err := listZones(ctx, zones)
	if err != nil {
		return nil, err
	}
	var found []keycard.Zone
	for _, zone := range all {
		if sameIssuer(zone.Protocols.Oauth2.Issuer, zoneURL) {
			found = append(found, zone)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("%w: no zone the service account can list issues %s", ErrZoneNotFound, zoneURL)
	case 1:
		return &found[0], nil
	default:
		return nil, fmt.Errorf("%w: %d zones issue %s. Set zone_id", ErrZoneAmbiguous, len(found), zoneURL)
	}
}

func listZones(ctx context.Context, zones *keycard.ZoneService) ([]keycard.Zone, error) {
	var all []keycard.Zone
	after := ""
	seen := map[string]bool{}
	for {
		params := keycard.ZoneListParams{Limit: keycard.Int(zonePageSize)}
		if after != "" {
			params.After = keycard.String(after)
		}
		page, err := zones.List(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("%w: list zones: %w", ErrAPI, requestError(err))
		}
		if page == nil {
			return nil, fmt.Errorf("%w: list zones: the response holds no page", ErrAPI)
		}
		all = append(all, page.Items...)
		next := page.Pagination.AfterCursor
		if next == "" {
			return all, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("%w: list zones repeated the cursor %q", ErrAPI, next)
		}
		seen[next] = true
		after = next
	}
}

func sameIssuer(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}
