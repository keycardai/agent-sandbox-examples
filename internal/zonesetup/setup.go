package zonesetup

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/keycardai/keycard-go"
	"github.com/keycardai/keycard-go/packages/param"
)

// listPageSize is how many items each list request asks for.
const listPageSize = 100

// The error classes of the apply steps, beside the classes values.go
// declares.
var (
	// ErrAmbiguous is an identifier more than one entity in the zone has.
	ErrAmbiguous = errors.New("ambiguous")
	// ErrNotFound is an entity the run needs but does not create, such as a
	// credential provider.
	ErrNotFound = errors.New("not found")
)

// Setup creates or corrects the per-zone entities the values file describes:
// the cluster's OIDC provider, the egress proxy application, and its
// resources with their secrets and dependencies.
//
// Every step is create-or-patch. An entity that already matches is left
// alone, one whose settings disagree with the values file is patched, and
// nothing is deleted. A run over a zone that is already set up writes only
// the resource secrets, whose stored values cannot be read back to compare.
type Setup struct {
	Values  *Values
	Secrets *Secrets
	// DryRun reports what each step would do and writes nothing.
	DryRun bool
	// Log receives one line per step. Nil discards them.
	Log io.Writer

	zones           *keycard.ZoneService
	vaultProviderID string
}

// Outputs are the ids new-sandbox needs, in the shape of the catalog
// installs' outputs.
type Outputs struct {
	K8sSandbox K8sSandboxOutputs `json:"k8s_sandbox"`
}

// K8sSandboxOutputs are the once-per-zone install's outputs.
type K8sSandboxOutputs struct {
	ProviderID            string `json:"provider_id"`
	ApplicationID         string `json:"application_id"`
	ApplicationIdentifier string `json:"application_identifier"`
	Namespace             string `json:"namespace"`
}

// pendingID stands for the id of an entity a dry run would have created.
// A step that needs such an id reports its intent and does nothing.
const pendingID = ""

// Run authenticates as the organization service account, resolves the zone,
// performs every step in dependency order and returns the outputs.
func (s *Setup) Run(ctx context.Context) (*Outputs, error) {
	zones, err := connect(ctx, s.Values, s.Secrets)
	if err != nil {
		return nil, err
	}
	s.zones = zones
	zone, err := resolveZone(ctx, zones, s.Values)
	if err != nil {
		return nil, err
	}
	s.sayf("zone %s issues %s", zone.ID, zone.Protocols.Oauth2.Issuer)
	providerID, err := s.ensureProvider(ctx, zone.ID)
	if err != nil {
		return nil, fmt.Errorf("provider: %w", err)
	}
	appID, err := s.ensureApplication(ctx, zone.ID)
	if err != nil {
		return nil, fmt.Errorf("application: %w", err)
	}
	resourceIDs, err := s.ensureResources(ctx, zone.ID)
	if err != nil {
		return nil, err
	}
	if err = s.ensureDependencies(ctx, zone.ID, appID, resourceIDs); err != nil {
		return nil, fmt.Errorf("dependencies: %w", err)
	}
	return &Outputs{
		K8sSandbox: K8sSandboxOutputs{
			ProviderID:            providerID,
			ApplicationID:         appID,
			ApplicationIdentifier: ApplicationIdentifier(s.Values.Namespace),
			Namespace:             s.Values.Namespace,
		},
	}, nil
}

// ensureProvider creates or finds the OIDC Provider for the cluster issuer.
// The provider is identified by the issuer URL and has only an issuer. The
// zone discovers the JWKS from it and verifies ServiceAccount tokens.
func (s *Setup) ensureProvider(ctx context.Context, zoneID string) (string, error) {
	issuer := s.Values.IssuerURL
	provider, found, err := s.findProvider(ctx, zoneID, ProviderIdentifier(issuer))
	if err != nil {
		return "", err
	}
	if !found {
		return s.createProvider(ctx, zoneID, issuer)
	}
	if provider.Protocols.Oauth2.Issuer == issuer {
		s.sayf("provider %s is %s", provider.Identifier, provider.ID)
		return provider.ID, nil
	}
	change := fmt.Sprintf("issuer %q -> %q", provider.Protocols.Oauth2.Issuer, issuer)
	if s.DryRun {
		s.sayf("would patch provider %s (%s)", provider.ID, change)
		return provider.ID, nil
	}
	_, err = s.zones.Providers.Update(ctx, provider.ID, keycard.ZoneProviderUpdateParams{
		ZoneID: zoneID,
		Protocols: keycard.ZoneProviderUpdateParamsProtocols{
			Oauth2: keycard.ZoneProviderUpdateParamsProtocolsOauth2{Issuer: param.NewOpt(issuer)},
		},
	})
	if err != nil {
		return "", apiErrorf(err, "patching provider %s", provider.ID)
	}
	s.sayf("patched provider %s (%s)", provider.ID, change)
	return provider.ID, nil
}

