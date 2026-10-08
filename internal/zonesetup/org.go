package zonesetup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/keycardai/keycard-go"
	"github.com/keycardai/keycard-go/option"
)

// ErrZoneNotReady means a new zone was not ready before the wait ran out.
var ErrZoneNotReady = errors.New("zone not ready")

// Org is the management API of the organization a service account belongs
// to, before a zone is chosen.
type Org struct {
	zones keycard.ZoneService
}

// Login authenticates as the organization service account clientID at
// managementAPIURL. It writes nothing. Every error wraps ErrAuth.
func Login(ctx context.Context, managementAPIURL, clientID, secret string) (*Org, error) {
	httpClient := &http.Client{Timeout: requestTimeout, CheckRedirect: refuseRedirect}
	token, err := serviceAccountToken(ctx, httpClient, managementAPIURL, clientID, secret)
	if err != nil {
		return nil, err
	}
	// NewZoneService reads no KEYCARD_API_* variable, unlike keycard.NewClient.
	return &Org{zones: keycard.NewZoneService(
		option.WithBaseURL(managementAPIURL),
		option.WithHTTPClient(httpClient),
		option.WithAPIKey(token),
		option.WithMaxRetries(0),
	)}, nil
}

// Session returns a session for zone.
func (o *Org) Session(zone *keycard.Zone) *Session {
	return &Session{Zone: zone, zones: o.zones}
}

// Zones lists the zones the service account can see. Every error wraps
// ErrAPI.
func (o *Org) Zones(ctx context.Context) ([]keycard.Zone, error) {
	return listZones(ctx, &o.zones)
}

// CreateZone creates a zone named name and checks every poll, for up to wait,
// whether it is ready. A zone is ready once it has an issuer URL and the
// policy engine stops answering 404 for it. The zone keeps the default that a
// user needs an invitation to register.
func (o *Org) CreateZone(ctx context.Context, name string, poll, wait time.Duration) (*keycard.Zone, error) {
	zone, err := o.zones.New(ctx, keycard.ZoneNewParams{Name: name})
	if err != nil {
		return nil, fmt.Errorf("%w: create zone %q: %w", ErrAPI, name, requestError(err))
	}
	if zone == nil || zone.ID == "" {
		return nil, fmt.Errorf("%w: create zone %q: the response holds no zone", ErrAPI, name)
	}
	deadline := time.Now().Add(wait)
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for zone %s: %w", zone.ID, ctx.Err())
		case <-time.After(poll):
		}
		ready, err := o.zoneReady(ctx, zone)
		if err != nil {
			return nil, err
		}
		if ready {
			return zone, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: zone %s after %s", ErrZoneNotReady, zone.ID, wait)
		}
	}
}

// zoneReady reports whether zone is ready. It reads the zone again while
// the issuer URL is empty.
func (o *Org) zoneReady(ctx context.Context, zone *keycard.Zone) (bool, error) {
	if zone.Protocols.Oauth2.Issuer == "" {
		got, err := o.zones.Get(ctx, zone.ID, keycard.ZoneGetParams{})
		if err != nil {
			return false, fmt.Errorf("%w: zone %s: %w", ErrAPI, zone.ID, requestError(err))
		}
		if got == nil || got.Protocols.Oauth2.Issuer == "" {
			return false, nil
		}
		*zone = *got
	}
	_, err := o.zones.PolicySets.List(ctx, zone.ID, keycard.ZonePolicySetListParams{
		FilterActive: keycard.Bool(true),
	})
	var apiErr *keycard.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: zone %s policy sets: %w", ErrAPI, zone.ID, requestError(err))
	}
	return true, nil
}

// Users lists every user in the zone. Every error wraps ErrAPI.
func (s *Session) Users(ctx context.Context) ([]keycard.User, error) {
	return listAll("users", func(params pageParams) (listPage[keycard.User], error) {
		page, err := s.zones.Users.List(ctx, s.Zone.ID, keycard.ZoneUserListParams{
			After: params.After, Limit: params.Limit,
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

// Providers lists every provider in the zone. Every error wraps ErrAPI.
func (s *Session) Providers(ctx context.Context) ([]keycard.Provider, error) {
	return s.listProviders(ctx, keycard.ZoneProviderListParams{})
}
