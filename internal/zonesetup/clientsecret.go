package zonesetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/keycardai/keycard-go"
)

const (
	passwordCredential = "password"
	// clientSecretMode lets only the owner read the client secret file.
	clientSecretMode = 0o600
)

// ErrClientSecret is a client secret file that zone-setup cannot read or
// write, or a client secret run without one.
var ErrClientSecret = errors.New("client secret")

// ClientSecret is the Application's password credential, as
// Options.ClientSecretFile holds it. The proxy sends ClientID and
// ClientSecret with HTTP basic authentication.
type ClientSecret struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// ReadClientSecret reads the client secret file at path. It returns an error
// that wraps fs.ErrNotExist when there is no file.
func ReadClientSecret(path string) (ClientSecret, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the file the operator named.
	if err != nil {
		return ClientSecret{}, fmt.Errorf("%w: %w", ErrClientSecret, err)
	}
	var kept ClientSecret
	if err = json.Unmarshal(raw, &kept); err != nil || kept.ClientID == "" || kept.ClientSecret == "" {
		return ClientSecret{}, fmt.Errorf("%w: %s does not hold a client_id and a client_secret", ErrClientSecret, path)
	}
	return kept, nil
}

// writeClientSecret writes secret to path, readable by its owner alone.
func writeClientSecret(path string, secret ClientSecret) error {
	raw, err := json.Marshal(secret) //nolint:gosec // G117: this file is where the secret is kept.
	if err != nil {
		return fmt.Errorf("%w: %w", ErrClientSecret, err)
	}
	if err = os.WriteFile(path, raw, clientSecretMode); err != nil {
		return fmt.Errorf("%w: %w", ErrClientSecret, err)
	}
	// WriteFile keeps the mode of a file that already exists.
	if err = os.Chmod(path, clientSecretMode); err != nil {
		return fmt.Errorf("%w: %w", ErrClientSecret, err)
	}
	return nil
}

// ensureClientSecret returns the client ID of the Application's password
// credential. It reuses the credential that opts.ClientSecretFile names
// while the Application still has it. Otherwise it creates one and writes
// its client ID and secret to the file. The zone returns the secret only on
// creation, so a credential without its file cannot be reused.
func ensureClientSecret(ctx context.Context, opts *Options, session *Session, applicationID string) (string, error) {
	if opts.ClientSecretFile == "" {
		return "", fmt.Errorf("%w: application_credential %s needs a file to keep the secret in",
			ErrClientSecret, CredentialClientSecret)
	}
	if applicationID == "" {
		opts.sayf("would create a client secret")
		return "", nil
	}
	clientID, err := keptClientSecret(ctx, opts, session, applicationID)
	if err != nil || clientID != "" {
		return clientID, err
	}
	if opts.DryRun {
		opts.sayf("would create a client secret")
		return "", nil
	}
	created, err := session.CreatePasswordCredential(ctx, applicationID)
	if err != nil {
		return "", fmt.Errorf("creating the client secret: %w", err)
	}
	if err = writeClientSecret(opts.ClientSecretFile, created); err != nil {
		return "", err
	}
	opts.sayf("created client secret %s and wrote it to %s", created.ClientID, opts.ClientSecretFile)
	return created.ClientID, nil
}

// keptClientSecret returns the client ID in opts.ClientSecretFile when the
// application still has that credential, and "" when there is no file or
// the application does not have it.
func keptClientSecret(ctx context.Context, opts *Options, session *Session, applicationID string) (string, error) {
	kept, err := ReadClientSecret(opts.ClientSecretFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", err
	}
	found, err := session.HasPasswordCredential(ctx, applicationID, kept.ClientID)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the client secret %s: %w", kept.ClientID, err)
	case found:
		opts.sayf("found client secret %s from %s", kept.ClientID, opts.ClientSecretFile)
		return kept.ClientID, nil
	}
	opts.sayf("application %s has no credential %s", applicationID, kept.ClientID)
	return "", nil
}

// HasPasswordCredential reports whether the application has a password
// credential whose client ID is clientID.
func (s *Session) HasPasswordCredential(ctx context.Context, applicationID, clientID string) (bool, error) {
	all, err := s.listCredentials(ctx, applicationID)
	if err != nil {
		return false, err
	}
	for _, credential := range all {
		if credential.Type == passwordCredential && credential.Identifier == clientID {
			return true, nil
		}
	}
	return false, nil
}

// CreatePasswordCredential creates a password credential for the
// application. The zone picks the client ID and the secret.
func (s *Session) CreatePasswordCredential(ctx context.Context, applicationID string) (ClientSecret, error) {
	type passwordParams = keycard.ZoneApplicationCredentialNewParamsBodyApplicationCredentialCreatePassword
	created, err := s.zones.ApplicationCredentials.New(ctx, s.Zone.ID, keycard.ZoneApplicationCredentialNewParams{
		OfApplicationCredentialCreatePassword: &passwordParams{ApplicationID: applicationID, Type: passwordCredential},
	})
	if err != nil {
		return ClientSecret{}, fmt.Errorf("%w: create password credential: %w", ErrAPI, requestError(err))
	}
	if created == nil || created.Identifier == "" || created.Password == "" {
		return ClientSecret{}, fmt.Errorf("%w: create password credential: the response holds no identifier "+
			"or password", ErrAPI)
	}
	return ClientSecret{ClientID: created.Identifier, ClientSecret: created.Password}, nil
}
