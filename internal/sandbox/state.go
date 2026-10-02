// Package sandbox sets up a Keycard sandbox in localdev through svc-catalog and
// runs an agent in it. cmd/sandbox drives it as a wizard or as commands.
package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// State is what setup records so later commands need only an email. It holds
// no secret: API keys go into the zone's vault, and the service account
// credentials come from the environment.
type State struct {
	APIURL        string           `json:"api_url"`
	OrgID         string           `json:"org_id"`
	ZoneID        string           `json:"zone_id"`
	ZoneName      string           `json:"zone_name"`
	ZoneURL       string           `json:"zone_url"`
	Cluster       string           `json:"cluster"`
	Issuer        string           `json:"issuer"`
	Namespace     string           `json:"namespace"`
	ApplicationID string           `json:"application_id"`
	ProviderID    string           `json:"provider_id"`
	Resources     []Resource       `json:"resources"`
	PreviousSet   *Binding         `json:"previous_policy_set,omitempty"`
	Users         map[string]*User `json:"users"`
}

// Resource is an API server installed with an API key.
type Resource struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	APIURL string `json:"api_url"`
}

// User is a zone user the CLI started a sandbox for. A user with access is
// enrolled through kubernetes-sandbox-user, which writes their policies. A user
// without access has a sandbox and a credential but no policies, so the zone
// refuses every request their sandbox makes.
type User struct {
	Email        string `json:"email"`
	ID           string `json:"id"`
	Identifier   string `json:"identifier"`
	Subject      string `json:"subject"`
	CredentialID string `json:"credential_id"`
	Pod          string `json:"pod"`
	Access       bool   `json:"access"`
}

// DefaultStatePath is ~/.keycard/sandbox/state.json.
func DefaultStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".keycard", "sandbox", "state.json")
	}
	return filepath.Join(home, ".keycard", "sandbox", "state.json")
}

// LoadState reads the state file. A missing file is an empty state.
func LoadState(path string) (*State, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the path is the CLI's own state file
	if errors.Is(err, os.ErrNotExist) {
		return &State{Users: map[string]*User{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if s.Users == nil {
		s.Users = map[string]*User{}
	}
	return &s, nil
}

// Save writes the state file, readable by its owner alone.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return nil
}

// ResourceURLs lists the installed resources' identifiers.
func (s *State) ResourceURLs() []string {
	urls := make([]string, 0, len(s.Resources))
	for _, r := range s.Resources {
		urls = append(urls, r.APIURL)
	}
	return urls
}

// SortedUsers lists the users, those with access first, each group by email.
func (s *State) SortedUsers() []*User {
	users := make([]*User, 0, len(s.Users))
	for _, u := range s.Users {
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].Access != users[j].Access {
			return users[i].Access
		}
		return users[i].Email < users[j].Email
	})
	return users
}

// HasResource reports whether setup installed the API server.
func (s *State) HasResource(slug string) bool {
	for _, r := range s.Resources {
		if r.Slug == slug {
			return true
		}
	}
	return false
}

// SetResource records an installed resource, replacing one with the same slug.
func (s *State) SetResource(r Resource) {
	for i := range s.Resources {
		if s.Resources[i].Slug == r.Slug {
			s.Resources[i] = r
			return
		}
	}
	s.Resources = append(s.Resources, r)
}