func (s *Setup) createProvider(ctx context.Context, zoneID, issuer string) (string, error) {
	identifier := ProviderIdentifier(issuer)
	if s.DryRun {
		s.sayf("would create provider %s", identifier)
		return pendingID, nil
	}
	provider, err := s.zones.Providers.New(ctx, zoneID, keycard.ZoneProviderNewParams{
		Identifier:  identifier,
		Name:        providerName(issuer),
		Description: param.NewOpt("OIDC issuer of the Kubernetes cluster that runs the sandboxes"),
		Protocols: keycard.ZoneProviderNewParamsProtocols{
			Oauth2: keycard.ZoneProviderNewParamsProtocolsOauth2{Issuer: param.NewOpt(issuer)},
		},
	})
	if err != nil {
		return "", apiErrorf(err, "creating provider %s", identifier)
	}
	s.sayf("created provider %s as %s", identifier, provider.ID)
	return provider.ID, nil
}

// findProvider is the Provider with exactly this identifier, if there is one.
func (s *Setup) findProvider(ctx context.Context, zoneID, identifier string) (keycard.Provider, bool, error) {
	res, err := s.zones.Providers.List(ctx, zoneID, keycard.ZoneProviderListParams{
		Identifier: param.NewOpt(identifier),
	})
	if err != nil {
		return keycard.Provider{}, false, apiErrorf(err, "listing providers")
	}
	return exactlyOne(res.Items, func(provider keycard.Provider) bool {
		return provider.Identifier == identifier
	}, "providers have identifier "+identifier)
}

// ensureApplication creates or finds the egress proxy Application under its
// well-known identifier. Consent is implicit: the proxy acts for an absent
// user, so there is nobody to prompt.
func (s *Setup) ensureApplication(ctx context.Context, zoneID string) (string, error) {
	identifier := ApplicationIdentifier(s.Values.Namespace)
	res, err := s.zones.Applications.List(ctx, zoneID, keycard.ZoneApplicationListParams{
		FilterIdentifier: keycard.ZoneApplicationListParamsFilterIdentifierUnion{OfString: param.NewOpt(identifier)},
	})
	if err != nil {
		return "", apiErrorf(err, "listing applications")
	}
	app, found, err := exactlyOne(res.Items, func(app keycard.Application) bool {
		return app.Identifier == identifier
	}, "applications have identifier "+identifier)
	if err != nil {
		return "", err
	}
	if !found {
		return s.createApplication(ctx, zoneID, identifier)
	}
	if app.Consent == keycard.ApplicationConsentImplicit {
		s.sayf("application %s is %s", identifier, app.ID)
		return app.ID, nil
	}
	if s.DryRun {
		s.sayf("would patch application %s (consent %s -> implicit)", app.ID, app.Consent)
		return app.ID, nil
	}
	_, err = s.zones.Applications.Update(ctx, app.ID, keycard.ZoneApplicationUpdateParams{
		ZoneID:  zoneID,
		Consent: keycard.ZoneApplicationUpdateParamsConsentImplicit,
	})
	if err != nil {
		return "", apiErrorf(err, "patching application %s", app.ID)
	}
	s.sayf("patched application %s (consent %s -> implicit)", app.ID, app.Consent)
	return app.ID, nil
}

