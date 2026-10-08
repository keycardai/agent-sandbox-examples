// Command zone-setup configures a Keycard zone for the sandboxes from a values
// file. zonesetup.Run lists the steps. It prints the k8s_sandbox and
// sandbox_user_access values as YAML. With --dry-run it changes nothing and
// prints nothing on stdout.
//
//	zone-setup --values zone-setup.yaml > zone-setup.out.yaml
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"
	"sigs.k8s.io/yaml"

	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const issuerURLFlag = "issuer-url"

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
		Usage:           "find or create the zone's provider, application, resources, users' access and policy set",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "values", Required: true, Usage: "the values file, YAML or JSON"},
			&cli.StringFlag{
				Name:  issuerURLFlag,
				Usage: "the cluster's OIDC issuer. It replaces issuer_url in the values file",
			},
			&cli.BoolFlag{Name: "dry-run", Usage: "change nothing and print what would change"},
			&cli.BoolFlag{
				Name: zonesetup.ReplaceActiveFlag,
				Usage: "activate the sandbox policy set even when another policy set is active. " +
					"The other set's policies stop applying",
			},
		},
		Action: setup,
	}
}

func setup(ctx context.Context, cmd *cli.Command) error {
	path := cmd.String("values")
	values, err := zonesetup.ReadValuesFile(path, cmd.String(issuerURLFlag), "--"+issuerURLFlag)
	if err != nil {
		return fmt.Errorf("reading the values: %w", err)
	}
	secrets, err := zonesetup.LoadSecrets(values, os.LookupEnv, os.ReadFile)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	opts := &zonesetup.Options{
		DryRun:        cmd.Bool("dry-run"),
		ReplaceActive: cmd.Bool(zonesetup.ReplaceActiveFlag),
		Log:           cmd.ErrWriter,
	}
	out, err := zonesetup.Run(ctx, values, secrets, opts)
	if err != nil {
		return fmt.Errorf("setting up the zone: %w", err)
	}
	if opts.DryRun {
		return nil
	}
	raw, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("encoding the output: %w", err)
	}
	if _, err = cmd.Writer.Write(raw); err != nil {
		return fmt.Errorf("writing the output: %w", err)
	}
	return nil
}
