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
	"github.com/keycardai/keycard-go/packages/param"
)

const (
	maxTokenResponseBytes = 1 << 20
	requestTimeout        = 30 * time.Second
	pageSize              = 100
)

// Session is one authenticated run against the zone the values file names.
type Session struct {
	Zone  *keycard.Zone
	zones keycard.ZoneService
}

// Connect authenticates as the organization service account and resolves the
// zone the values file names by zone_url, zone_id or both. It writes nothing
// to the zone. Every error wraps ErrAuth, ErrAPI, ErrZoneNotFound,
// ErrZoneAmbiguous or ErrZoneMismatch.
func Connect(ctx context.Context, values *Values, secrets *Secrets) (*Session, error) {
	httpClient := &http.Client{Timeout: requestTimeout, CheckRedirect: refuseRedirect}
	token, err := serviceAccountToken(ctx, httpClient, values, secrets.ManagementClientSecret)
	if err != nil {
		return nil, err
	}
	// NewZoneService reads no KEYCARD_API_* variable, unlike keycard.NewClient.
	session := &Session{zones: keycard.NewZoneService(
		option.WithBaseURL(values.ManagementAPIURL),
		option.WithHTTPClient(httpClient),
		option.WithAPIKey(token),
		option.WithMaxRetries(0),
	)}
	if values.ZoneID != "" {
		session.Zone, err = zoneByID(ctx, &session.zones, values)
	} else {
		session.Zone, err = zoneByURL(ctx, &session.zones, values.ZoneURL)
	}
	if err != nil {
		return nil, err
	}
	return session, nil
}

// FindProvider returns the id of the zone's provider whose OAuth issuer is
// issuerURL, byte for byte, because svc-sts compares a token's iss to it
// exactly. It returns "" when no provider matches. Every error wraps ErrAPI,
// ErrProviderAmbiguous or ErrProviderMismatch.
func (s *Session) FindProvider(ctx context.Context, issuerURL string) (string, error) {
	all, err := s.listProviders(ctx)
	if err != nil {
		return "", err
	}
	var found []keycard.Provider
	for _, provider := range all {
		switch issuer := provider.Protocols.Oauth2.Issuer; {
		case issuer == issuerURL:
			found = append(found, provider)
		case provider.Identifier == issuerURL:
			return "", fmt.Errorf("%w: provider %s has the identifier %s but the issuer %s",
				ErrProviderMismatch, provider.ID, issuerURL, issuer)
		}
	}
	switch {
	case len(found) > 1:
		return "", fmt.Errorf("%w: %d providers have the issuer %s", ErrProviderAmbiguous, len(found), issuerURL)
	case len(found) == 1 && found[0].ID == "":
		return "", fmt.Errorf("%w: the provider with the issuer %s has no id", ErrAPI, issuerURL)
	case len(found) == 1:
		return found[0].ID, nil
	default:
		return "", nil
	}
}

// CreateProvider creates an issuer-only provider for issuerURL. svc-sts
// discovers the JWKS from the issuer, so the provider holds no jwks_uri and no
// client credential. When the create conflicts with a provider a concurrent
// run made, it returns that provider's id.
func (s *Session) CreateProvider(ctx context.Context, issuerURL string) (string, error) {
	parsed, err := url.Parse(issuerURL)
	if err != nil {
		return "", fmt.Errorf("%w: issuer_url is not a URL", ErrValues)
	}
	provider, err := s.zones.Providers.New(ctx, s.Zone.ID, keycard.ZoneProviderNewParams{
		Identifier:  issuerURL,
		Name:        "Kubernetes cluster " + parsed.Host,
		Description: keycard.String("OIDC issuer of the Kubernetes cluster that runs the sandboxes"),
		Protocols: keycard.ZoneProviderNewParamsProtocols{
			Oauth2: keycard.ZoneProviderNewParamsProtocolsOauth2{Issuer: keycard.String(issuerURL)},
		},
	})
	var apiErr *keycard.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
		if id, findErr := s.FindProvider(ctx, issuerURL); findErr != nil || id != "" {
			return id, findErr
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w: create provider: %w", ErrAPI, requestError(err))
	}
	if provider == nil || provider.ID == "" {
		return "", fmt.Errorf("%w: create provider: the response holds no provider id", ErrAPI)
	}
	return provider.ID, nil
}

// ApplicationIdentifier is the identifier of the egress proxy Application for
// the sandboxes in namespace.
func ApplicationIdentifier(namespace string) string {
	return "urn:keycard:sandbox-proxy:" + namespace
}

// FindApplication returns the zone's application whose identifier is
// identifier. Its ID is "" when no application matches. Every error wraps
// ErrAPI or ErrApplicationAmbiguous.
func (s *Session) FindApplication(ctx context.Context, identifier string) (keycard.Application, error) {
	all, err := s.listApplications(ctx, identifier)
	if err != nil {
		return keycard.Application{}, err
	}
	var found []keycard.Application
	for _, app := range all {
		if app.Identifier == identifier {
			found = append(found, app)
		}
	}
	switch {
	case len(found) > 1:
		return keycard.Application{}, fmt.Errorf("%w: %d applications have the identifier %s",
			ErrApplicationAmbiguous, len(found), identifier)
	case len(found) == 1 && found[0].ID == "":
		return keycard.Application{}, fmt.Errorf("%w: the application %s has no id", ErrAPI, identifier)
	case len(found) == 1:
		return found[0], nil
	default:
		return keycard.Application{}, nil
	}
}

