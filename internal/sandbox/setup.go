package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	// sandboxPolicySet is the policy set kubernetes-sandbox-user writes.
	sandboxPolicySet = "Kubernetes Sandbox"
	// defaultPolicySet is the platform's policy set a zone starts with.
	defaultPolicySet = "default-zone-policies"
)

var (
	errNoAPIURL       = errors.New("the install returned no api_url")
	errNoResources    = errors.New("no API server is installed; pick at least one")
	errZoneNoURL      = errors.New("the zone has no URL")
	errIssuerMismatch = errors.New("the cluster mints another issuer; run `sandbox down` to recreate it")
)

// SetupInput is what setup needs.
type SetupInput struct {
	ZoneID         string
	APIKeys        []APIKey
	Cluster        string
	Issuer         string
	Namespace      string
	EgressProxyDir string
}

// APIKey is the key an API server package is installed with.
type APIKey struct {
	Slug string
	Name string
	// Label is the package's name for the key, such as Personal API Key.
	Label string
	Key   string
}

// Setup prepares the zone and the sandbox cluster. Every step checks first,
// so running it again changes only what is missing.
func (a *App) Setup(ctx context.Context, input SetupInput) error {
	state := a.State
	state.Cluster, state.Issuer, state.Namespace = input.Cluster, input.Issuer, input.Namespace
	a.UI.Section("Setting up")

	steps := make([]step, 0, 1+len(input.APIKeys))
	steps = append(steps, step{"Zone", func(ctx context.Context) (string, error) { return a.useZone(ctx, input.ZoneID) }})
	for _, key := range input.APIKeys {
		steps = append(steps, step{key.Name + " installed", func(ctx context.Context) (string, error) {
			return a.installAPIKey(ctx, key)
		}})
	}
	if err := a.runSteps(ctx, steps...); err != nil {
		return err
	}
	if len(state.Resources) == 0 {
		return errNoResources
	}

	sandbox := a.sandboxKube()
	return a.runSteps(ctx,
		step{"Current policy set recorded for `sandbox down`", a.recordPolicySet},
		step{"Egress images", func(ctx context.Context) (string, error) {
			missing := MissingImages(ctx)
			if len(missing) == 0 {
				return "already built", nil
			}
			return "built " + strings.Join(missing, ", "), BuildImages(ctx, input.EgressProxyDir)
		}},
		step{"Sandbox cluster " + state.Cluster, a.ensureCluster},
		step{"Images in the cluster", func(ctx context.Context) (string, error) {
			if ImportedImages(ctx, state.Cluster) {
				return "already imported", nil
			}
			return "imported", ImportImages(ctx, state.Cluster)
		}},
		step{"Issuer published", func(ctx context.Context) (string, error) {
			return state.Issuer, PublishIssuer(ctx, sandbox, Kube{Context: LocaldevContext}, state.Issuer)
		}},
		step{"Issuer verified the way the zone checks it", func(ctx context.Context) (string, error) {
			return "", VerifyIssuer(ctx, sandbox, state.Issuer)
		}},
		step{"Sandbox application installed", a.installApplication},
		step{"Namespace " + state.Namespace + " ready, zone CA loaded", func(ctx context.Context) (string, error) {
			ca, err := LocaldevCA()
			if err != nil {
				return "", err
			}
			return "", sandbox.Apply(ctx, NamespaceObjects(state.Namespace, ca)...)
		}},
	)
}

func (a *App) useZone(ctx context.Context, zoneID string) (string, error) {
	zone, err := a.KC.GetZone(ctx, zoneID)
	if err != nil {
		return "", err
	}
	if zone.URL() == "" {
		return "", fmt.Errorf("%w: %s", errZoneNoURL, zone.ID)
	}
	state := a.State
	if zone.ID != state.ZoneID {
		// A different zone starts a fresh record of what it holds.
		state.Resources, state.Users, state.PreviousSet = nil, map[string]*User{}, nil
		state.ApplicationID, state.ProviderID = "", ""
	}
	state.ZoneID, state.ZoneName, state.ZoneURL = zone.ID, zone.Name, zone.URL()
	return zone.Name + " (" + zone.ID + ")", a.save()
}

// installAPIKey installs an API server package with its key, which the
// install stores in the zone's vault.
func (a *App) installAPIKey(ctx context.Context, key APIKey) (string, error) {
	out, err := a.KC.Install(ctx, a.State.ZoneID, key.Slug, map[string]any{"api_key": key.Key})
	if err != nil {
		return "", err
	}
	apiURL, _ := out["api_url"].(string)
	if apiURL == "" {
		return "", fmt.Errorf("%w: %s", errNoAPIURL, key.Slug)
	}
	a.State.SetResource(Resource{Slug: key.Slug, Name: key.Name, APIURL: apiURL})
	return apiURL, a.save()
}

// recordPolicySet remembers the zone's active policy set before enrollment
// replaces it. When an earlier run already made the sandbox set active, the
// platform's default set is the one to restore.
func (a *App) recordPolicySet(ctx context.Context) (string, error) {
	state := a.State
	if state.PreviousSet != nil {
		return "already recorded", nil
	}
	binding, found, err := a.KC.ActiveBinding(ctx, state.ZoneID)
	if err != nil {
		return "", err
	}
	if !found {
		return "the zone has none", nil
	}
	set, err := a.KC.GetPolicySet(ctx, state.ZoneID, binding.PolicySetID)
	if err != nil {
		return "", err
	}
	if set.Name != sandboxPolicySet {
		state.PreviousSet = &binding
		return set.Name, a.save()
	}
	fallback, found, err := a.KC.FindPolicySet(ctx, state.ZoneID, defaultPolicySet)
	if err != nil {
		return "", err
	}
	if !found || fallback.LatestVersionID == "" {
		return "the sandbox set is already active", nil
	}
	state.PreviousSet = &Binding{PolicySetID: fallback.ID, PolicySetVersionID: fallback.LatestVersionID}
	return defaultPolicySet + " (the sandbox set was already active)", a.save()
}

func (a *App) ensureCluster(ctx context.Context) (string, error) {
	state := a.State
	exists, err := ClusterExists(ctx, state.Cluster)
	if err != nil {
		return "", err
	}
	if !exists {
		if err = CreateCluster(ctx, state.Cluster, state.Issuer); err != nil {
			return "", err
		}
	}
	// The API server cannot change its issuer while it runs.
	have, err := ClusterIssuer(ctx, a.sandboxKube())
	if err != nil {
		return "", err
	}
	if have != state.Issuer {
		return "", fmt.Errorf("%w: cluster %s mints %s, not %s", errIssuerMismatch, state.Cluster, have, state.Issuer)
	}
	if exists {
		return "already running", nil
	}
	return "created", nil
}

func (a *App) installApplication(ctx context.Context) (string, error) {
	state := a.State
	resources := make([]any, 0, len(state.Resources))
	for _, apiURL := range state.ResourceURLs() {
		resources = append(resources, apiURL)
	}
	out, err := a.KC.Install(ctx, state.ZoneID, "kubernetes-sandbox", map[string]any{
		"issuer": state.Issuer, "namespace": state.Namespace, "resources": resources,
	})
	if err != nil {
		return "", err
	}
	state.ApplicationID, _ = out["application_id"].(string)
	state.ProviderID, _ = out["provider_id"].(string)
	detail := applicationIdentifier(state.Namespace)
	if skipped := stringList(out["skipped_resources"]); len(skipped) > 0 {
		detail += " · skipped " + strings.Join(skipped, ", ")
	}
	return detail, a.save()
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
