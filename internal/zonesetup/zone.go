package zonesetup

import (
	"bytes"
	"context"
	"crypto/x509"
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
	"github.com/keycardai/keycard-go/packages/param"
)

const (
	maxTokenResponseBytes = 1 << 20
	requestTimeout        = 30 * time.Second
	pageSize              = 100
	tokenCredential       = "token"
)

// Session is one authenticated run against the zone the values file names.
type Session struct {
	Zone            *keycard.Zone
	zones           keycard.ZoneService
	vaultProviderID string
}

// Connect authenticates as the organization service account and resolves the
// zone the values file names by zone_url, zone_id or both. It trusts roots,
// or the system roots if roots is nil. It writes nothing to the zone. Every
// error wraps ErrAuth, ErrAPI, ErrZoneNotFound, ErrZoneAmbiguous or
// ErrZoneMismatch.
func Connect(ctx context.Context, values *Values, secrets *Secrets, roots *x509.CertPool) (*Session, error) {
	org, err := Login(ctx, values.ManagementAPIURL, values.ManagementServiceAccount.ClientID,
		secrets.ManagementClientSecret, roots)
	if err != nil {
		return nil, err
	}
	zone, err := org.Zone(ctx, values)
	if err != nil {
		return nil, err
	}
	return org.Session(zone), nil
}