func (s *Setup) createApplication(ctx context.Context, zoneID, identifier string) (string, error) {
	if s.DryRun {
		s.sayf("would create application %s", identifier)
		return pendingID, nil
	}
	app, err := s.zones.Applications.New(ctx, zoneID, keycard.ZoneApplicationNewParams{
		Identifier: identifier,
		Name:       applicationName,
		Description: param.NewOpt(
			"Egress proxy sidecar that mints credentials for sandboxes in namespace " + s.Values.Namespace,
		),
		Consent: keycard.ZoneApplicationNewParamsConsentImplicit,
	})
	if err != nil {
		return "", apiErrorf(err, "creating application %s", identifier)
	}
	s.sayf("created application %s as %s", identifier, app.ID)
	return app.ID, nil
}

// ensureResources creates or patches each Resource and stores its static
// credential, and returns the resource ids in values-file order.
func (s *Setup) ensureResources(ctx context.Context, zoneID string) ([]string, error) {
	ids := make([]string, 0, len(s.Values.Resources))
	for _, resource := range s.Values.Resources {
		providerID, err := s.resourceProvider(ctx, zoneID, resource)
		if err != nil {
			return nil, fmt.Errorf("resource %s: %w", resource.Identifier, err)
		}
		resourceID, err := s.ensureResource(ctx, zoneID, resource, providerID)
		if err != nil {
			return nil, fmt.Errorf("resource %s: %w", resource.Identifier, err)
		}
		ids = append(ids, resourceID)
		if resource.SecretEnv == "" {
			continue
		}
		if err = s.ensureSecret(ctx, zoneID, resource, resourceID); err != nil {
			return nil, fmt.Errorf("resource %s: secret: %w", resource.Identifier, err)
		}
	}
	return ids, nil
}

// resourceProvider is the id of the Provider that serves the resource's
// credential: the named existing provider, or the zone vault for a static
// token.
func (s *Setup) resourceProvider(ctx context.Context, zoneID string, resource ResourceValues) (string, error) {
	if resource.CredentialProvider == "" {
		return s.vaultProvider(ctx, zoneID)
	}
	provider, found, err := s.findProvider(ctx, zoneID, resource.CredentialProvider)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: provider with identifier %s. Create it in the zone first",
			ErrNotFound, resource.CredentialProvider)
	}
	return provider.ID, nil
}

// vaultProvider is the zone's keycard-vault Provider, which serves a static
// credential stored against a Resource.
func (s *Setup) vaultProvider(ctx context.Context, zoneID string) (string, error) {
	if s.vaultProviderID != "" {
		return s.vaultProviderID, nil
	}
	res, err := s.zones.Providers.List(ctx, zoneID, keycard.ZoneProviderListParams{
		Type: keycard.ZoneProviderListParamsTypeKeycardVault,
	})
	if err != nil {
		return "", apiErrorf(err, "listing providers")
	}
	provider, found, err := exactlyOne(res.Items, func(provider keycard.Provider) bool {
		return provider.Type == keycard.ProviderTypeKeycardVault
	}, "keycard-vault providers in the zone")
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: keycard-vault provider to store a static credential with", ErrNotFound)
	}
	s.vaultProviderID = provider.ID
	s.sayf("static credentials are served by provider %s", provider.ID)
	return provider.ID, nil
}

func (s *Setup) ensureResource(
	ctx context.Context,
	zoneID string,
	resource ResourceValues,
	providerID string,
) (string, error) {
	res, err := s.zones.Resources.List(ctx, zoneID, keycard.ZoneResourceListParams{
		FilterIdentifier: keycard.ZoneResourceListParamsFilterIdentifierUnion{OfString: param.NewOpt(resource.Identifier)},
	})
	if err != nil {
		return "", apiErrorf(err, "listing resources")
	}
	existing, found, err := exactlyOne(res.Items, func(existing keycard.Resource) bool {
		return existing.Identifier == resource.Identifier
	}, "resources have identifier "+resource.Identifier)
	if err != nil {
		return "", err
	}
	if !found {
		return s.createResource(ctx, zoneID, resource, providerID)
	}
	if existing.Prefix == resource.Prefix && existing.CredentialProviderID == providerID {
		s.sayf("resource %s is %s", resource.Identifier, existing.ID)
		return existing.ID, nil
	}
	change := fmt.Sprintf("prefix %t -> %t, provider %q -> %q",
		existing.Prefix, resource.Prefix, existing.CredentialProviderID, providerID)
	if s.DryRun {
		s.sayf("would patch resource %s (%s)", existing.ID, change)
		return existing.ID, nil
	}
	_, err = s.zones.Resources.Update(ctx, existing.ID, keycard.ZoneResourceUpdateParams{
		ZoneID:               zoneID,
		Prefix:               param.NewOpt(resource.Prefix),
		CredentialProviderID: param.NewOpt(providerID),
	})
	if err != nil {
		return "", apiErrorf(err, "patching resource %s", existing.ID)
	}
	s.sayf("patched resource %s (%s)", existing.ID, change)
	return existing.ID, nil
}

