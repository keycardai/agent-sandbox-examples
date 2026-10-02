// Command zone-setup configures a Keycard zone for the sandboxes from a values
// file. It finds or creates the provider for the cluster's OIDC issuer and the
// egress proxy Application with implicit consent, finds, creates or patches
// each resource, stores each secret_env credential, and makes each resource a
// dependency of the Application. For each user it binds the user's
// ServiceAccount to the Application with a token credential. It prints the
// k8s_sandbox and sandbox_user_access values as YAML. With --dry-run it changes
// nothing and prints nothing on stdout.
//
//	zone-setup --values zone-setup.yaml > zone-setup.out.yaml
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/keycardai/keycard-go"
	"github.com/urfave/cli/v3"
	"sigs.k8s.io/yaml"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newCommand().Run(ctx, os.Args)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "zone-setup: %v\n", err)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:            "zone-setup",
		Usage:           "find or create the zone's provider, application, resources and users' token credentials",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "values", Required: true, Usage: "the values file, YAML or JSON"},
			&cli.BoolFlag{Name: "dry-run", Usage: "change nothing and print what would change"},
		},
		Action: setup,
	}
}

type output struct {
	K8sSandbox struct {
		ApplicationID         string `json:"application_id"`
		ApplicationIdentifier string `json:"application_identifier"`
		Namespace             string `json:"namespace"`
		ProviderID            string `json:"provider_id"`
	} `json:"k8s_sandbox"`
	SandboxUserAccess []userAccess `json:"sandbox_user_access,omitempty"`
}

type userAccess struct {
	UserEmail      string `json:"user_email"`
	UserID         string `json:"user_id"`
	ServiceAccount string `json:"service_account"`
	ClientID       string `json:"client_id"`
	Namespace      string `json:"namespace"`
}

func setup(ctx context.Context, cmd *cli.Command) error {
	path := cmd.String("values")
	values, err := readValues(path)
	if err != nil {
		return err
	}
	secrets, err := zonesetup.LoadSecrets(values, os.LookupEnv, os.ReadFile)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	session, err := zonesetup.Connect(ctx, values, secrets)
	if err != nil {
		return fmt.Errorf("resolving the zone: %w", err)
	}
	sayf(cmd, "zone %s issues %s", session.Zone.ID, session.Zone.Protocols.Oauth2.Issuer)
	providerID, err := ensureProvider(ctx, cmd, session, values.IssuerURL)
	if err != nil {
		return err
	}
	identifier := zonesetup.ApplicationIdentifier(values.Namespace)
	applicationID, err := ensureApplication(ctx, cmd, session, identifier, values.Namespace)
	if err != nil {
		return err
	}
	err = ensureResources(ctx, cmd, session, applicationID, values.Resources, secrets.Resource)
	if err != nil {
		return err
	}
	access, err := ensureUserAccess(ctx, cmd, session, applicationID, providerID, values)
	if err != nil {
		return err
	}
	if cmd.Bool("dry-run") {
		sayf(cmd, "dry run: nothing was written")
		return nil
	}
	var out output
	out.K8sSandbox.ApplicationID = applicationID
	out.K8sSandbox.ApplicationIdentifier = identifier
	out.K8sSandbox.Namespace = values.Namespace
	out.K8sSandbox.ProviderID = providerID
	out.SandboxUserAccess = access
	return writeOutput(cmd, out)
}

func writeOutput(cmd *cli.Command, out output) error {
	raw, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("encoding the output: %w", err)
	}
	if _, err = cmd.Writer.Write(raw); err != nil {
		return fmt.Errorf("writing the output: %w", err)
	}
	return nil
}

func ensureProvider(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	issuerURL string,
) (string, error) {
	providerID, err := session.FindProvider(ctx, issuerURL)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the provider: %w", err)
	case providerID != "":
		sayf(cmd, "found provider %s", providerID)
		return providerID, nil
	case cmd.Bool("dry-run"):
		sayf(cmd, "would create a provider for %s", issuerURL)
		return "", nil
	}
	if providerID, err = session.CreateProvider(ctx, issuerURL); err != nil {
		return "", fmt.Errorf("creating the provider: %w", err)
	}
	sayf(cmd, "created provider %s", providerID)
	return providerID, nil
}