// CreateApplication creates the egress proxy Application for the sandboxes in
// namespace with implicit consent, and returns its id.
func (s *Session) CreateApplication(ctx context.Context, identifier, namespace string) (string, error) {
	app, err := s.zones.Applications.New(ctx, s.Zone.ID, keycard.ZoneApplicationNewParams{
		Identifier: identifier,
		Name:       "Keycard egress proxy",
		Description: keycard.String(
			"Egress proxy sidecar that mints credentials for sandboxes in namespace " + namespace,
		),
		Consent: keycard.ZoneApplicationNewParamsConsentImplicit,
	})
	if err != nil {
		return "", fmt.Errorf("%w: create application: %w", ErrAPI, requestError(err))
	}
	if app == nil || app.ID == "" {
		return "", fmt.Errorf("%w: create application: the response holds no application id", ErrAPI)
	}
	return app.ID, nil
}

// SetApplicationConsentImplicit sets the application's consent to implicit.
// The proxy acts for an absent user, so nobody can answer a consent prompt.
func (s *Session) SetApplicationConsentImplicit(ctx context.Context, applicationID string) error {
	app, err := s.zones.Applications.Update(ctx, applicationID, keycard.ZoneApplicationUpdateParams{
		ZoneID:  s.Zone.ID,
		Consent: keycard.ZoneApplicationUpdateParamsConsentImplicit,
	})
	if err != nil {
		return fmt.Errorf("%w: update application %s: %w", ErrAPI, applicationID, requestError(err))
	}
	if app == nil || app.ID != applicationID {
		return fmt.Errorf("%w: update application %s: the response holds another application or none",
			ErrAPI, applicationID)
	}
	return nil
}

func (s *Session) listApplications(ctx context.Context, identifier string) ([]keycard.Application, error) {
	return listAll("applications", func(params pageParams) (listPage[keycard.Application], error) {
		page, err := s.zones.Applications.List(ctx, s.Zone.ID, keycard.ZoneApplicationListParams{
			After: params.After, Limit: params.Limit,
			FilterIdentifier: keycard.ZoneApplicationListParamsFilterIdentifierUnion{
				OfString: keycard.String(identifier),
			},
		})
		if err != nil || page == nil {
			return listPage[keycard.Application]{}, err //nolint:wrapcheck // listAll passes it through requestError.
		}
		return listPage[keycard.Application]{
			items: page.Items, after: page.Pagination.AfterCursor,
			complete: page.JSON.Items.Valid() && page.JSON.Pagination.Valid(),
		}, nil
	})
}

func (s *Session) listProviders(ctx context.Context) ([]keycard.Provider, error) {
	return listAll("providers", func(params pageParams) (listPage[keycard.Provider], error) {
		page, err := s.zones.Providers.List(ctx, s.Zone.ID, keycard.ZoneProviderListParams{
			After: params.After, Limit: params.Limit,
		})
		if err != nil || page == nil {
			return listPage[keycard.Provider]{}, err //nolint:wrapcheck // listAll passes it through requestError.
		}
		return listPage[keycard.Provider]{
			items: page.Items, after: page.Pagination.AfterCursor,
			complete: page.JSON.Items.Valid() && page.JSON.Pagination.Valid(),
		}, nil
	})
}

// refuseRedirect stops every redirect. net/http keeps the Authorization header
// on a redirect to the same host, even from https to http.
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
	errNoPage     = errors.New("the response holds no complete page")
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
	return listAll("zones", func(params pageParams) (listPage[keycard.Zone], error) {
		page, err := zones.List(ctx, keycard.ZoneListParams{After: params.After, Limit: params.Limit})
		if err != nil || page == nil {
			return listPage[keycard.Zone]{}, err //nolint:wrapcheck // listAll passes it through requestError.
		}
		return listPage[keycard.Zone]{
			items: page.Items, after: page.Pagination.AfterCursor,
			complete: page.JSON.Items.Valid() && page.JSON.Pagination.Valid(),
		}, nil
	})
}

type pageParams struct {
	After param.Opt[string]
	Limit param.Opt[int64]
}

// keycard-go decodes a null page, or one without items or pagination, without
// an error.
type listPage[T any] struct {
	items    []T
	after    string
	complete bool
}

func listAll[T any](name string, request func(pageParams) (listPage[T], error)) ([]T, error) {
	var all []T
	params := pageParams{Limit: keycard.Int(pageSize)}
	seen := map[string]bool{}
	for {
		page, err := request(params)
		if err != nil {
			return nil, fmt.Errorf("%w: list %s: %w", ErrAPI, name, requestError(err))
		}
		if !page.complete {
			return nil, fmt.Errorf("%w: list %s: %w", ErrAPI, name, errNoPage)
		}
		all = append(all, page.items...)
		if page.after == "" {
			return all, nil
		}
		if seen[page.after] {
			return nil, fmt.Errorf("%w: list %s repeated a cursor", ErrAPI, name)
		}
		seen[page.after] = true
		params.After = keycard.String(page.after)
	}
}

func sameIssuer(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}
