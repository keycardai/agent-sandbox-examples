// Command zone-setup creates the zone entities the Kubernetes Sandbox and
// Sandbox User Access catalog items create, from one values file, through
// the Keycard management API. It is the manual path when the catalog is not
// available and the executable specification of what the catalog produces.
//
// It is idempotent. An entity that already matches is left alone, one whose
// settings disagree with the values file is patched, and nothing is deleted.
// --dry-run reports what each step would do and writes nothing.
//
// Two different credentials are in play and NEVER interchangeable:
//
//   - The organization service account in the values file authenticates this
//     tool to the management API. The zone's token endpoint rejects it.
//   - The workload-identity credential this tool creates is what the sidecar
//     authenticates its token exchange with. It has no secret. The cluster's
//     ServiceAccount token is the proof.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"sigs.k8s.io/yaml"

	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

// defaultTimeout bounds a whole run.
const defaultTimeout = 5 * time.Minute

var errUnexpectedArgument = errors.New("unexpected argument")

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newCommand(os.Stdout, os.Stderr).Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "zone-setup: %v\n", err)
		return 1
	}
	return 0
}

func newCommand(stdout, stderr io.Writer) *cli.Command {
	return &cli.Command{
		Name:  "zone-setup",
		Usage: "create or correct the zone entities a values file describes, and print the ids new-sandbox needs",
		Description: "Creates the OIDC provider, the egress proxy application, its resources and their\n" +
			"dependencies, and one workload-identity credential and impersonation policy per\n" +
			"user, through the Keycard management API. Safe to run twice: what matches is\n" +
			"left alone, what disagrees with the values file is patched, nothing is deleted.",
		Writer:          stdout,
		ErrWriter:       stderr,
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "values",
				Aliases: []string{"f"},
				Value:   "zone-setup.yaml",
				Usage:   "the values file, YAML or JSON",
				Sources: cli.EnvVars("ZONE_SETUP_VALUES"),
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "report what each step would do and write nothing",
			},
			&cli.BoolFlag{
				Name: "replace-active-policy-set",
				Usage: "activate the sandbox policy set even when another policy set is active in the zone, " +
					"replacing its binding",
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Value: defaultTimeout,
				Usage: "give up after this long",
			},
		},
		Action: run,
	}
}

func run(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() > 0 {
		return fmt.Errorf("%w %q", errUnexpectedArgument, cmd.Args().First())
	}
	stdout, stderr := cmd.Writer, cmd.ErrWriter

	values, err := loadValues(cmd.String("values"))
	if err != nil {
		return err
	}
	secrets, err := zonesetup.LoadSecrets(values, os.LookupEnv, os.ReadFile)
	if err != nil {
		return fmt.Errorf("reading secrets: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, cmd.Duration("timeout"))
	defer cancel()

	setup := &zonesetup.Setup{
		Values:                 values,
		Secrets:                secrets,
		DryRun:                 cmd.Bool("dry-run"),
		ReplaceActivePolicySet: cmd.Bool("replace-active-policy-set"),
		Log:                    stderr,
	}
	out, err := setup.Run(ctx)
	if err != nil {
		return fmt.Errorf("setting up the zone: %w", err)
	}
	if setup.DryRun {
		_, _ = fmt.Fprintln(stderr, "zone-setup: dry run complete. Nothing was written")
		return nil
	}
	doc, err := yaml.Marshal(out)
	if err != nil {
		return fmt.Errorf("encoding the outputs: %w", err)
	}
	if _, err = stdout.Write(doc); err != nil {
		return fmt.Errorf("writing the outputs: %w", err)
	}
	return nil
}

func loadValues(path string) (*zonesetup.Values, error) {
	//nolint:gosec // G304: the path is the --values flag, and the operator names the file to read.
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the values file: %w", err)
	}
	defer func() { _ = file.Close() }()
	values, err := zonesetup.ParseValues(file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return values, nil
}