func ensureApplication(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	identifier, namespace string,
) (string, error) {
	app, err := session.FindApplication(ctx, identifier)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the application: %w", err)
	case app.ID == "" && cmd.Bool("dry-run"):
		sayf(cmd, "would create application %s", identifier)
		return "", nil
	case app.ID == "":
		applicationID, err := session.CreateApplication(ctx, identifier, namespace)
		if err != nil {
			return "", fmt.Errorf("creating the application: %w", err)
		}
		sayf(cmd, "created application %s", applicationID)
		return applicationID, nil
	case app.Consent == keycard.ApplicationConsentImplicit:
		sayf(cmd, "found application %s", app.ID)
		return app.ID, nil
	case cmd.Bool("dry-run"):
		sayf(cmd, "would set consent to implicit on application %s", app.ID)
		return app.ID, nil
	}
	if err := session.SetApplicationConsentImplicit(ctx, app.ID); err != nil {
		return "", fmt.Errorf("setting consent on the application: %w", err)
	}
	sayf(cmd, "set consent to implicit on application %s", app.ID)
	return app.ID, nil
}

func ensureResources(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	applicationID string,
	resources []zonesetup.ResourceValues,
	tokens map[string]string,
) error {
	ids := make([]string, 0, len(resources))
	for _, resource := range resources {
		providerID, err := session.ResourceProvider(ctx, resource)
		if err != nil {
			return fmt.Errorf("finding the credential provider for %s: %w", resource.Identifier, err)
		}
		resourceID, err := ensureResource(ctx, cmd, session, resource, providerID)
		if err != nil {
			return err
		}
		ids = append(ids, resourceID)
		if resource.SecretEnv == "" {
			continue
		}
		token, ok := tokens[resource.SecretEnv]
		if !ok {
			return fmt.Errorf("%w: %s", zonesetup.ErrMissingSecret, resource.SecretEnv)
		}
		if err = ensureSecret(ctx, cmd, session, resource, resourceID, token); err != nil {
			return err
		}
	}
	return addMissingDependencies(ctx, cmd, session, applicationID, resources, ids)
}

func ensureResource(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	resource zonesetup.ResourceValues,
	providerID string,
) (string, error) {
	found, err := session.FindResource(ctx, resource.Identifier)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the resource %s: %w", resource.Identifier, err)
	case found.ID == "" && cmd.Bool("dry-run"):
		sayf(cmd, "would create a resource for %s", resource.Identifier)
		return "", nil
	case found.ID == "":
		resourceID, err := session.CreateResource(ctx, resource, providerID)
		if err != nil {
			return "", fmt.Errorf("creating the resource %s: %w", resource.Identifier, err)
		}
		sayf(cmd, "created resource %s for %s", resourceID, resource.Identifier)
		return resourceID, nil
	case found.Prefix == resource.Prefix && found.CredentialProviderID == providerID:
		sayf(cmd, "found resource %s for %s", found.ID, resource.Identifier)
		return found.ID, nil
	case cmd.Bool("dry-run"):
		sayf(cmd, "would patch resource %s for %s", found.ID, resource.Identifier)
		return found.ID, nil
	}
	if err := session.PatchResource(ctx, found.ID, resource.Prefix, providerID); err != nil {
		return "", fmt.Errorf("patching the resource %s: %w", resource.Identifier, err)
	}
	sayf(cmd, "patched resource %s for %s", found.ID, resource.Identifier)
	return found.ID, nil
}

func ensureSecret(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	resource zonesetup.ResourceValues,
	resourceID, token string,
) error {
	if resourceID == "" {
		sayf(cmd, "would store the %s credential for %s", resource.SecretEnv, resource.Identifier)
		return nil
	}
	secretID, err := session.FindSecret(ctx, resourceID)
	switch {
	case err != nil:
		return fmt.Errorf("finding the credential for %s: %w", resource.Identifier, err)
	case secretID == "" && cmd.Bool("dry-run"):
		sayf(cmd, "would store the %s credential for %s", resource.SecretEnv, resource.Identifier)
	case secretID == "":
		if err := session.CreateSecret(ctx, resourceID, resource.SecretEnv, token); err != nil {
			return fmt.Errorf("storing the credential for %s: %w", resource.Identifier, err)
		}
		sayf(cmd, "stored the %s credential for %s", resource.SecretEnv, resource.Identifier)
	case cmd.Bool("dry-run"):
		sayf(cmd, "would update the %s credential for %s", resource.SecretEnv, resource.Identifier)
	default:
		if err := session.UpdateSecret(ctx, secretID, token); err != nil {
			return fmt.Errorf("updating the credential for %s: %w", resource.Identifier, err)
		}
		sayf(cmd, "updated the %s credential for %s", resource.SecretEnv, resource.Identifier)
	}
	return nil
}

