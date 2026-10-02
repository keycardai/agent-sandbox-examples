// Command zone-setup configures a Keycard zone for the sandboxes from a values
// file. It finds or creates the zone's provider for the cluster's OIDC issuer
// and the egress proxy Application, with implicit consent, and prints their
// ids under k8s_sandbox in YAML. With --dry-run it changes nothing and prints
// nothing on stdout.
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
		Usage:           "find or create the zone's provider and egress proxy application",
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
		ProviderID            string `json:"provider_id"`
	} `json:"k8s_sandbox"`
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
	if cmd.Bool("dry-run") {
		sayf(cmd, "dry run: nothing was written")
		return nil
	}
	var out output
	out.K8sSandbox.ApplicationID = applicationID
	out.K8sSandbox.ApplicationIdentifier = identifier
	out.K8sSandbox.ProviderID = providerID
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
