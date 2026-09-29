package zonesetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/keycardai/keycard-go"
	"github.com/keycardai/keycard-go/option"
)

const (
	// requestTimeout bounds each request to the management API.
	requestTimeout = 30 * time.Second
	// zonePageSize is how many zones each list request asks for.
	zonePageSize = 100
	// maxTokenResponseBytes bounds what is read from the token endpoint.
	maxTokenResponseBytes = 1 << 20
)

// ResolveZone authenticates as the organization service account and returns
// the zone the values file names by zone_url, zone_id or both. It writes
// nothing to the zone. Every error wraps ErrAuth, ErrAPI, ErrZoneNotFound,
// ErrZoneAmbiguous or ErrZoneMismatch.
func ResolveZone(ctx context.Context, values *Values, secrets *Secrets) (*keycard.Zone, error) {
	httpClient := &http.Client{Timeout: requestTimeout, CheckRedirect: refuseRedirect}
	token, err := serviceAccountToken(ctx, httpClient, values, secrets.ManagementClientSecret)
	if err != nil {
		return nil, err
	}
	// NewZoneService reads no KEYCARD_API_* variable, unlike keycard.NewClient.
	// A retried request can repeat a write, so every request is sent once.
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

// refuseRedirect stops every redirect, so a credential goes only to the URL
// it was sent to.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("%w to %s", errRedirect, req.URL.Redacted())
}

// errRedirect is wrapped by the ErrAuth or ErrAPI of the refused request.
var errRedirect = errors.New("refused a redirect")

// serviceAccountToken exchanges the service account's client credentials for
// a bearer token at the management API's /service-account-token endpoint. It
// sends one request. keycard-go sends this exchange to https://api.keycard.ai
// whatever the base URL, so the exchange happens here.
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
	req.SetBasicAuth(values.ServiceAccount.ClientID, secret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAuth, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: the token endpoint returned HTTP %d for client %s. "+
			"The client must be an organization service account",
			ErrAuth, res.StatusCode, values.ServiceAccount.ClientID)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	dec := json.NewDecoder(io.LimitReader(res.Body, maxTokenResponseBytes))
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
		return "", fmt.Errorf("%w: token response has token_type %q, not Bearer", ErrAuth, payload.TokenType)
	}
	return payload.AccessToken, nil
}

// zoneByID fetches the zone by id. When zone_url is also set, the zone must
// issue from it.
func zoneByID(ctx context.Context, zones *keycard.ZoneService, values *Values) (*keycard.Zone, error) {
	zone, err := zones.Get(ctx, values.ZoneID, keycard.ZoneGetParams{})
	var apiErr *keycard.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: zone_id %s", ErrZoneNotFound, values.ZoneID)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: zone %s: %w", ErrAPI, values.ZoneID, err)
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

// zoneByURL returns the single zone that issues from zoneURL. The list API
// has no issuer filter, so the match happens here.
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

// listZones walks every page of zones the service account can list. A page
// with no after cursor is the last. A cursor seen before is refused.
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
			return nil, fmt.Errorf("%w: list zones: %w", ErrAPI, err)
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

// sameIssuer compares two issuer URLs exactly, except that a trailing slash on
// either does not matter.
func sameIssuer(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}
