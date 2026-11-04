package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/keycardai/agent-sandbox-examples/internal/newsandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

// clientSecretName is the Secret that holds the Application's client secret
// in the sandbox namespace. The sandbox chart reads it under the key
// client-secret.
const clientSecretName = "egress-proxy-client-secret" //nolint:gosec // G101: a Secret's name, not a secret.

// secretManifest is the Secret applyClientSecret sends to kubectl apply.
type secretManifest struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   secretMetadata    `json:"metadata"`
	Type       string            `json:"type"`
	StringData map[string]string `json:"stringData"`
}

type secretMetadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// clientSecretFile is where zone-setup keeps the Application's client ID and
// secret in a client secret run.
func (l *Local) clientSecretFile() string {
	return filepath.Join(l.outDir(), "client-secret.json")
}

// chartClientSecret returns the sandbox chart's clientSecret value in a
// client secret run, and nil otherwise.
func (l *Local) chartClientSecret() *newsandbox.ClientSecret {
	if !l.ClientSecret() {
		return nil
	}
	return &newsandbox.ClientSecret{SecretName: clientSecretName}
}

// applyClientSecret creates or updates the Secret clientSecretName in
// namespace from the file zone-setup wrote. A running pod keeps the secret
// it started with.
func (l *Local) applyClientSecret(ctx context.Context, cluster, namespace string) error {
	kept, err := zonesetup.ReadClientSecret(l.clientSecretFile())
	if err != nil {
		return fmt.Errorf("%w. Run without --skip-zone-setup", err)
	}
	manifest, err := json.Marshal(secretManifest{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata:   secretMetadata{Name: clientSecretName, Namespace: namespace},
		Type:       "Opaque",
		StringData: map[string]string{"client-secret": kept.ClientSecret},
	})
	if err != nil {
		return fmt.Errorf("encoding the Secret %s: %w", clientSecretName, err)
	}
	l.step("Storing the client secret " + kept.ClientID + " in the Secret " + clientSecretName)
	if _, err = l.kubectl(ctx, cluster, manifest, "apply", "--namespace", namespace, "-f", "-"); err != nil {
		return fmt.Errorf("applying the Secret %s: %w", clientSecretName, err)
	}
	return nil
}