// FindProvider returns the id of the zone's provider whose OAuth issuer is
// issuerURL, byte for byte, because the zone compares a token's iss to it
// exactly. It returns "" when no provider matches. Every error wraps ErrAPI,
// ErrProviderAmbiguous or ErrProviderMismatch.
func (s *Session) FindProvider(ctx context.Context, issuerURL string) (string, error) {
	all, err := s.listProviders(ctx, keycard.ZoneProviderListParams{})
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

// CreateProvider creates an issuer-only provider for issuerURL. The zone
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

// ApplicationName is the name CreateApplication gives the egress proxy
// Application.
const ApplicationName = "Sandbox Egress Proxy"

// CreateApplication creates the egress proxy Application for the sandboxes in
// namespace with implicit consent, and returns its id.
func (s *Session) CreateApplication(ctx context.Context, identifier, namespace string) (string, error) {
	app, err := s.zones.Applications.New(ctx, s.Zone.ID, keycard.ZoneApplicationNewParams{
		Identifier: identifier,
		Name:       ApplicationName,
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

// ResourceProvider returns the id of the provider that serves the resource's
// credential: the provider whose identifier is credential_provider, or the
// zone's keycard-vault provider for a static token. Every error wraps ErrAPI,
// ErrProviderAmbiguous or ErrProviderNotFound.
func (s *Session) ResourceProvider(ctx context.Context, resource ResourceValues) (string, error) {
	if resource.CredentialProvider == "" {
		return s.vaultProvider(ctx)
	}
	identifier := resource.CredentialProvider
	all, err := s.listProviders(ctx, keycard.ZoneProviderListParams{
		FilterIdentifier: keycard.ZoneProviderListParamsFilterIdentifierUnion{OfString: keycard.String(identifier)},
	})
	if err != nil {
		return "", err
	}
	return oneProvider(all, func(p keycard.Provider) bool { return p.Identifier == identifier },
		"the identifier "+identifier)
}

func oneProvider(all []keycard.Provider, match func(keycard.Provider) bool, what string) (string, error) {
	var found []keycard.Provider
	for _, provider := range all {
		if match(provider) {
			found = append(found, provider)
		}
	}
	switch {
	case len(found) > 1:
		return "", fmt.Errorf("%w: %d providers have %s", ErrProviderAmbiguous, len(found), what)
	case len(found) == 0:
		return "", fmt.Errorf("%w: no provider has %s. Create it in the zone first", ErrProviderNotFound, what)
	case found[0].ID == "":
		return "", fmt.Errorf("%w: the provider with %s has no id", ErrAPI, what)
	default:
		return found[0].ID, nil
	}
}

// FindResource returns the zone's resource whose identifier is identifier.
// Its ID is "" when no resource matches. Every error wraps ErrAPI or
// ErrResourceAmbiguous.
func (s *Session) FindResource(ctx context.Context, identifier string) (keycard.Resource, error) {
	all, err := s.listResources(ctx, identifier)
	if err != nil {
		return keycard.Resource{}, err
	}
	var found []keycard.Resource
	for _, resource := range all {
		if resource.Identifier == identifier {
			found = append(found, resource)
		}
	}
	switch {
	case len(found) > 1:
		return keycard.Resource{}, fmt.Errorf("%w: %d resources have the identifier %s",
			ErrResourceAmbiguous, len(found), identifier)
	case len(found) == 1 && found[0].ID == "":
		return keycard.Resource{}, fmt.Errorf("%w: the resource %s has no id", ErrAPI, identifier)
	case len(found) == 1:
		return found[0], nil
	default:
		return keycard.Resource{}, nil
	}
}

// CreateResource creates the resource with its credential provider and
// returns its id.
func (s *Session) CreateResource(ctx context.Context, resource ResourceValues, providerID string) (string, error) {
	created, err := s.zones.Resources.New(ctx, s.Zone.ID, keycard.ZoneResourceNewParams{
		Identifier:           resource.Identifier,
		Name:                 resource.Name,
		Prefix:               keycard.Bool(resource.Prefix),
		CredentialProviderID: keycard.String(providerID),
	})
	if err != nil {
		return "", fmt.Errorf("%w: create resource %s: %w", ErrAPI, resource.Identifier, requestError(err))
	}
	if created == nil || created.ID == "" {
		return "", fmt.Errorf("%w: create resource %s: the response holds no resource id", ErrAPI, resource.Identifier)
	}
	return created.ID, nil
}

// PatchResource sets the resource's prefix and credential provider.
func (s *Session) PatchResource(ctx context.Context, resourceID string, prefix bool, providerID string) error {
	updated, err := s.zones.Resources.Update(ctx, resourceID, keycard.ZoneResourceUpdateParams{
		ZoneID:               s.Zone.ID,
		Prefix:               keycard.Bool(prefix),
		CredentialProviderID: keycard.String(providerID),
	})
	if err != nil {
		return fmt.Errorf("%w: update resource %s: %w", ErrAPI, resourceID, requestError(err))
	}
	if updated == nil || updated.ID != resourceID {
		return fmt.Errorf("%w: update resource %s: the response holds another resource or none", ErrAPI, resourceID)
	}
	return nil
}

// FindSecret returns the id of the token secret stored for the resource, or
// "" when there is none. The exchange reads the resource's first secret, so
// more than one secret, or one that is not a token, is refused. Every error
// wraps ErrAPI.
func (s *Session) FindSecret(ctx context.Context, resourceID string) (string, error) {
	all, err := s.listSecrets(ctx, resourceID)
	if err != nil {
		return "", err
	}
	for _, secret := range all {
		if secret.ID == "" || secret.EntityID != resourceID {
			return "", fmt.Errorf("%w: a secret listed for resource %s has no id or another entity_id",
				ErrAPI, resourceID)
		}
	}
	switch {
	case len(all) > 1:
		return "", fmt.Errorf("%w: %d secrets are stored for resource %s", ErrAPI, len(all), resourceID)
	case len(all) == 1 && all[0].Type != keycard.SecretTypeToken:
		return "", fmt.Errorf("%w: the secret stored for resource %s is not a token", ErrAPI, resourceID)
	case len(all) == 1:
		return all[0].ID, nil
	default:
		return "", nil
	}
}

// CreateSecret stores token as the resource's credential, named name.
func (s *Session) CreateSecret(ctx context.Context, resourceID, name, token string) error {
	created, err := s.zones.Secrets.New(ctx, s.Zone.ID, keycard.ZoneSecretNewParams{
		Name:     name,
		EntityID: resourceID,
		Data:     keycard.ZoneSecretNewParamsDataUnion{OfToken: tokenData(token)},
	})
	if err != nil {
		return fmt.Errorf("%w: create secret for resource %s: %w", ErrAPI, resourceID, requestError(err))
	}
	if created == nil || created.ID == "" {
		return fmt.Errorf("%w: create secret for resource %s: the response holds no secret id", ErrAPI, resourceID)
	}
	return nil
}

// UpdateSecret replaces the secret's token.
func (s *Session) UpdateSecret(ctx context.Context, secretID, token string) error {
	updated, err := s.zones.Secrets.Update(ctx, secretID, keycard.ZoneSecretUpdateParams{
		ZoneID: s.Zone.ID,
		Data:   keycard.ZoneSecretUpdateParamsDataUnion{OfToken: tokenData(token)},
	})
	if err != nil {
		return fmt.Errorf("%w: update secret %s: %w", ErrAPI, secretID, requestError(err))
	}
	if updated == nil || updated.ID != secretID {
		return fmt.Errorf("%w: update secret %s: the response holds another secret or none", ErrAPI, secretID)
	}
	return nil
}

func tokenData(token string) *keycard.SecretTokenFieldsParam {
	return &keycard.SecretTokenFieldsParam{Token: token, Type: keycard.SecretTokenFieldsTypeToken}
}

// ApplicationDependencies returns the ids of the resources the application
// depends on. Every error wraps ErrAPI.
func (s *Session) ApplicationDependencies(ctx context.Context, applicationID string) (map[string]bool, error) {
	all, err := listAll("dependencies", func(params pageParams) (listPage[keycard.Resource], error) {
		page, err := s.zones.Applications.Dependencies.List(ctx, applicationID,
			keycard.ZoneApplicationDependencyListParams{ZoneID: s.Zone.ID, After: params.After, Limit: params.Limit})
		if err != nil || page == nil {
			return listPage[keycard.Resource]{}, err //nolint:wrapcheck // listAll passes it through requestError.
		}
		return listPage[keycard.Resource]{
			items: page.Items, after: page.Pagination.AfterCursor,
			complete: page.JSON.Items.Valid() && page.JSON.Pagination.Valid(),
		}, nil
	})
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(all))
	for _, resource := range all {
		ids[resource.ID] = true
	}
	return ids, nil
}

// AddApplicationDependency makes the resource a dependency of the
// application.
func (s *Session) AddApplicationDependency(ctx context.Context, applicationID, resourceID string) error {
	err := s.zones.Applications.Dependencies.Add(ctx, resourceID, keycard.ZoneApplicationDependencyAddParams{
		ZoneID: s.Zone.ID, ID: applicationID,
	})
	if err != nil {
		return fmt.Errorf("%w: add resource %s as a dependency of application %s: %w",
			ErrAPI, resourceID, applicationID, requestError(err))
	}
	return nil
}

// ServiceAccountSubject is the sub claim of a Kubernetes token for the
// ServiceAccount named userID in namespace.
func ServiceAccountSubject(namespace, userID string) string {
	return "system:serviceaccount:" + namespace + ":" + userID
}

// FindUser returns the zone's user whose email is exactly email, as the
// management API's email filter matches it. The user has an ID and an Identifier. Every error
// wraps ErrAPI, ErrUserAmbiguous or ErrUserNotFound.
func (s *Session) FindUser(ctx context.Context, email string) (keycard.User, error) {
	all, err := s.listUsers(ctx, email)
	if err != nil {
		return keycard.User{}, err
	}
	var found []keycard.User
	for _, user := range all {
		if user.Email == email {
			found = append(found, user)
		}
	}
	switch {
	case len(found) > 1:
		return keycard.User{}, fmt.Errorf("%w: %d users have the email %s", ErrUserAmbiguous, len(found), email)
	case len(found) == 0:
		return keycard.User{}, fmt.Errorf("%w: no user has the email %s. Create the user in the zone first",
			ErrUserNotFound, email)
	case found[0].ID == "" || found[0].Identifier == "":
		return keycard.User{}, fmt.Errorf("%w: the user %s has no id or identifier", ErrAPI, email)
	default:
		return found[0], nil
	}
}

// FindTokenCredential returns the id of the application's token credential
// that the provider verifies for subject, or "" when there is none. A bare
// "*" subject, which the zone stores for a credential created without one,
// matches only a literal "*" and is ignored. Every error wraps ErrAPI or
// ErrCredentialWildcard.
func (s *Session) FindTokenCredential(ctx context.Context, applicationID, providerID, subject string) (string, error) {
	all, err := s.listCredentials(ctx, applicationID)
	if err != nil {
		return "", err
	}
	var found []string
	for _, credential := range all {
		switch {
		case credential.Type != tokenCredential, credential.ProviderID != providerID:
		case wildcardSubject(credential.Subject):
			return "", fmt.Errorf("%w: credential %s of application %s, for provider %s, has the subject %s",
				ErrCredentialWildcard, credential.ID, applicationID, providerID, credential.Subject)
		case credential.Subject == subject:
			found = append(found, credential.ID)
		}
	}
	switch {
	case len(found) > 1:
		return "", fmt.Errorf("%w: application %s has %d token credentials for %s",
			ErrAPI, applicationID, len(found), subject)
	case len(found) == 0:
		return "", nil
	case found[0] == "":
		return "", fmt.Errorf("%w: the token credential for %s has no id", ErrAPI, subject)
	default:
		return found[0], nil
	}
}

func wildcardSubject(subject string) bool {
	return len(subject) > 1 && strings.HasSuffix(subject, "*")
}

// CreateTokenCredential creates the application's token credential that the
// provider verifies for subject, and returns its id.
func (s *Session) CreateTokenCredential(
	ctx context.Context,
	applicationID, providerID, subject string,
) (string, error) {
	created, err := s.zones.ApplicationCredentials.New(ctx, s.Zone.ID, keycard.ZoneApplicationCredentialNewParams{
		OfApplicationCredentialCreateToken: &keycard.ZoneApplicationCredentialNewParamsBodyApplicationCredentialCreateToken{
			ApplicationID: applicationID,
			ProviderID:    providerID,
			Type:          tokenCredential,
			Subject:       keycard.String(subject),
		},
	})
	if err != nil {
		return "", fmt.Errorf("%w: create token credential for %s: %w", ErrAPI, subject, requestError(err))
	}
	if created == nil || created.ID == "" {
		return "", fmt.Errorf("%w: create token credential for %s: the response holds no credential id", ErrAPI, subject)
	}
	return created.ID, nil
}

func (s *Session) vaultProvider(ctx context.Context) (string, error) {
	if s.vaultProviderID != "" {
		return s.vaultProviderID, nil
	}
	all, err := s.listProviders(ctx, keycard.ZoneProviderListParams{
		FilterType: keycard.ZoneProviderListParamsFilterTypeUnion{
			OfZoneProviderListsFilterTypeString: keycard.String(string(keycard.ProviderTypeKeycardVault)),
		},
	})
	if err != nil {
		return "", err
	}
	providerID, err := oneProvider(
		all,
		func(p keycard.Provider) bool { return p.Type == keycard.ProviderTypeKeycardVault },
		"the type keycard-vault",
	)
	if err != nil {
		return "", err
	}
	s.vaultProviderID = providerID
	return providerID, nil
}

func (s *Session) listResources(ctx context.Context, identifier string) ([]keycard.Resource, error) {
	return listAll("resources", func(params pageParams) (listPage[keycard.Resource], error) {
		page, err := s.zones.Resources.List(ctx, s.Zone.ID, keycard.ZoneResourceListParams{
			After: params.After, Limit: params.Limit,
			FilterIdentifier: keycard.ZoneResourceListParamsFilterIdentifierUnion{
				OfString: keycard.String(identifier),
			},
		})
		if err != nil || page == nil {
			return listPage[keycard.Resource]{}, err //nolint:wrapcheck // listAll passes it through requestError.
		}
		return listPage[keycard.Resource]{
			items: page.Items, after: page.Pagination.AfterCursor,
			complete: page.JSON.Items.Valid() && page.JSON.Pagination.Valid(),
		}, nil
	})
}

// The secrets list is not paginated.
func (s *Session) listSecrets(ctx context.Context, resourceID string) ([]keycard.Secret, error) {
	all, err := s.zones.Secrets.List(ctx, s.Zone.ID, keycard.ZoneSecretListParams{
		EntityID: keycard.String(resourceID),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: list secrets: %w", ErrAPI, requestError(err))
	}
	if all == nil {
		return nil, fmt.Errorf("%w: list secrets: %w", ErrAPI, errNoPage)
	}
	return *all, nil
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

func (s *Session) listUsers(ctx context.Context, email string) ([]keycard.User, error) {
	return listAll("users", func(params pageParams) (listPage[keycard.User], error) {
		page, err := s.zones.Users.List(ctx, s.Zone.ID, keycard.ZoneUserListParams{
			After: params.After, Limit: params.Limit,
			FilterEmail: keycard.ZoneUserListParamsFilterEmailUnion{OfString: keycard.String(email)},
		})
		if err != nil || page == nil {
			return listPage[keycard.User]{}, err //nolint:wrapcheck // listAll passes it through requestError.
		}
		return listPage[keycard.User]{
			items: page.Items, after: page.Pagination.AfterCursor,
			complete: page.JSON.Items.Valid() && page.JSON.Pagination.Valid(),
		}, nil
	})
}

func (s *Session) listCredentials(ctx context.Context, applicationID string) ([]keycard.CredentialUnion, error) {
	all, err := listAll("application credentials", func(params pageParams) (listPage[keycard.CredentialUnion], error) {
		page, err := s.zones.ApplicationCredentials.List(ctx, s.Zone.ID, keycard.ZoneApplicationCredentialListParams{
			After: params.After, Limit: params.Limit, ApplicationID: keycard.String(applicationID),
		})
		if err != nil || page == nil {
			return listPage[keycard.CredentialUnion]{}, err //nolint:wrapcheck // listAll passes it through requestError.
		}
		return listPage[keycard.CredentialUnion]{
			items: page.Items, after: page.Pagination.AfterCursor,
			complete: page.JSON.Items.Valid() && page.JSON.Pagination.Valid(),
		}, nil
	})
	if err != nil {
		return nil, err
	}
	for _, credential := range all {
		if credential.ApplicationID != applicationID {
			return nil, fmt.Errorf("%w: a credential listed for application %s has another application_id",
				ErrAPI, applicationID)
		}
	}
	return all, nil
}

func (s *Session) listProviders(
	ctx context.Context,
	filter keycard.ZoneProviderListParams,
) ([]keycard.Provider, error) {
	return listAll("providers", func(params pageParams) (listPage[keycard.Provider], error) {
		filter.After, filter.Limit = params.After, params.Limit
		page, err := s.zones.Providers.List(ctx, s.Zone.ID, filter)
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
	managementAPIURL, clientID, secret string,
) (string, error) {
	base, err := url.Parse(managementAPIURL)
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
	req.SetBasicAuth(clientID, secret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrAuth, requestError(err))
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		err = fmt.Errorf("%w: the token endpoint returned HTTP %d for client %s. "+
			"The client must be an organization service account",
			ErrAuth, res.StatusCode, clientID)
		// RFC 6749 section 5.2: a token endpoint refuses a request with 400.
		// When client authentication fails, it may answer 401, and must when
		// the client used the Authorization header.
		if res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusUnauthorized {
			err = fmt.Errorf("%w: %w", ErrRefused, err)
		}
		return "", err
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