func (s *Setup) createResource(
	ctx context.Context,
	zoneID string,
	resource ResourceValues,
	providerID string,
) (string, error) {
	if s.DryRun {
		s.sayf("would create resource %s (prefix=%t, provider=%s)", resource.Identifier, resource.Prefix, providerID)
		return pendingID, nil
	}
	created, err := s.zones.Resources.New(ctx, zoneID, keycard.ZoneResourceNewParams{
		Identifier:           resource.Identifier,
		Name:                 resource.Name,
		Prefix:               param.NewOpt(resource.Prefix),
		CredentialProviderID: param.NewOpt(providerID),
	})
	if err != nil {
		return "", apiErrorf(err, "creating resource %s", resource.Identifier)
	}
	s.sayf("created resource %s as %s", resource.Identifier, created.ID)
	return created.ID, nil
}

// ensureSecret stores the resource's static token in the zone vault. The
// exchange reads the first secret stored against the resource, so there is
// exactly one per resource: an existing secret is updated in place and NEVER
// joined by a second. A stored value cannot be read back, so every run
// writes the value it holds.
func (s *Setup) ensureSecret(ctx context.Context, zoneID string, resource ResourceValues, resourceID string) error {
	if resourceID == pendingID {
		s.sayf("would store the %s credential for %s", resource.SecretEnv, resource.Identifier)
		return nil
	}
	existing, found, err := s.findSecret(ctx, zoneID, resourceID)
	if err != nil {
		return err
	}
	if s.DryRun {
		if found {
			s.sayf("would update secret %s from %s", existing.ID, resource.SecretEnv)
		} else {
			s.sayf("would store the %s credential for %s", resource.SecretEnv, resource.Identifier)
		}
		return nil
	}
	data := keycard.SecretTokenFieldsParam{
		Token: s.Secrets.Resource[resource.SecretEnv],
		Type:  keycard.SecretTokenFieldsTypeToken,
	}
	if !found {
		_, err = s.zones.Secrets.New(ctx, zoneID, keycard.ZoneSecretNewParams{
			Name:        resource.SecretEnv,
			Description: param.NewOpt("Credential the sandbox proxy attaches for " + resource.Identifier),
			EntityID:    resourceID,
			Data:        keycard.ZoneSecretNewParamsDataUnion{OfToken: &data},
		})
		if err != nil {
			return apiErrorf(err, "storing secret")
		}
		s.sayf("stored the %s credential for %s", resource.SecretEnv, resource.Identifier)
		return nil
	}
	_, err = s.zones.Secrets.Update(ctx, existing.ID, keycard.ZoneSecretUpdateParams{
		ZoneID: zoneID,
		Data:   keycard.ZoneSecretUpdateParamsDataUnion{OfToken: &data},
	})
	if err != nil {
		return apiErrorf(err, "updating secret %s", existing.ID)
	}
	s.sayf("updated the %s credential for %s", resource.SecretEnv, resource.Identifier)
	return nil
}

// findSecret is the token secret stored against the resource, if any.
func (s *Setup) findSecret(ctx context.Context, zoneID, resourceID string) (keycard.Secret, bool, error) {
	res, err := s.zones.Secrets.List(ctx, zoneID, keycard.ZoneSecretListParams{
		EntityID: param.NewOpt(resourceID),
		Type:     keycard.ZoneSecretListParamsTypeToken,
	})
	if err != nil {
		return keycard.Secret{}, false, apiErrorf(err, "listing secrets")
	}
	if res == nil {
		return keycard.Secret{}, false, nil
	}
	for _, secret := range *res {
		if secret.EntityID == resourceID {
			return secret, true, nil
		}
	}
	return keycard.Secret{}, false, nil
}