func addMissingDependencies(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	applicationID string,
	resources []zonesetup.ResourceValues,
	resourceIDs []string,
) error {
	has := map[string]bool{}
	if applicationID != "" && len(resourceIDs) > 0 {
		var err error
		if has, err = session.ApplicationDependencies(ctx, applicationID); err != nil {
			return fmt.Errorf("listing the application's dependencies: %w", err)
		}
	}
	for i, resourceID := range resourceIDs {
		if resourceID == "" || applicationID == "" {
			sayf(cmd, "would add the resource for %s as a dependency", resources[i].Identifier)
			continue
		}
		if err := ensureDependency(ctx, cmd, session, applicationID, resourceID, has[resourceID]); err != nil {
			return err
		}
	}
	return nil
}

func ensureDependency(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	applicationID, resourceID string,
	has bool,
) error {
	switch {
	case has:
		sayf(cmd, "found resource %s as a dependency", resourceID)
	case cmd.Bool("dry-run"):
		sayf(cmd, "would add resource %s as a dependency", resourceID)
	default:
		if err := session.AddApplicationDependency(ctx, applicationID, resourceID); err != nil {
			return fmt.Errorf("adding a dependency: %w", err)
		}
		sayf(cmd, "added resource %s as a dependency", resourceID)
	}
	return nil
}

func ensureUserAccess(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	applicationID, providerID string,
	values *zonesetup.Values,
) ([]userAccess, error) {
	access := make([]userAccess, 0, len(values.Users))
	for _, user := range values.Users {
		userID, err := session.FindUser(ctx, user.Email)
		if err != nil {
			return nil, fmt.Errorf("finding the user %s: %w", user.Email, err)
		}
		sayf(cmd, "found user %s for %s", userID, user.Email)
		subject := zonesetup.ServiceAccountSubject(values.Namespace, userID)
		credentialID, err := ensureTokenCredential(ctx, cmd, session, applicationID, providerID, subject)
		if err != nil {
			return nil, err
		}
		access = append(access, userAccess{
			UserEmail:      user.Email,
			UserID:         userID,
			ServiceAccount: subject,
			ClientID:       credentialID,
			Namespace:      values.Namespace,
		})
	}
	return access, nil
}

func ensureTokenCredential(
	ctx context.Context,
	cmd *cli.Command,
	session *zonesetup.Session,
	applicationID, providerID, subject string,
) (string, error) {
	if applicationID == "" || providerID == "" {
		sayf(cmd, "would create a token credential for %s", subject)
		return "", nil
	}
	credentialID, err := session.FindTokenCredential(ctx, applicationID, providerID, subject)
	switch {
	case err != nil:
		return "", fmt.Errorf("finding the token credential for %s: %w", subject, err)
	case credentialID != "":
		sayf(cmd, "found token credential %s for %s", credentialID, subject)
		return credentialID, nil
	case cmd.Bool("dry-run"):
		sayf(cmd, "would create a token credential for %s", subject)
		return "", nil
	}
	if credentialID, err = session.CreateTokenCredential(ctx, applicationID, providerID, subject); err != nil {
		return "", fmt.Errorf("creating the token credential for %s: %w", subject, err)
	}
	sayf(cmd, "created token credential %s for %s", credentialID, subject)
	return credentialID, nil
}

func sayf(cmd *cli.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.ErrWriter, format+"\n", args...)
}

func readValues(path string) (*zonesetup.Values, error) {
	file, err := os.Open(path) //nolint:gosec // G304: the path is the values file the operator named.
	if err != nil {
		return nil, fmt.Errorf("%w: %w", zonesetup.ErrValues, err)
	}
	defer func() { _ = file.Close() }()
	values, err := zonesetup.ParseValues(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return values, nil
}