// ensureDependencies makes each resource a dependency of the Application.
// An exchange for a resource the application does not depend on fails with
// interaction_required before any policy is read.
func (s *Setup) ensureDependencies(ctx context.Context, zoneID, appID string, resourceIDs []string) error {
	if len(resourceIDs) == 0 {
		return nil
	}
	if appID == pendingID {
		s.sayf("would make %d resources dependencies of the application", len(resourceIDs))
		return nil
	}
	has, err := s.dependencies(ctx, zoneID, appID)
	if err != nil {
		return err
	}
	for _, resourceID := range resourceIDs {
		if err = s.ensureDependency(ctx, zoneID, appID, resourceID, has[resourceID]); err != nil {
			return err
		}
	}
	return nil
}

// dependencies is the set of resource ids the application depends on.
func (s *Setup) dependencies(ctx context.Context, zoneID, appID string) (map[string]bool, error) {
	existing, err := listAll(func(after string) ([]keycard.Resource, string, error) {
		res, err := s.zones.Applications.Dependencies.List(ctx, appID, keycard.ZoneApplicationDependencyListParams{
			ZoneID: zoneID, After: optString(after), Limit: param.NewOpt[int64](listPageSize),
		})
		if err != nil {
			return nil, "", apiErrorf(err, "listing dependencies of application %s", appID)
		}
		return res.Items, next(res.PageInfo.HasNextPage, res.Pagination.AfterCursor), nil
	})
	if err != nil {
		return nil, err
	}
	has := make(map[string]bool, len(existing))
	for _, resource := range existing {
		has[resource.ID] = true
	}
	return has, nil
}

func (s *Setup) ensureDependency(ctx context.Context, zoneID, appID, resourceID string, has bool) error {
	switch {
	case resourceID == pendingID:
		s.sayf("would make the new resource a dependency of application %s", appID)
	case has:
		s.sayf("application %s depends on resource %s", appID, resourceID)
	case s.DryRun:
		s.sayf("would make resource %s a dependency of application %s", resourceID, appID)
	default:
		err := s.zones.Applications.Dependencies.Add(ctx, resourceID, keycard.ZoneApplicationDependencyAddParams{
			ZoneID: zoneID, ID: appID,
		})
		if err != nil {
			return apiErrorf(err, "adding resource %s", resourceID)
		}
		s.sayf("application %s now depends on resource %s", appID, resourceID)
	}
	return nil
}

func (s *Setup) sayf(format string, args ...any) {
	if s.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(s.Log, "zone-setup: "+format+"\n", args...)
}

// exactlyOne is the single item match accepts. None is reported as not
// found. More than one is ErrAmbiguous, described by describe ("providers
// have identifier x").
//
//nolint:ireturn // T is a type parameter, so every call site gets a concrete type.
func exactlyOne[T any](items []T, match func(T) bool, describe string) (T, bool, error) {
	var zero T
	var found []T
	for _, item := range items {
		if match(item) {
			found = append(found, item)
		}
	}
	switch len(found) {
	case 0:
		return zero, false, nil
	case 1:
		return found[0], true, nil
	default:
		return zero, false, fmt.Errorf("%w: %d %s", ErrAmbiguous, len(found), describe)
	}
}

// listAll walks a cursor-paginated list. page returns one page and the
// cursor of the next, or "" on the last page.
func listAll[T any](page func(after string) ([]T, string, error)) ([]T, error) {
	var all []T
	after := ""
	for {
		items, nextAfter, err := page(after)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if nextAfter == "" || len(items) == 0 {
			return all, nil
		}
		after = nextAfter
	}
}

func next(hasNext bool, cursor string) string {
	if !hasNext {
		return ""
	}
	return cursor
}

// apiErrorf wraps a failed management API request in ErrAPI. It keeps the
// response body and any redirect target out of the error.
func apiErrorf(err error, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %w", ErrAPI, fmt.Sprintf(format, args...), requestError(err))
}

func optString(value string) param.Opt[string] {
	if value == "" {
		return param.Opt[string]{}
	}
	return param.NewOpt(value)
}
