// Command sandbox sets up the sandbox demo on a local k3d cluster against a
// Keycard zone. Each subcommand is one step, and each step reads its settings
// from local/local.env. docs/local.md lists the steps in order.
//
//	go run ./cmd/sandbox tunnel        # in a terminal of its own
//	go run ./cmd/sandbox bootstrap
//	go run ./cmd/sandbox mirror        # in another terminal
//	go run ./cmd/sandbox check-issuer
//	go run ./cmd/sandbox down
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/keycardai/agent-sandbox-poc/internal/pinned"
	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

const defaultListen = "127.0.0.1:8480"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newCommand().Run(ctx, os.Args)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: %v\n", err)
		os.Exit(1)
	}
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:            "sandbox",
		Usage:           "run an AI agent in a Kubernetes sandbox behind the Keycard egress proxy",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "env", Usage: "settings file (default local/local.env)"},
		},
		Commands: []*cli.Command{
			{
				Name:  "tunnel",
				Usage: "publish the mirror at the issuer's https URL through a Cloudflare tunnel, until interrupted",
				Flags: []cli.Flag{listenFlag()},
				Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
					return local.Tunnel(ctx, cmd.String("listen"))
				}),
			},
			{
				Name:  "bootstrap",
				Usage: "create the k3d cluster with the issuer, or check that an existing one has it",
				Flags: []cli.Flag{
					clusterFlag(), issuerFlag(),
					&cli.BoolFlag{Name: "dry-run", Usage: "check the settings and print the k3d command, without running it"},
				},
				Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
					return local.Bootstrap(ctx, cmd.String("cluster"), cmd.String("issuer"), cmd.Bool("dry-run"))
				}),
			},
			{
				Name:  "mirror",
				Usage: "serve the cluster's issuer discovery document and JWKS, until interrupted",
				Flags: []cli.Flag{clusterFlag(), issuerFlag(), listenFlag()},
				Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
					return local.Mirror(ctx, cmd.String("cluster"), cmd.String("issuer"), cmd.String("listen"))
				}),
			},
			{
				Name:  "check-issuer",
				Usage: "verify a new ServiceAccount token against the published issuer, as the zone does",
				Flags: []cli.Flag{clusterFlag(), issuerFlag()},
				Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
					return local.CheckIssuer(ctx, cmd.String("cluster"), cmd.String("issuer"))
				}),
			},
			{
				Name:  "down",
				Usage: "delete the k3d cluster, after asking unless --yes. The zone's entities stay.",
				Flags: []cli.Flag{
					clusterFlag(),
					&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "delete without asking"},
				},
				Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
					return local.Down(ctx, cmd.String("cluster"), cmd.Bool("yes"), cmd.Reader)
				}),
			},
		},
	}
}

func clusterFlag() *cli.StringFlag {
	return &cli.StringFlag{Name: "cluster", Usage: "the k3d cluster; " + sandbox.KeyCluster + " when empty"}
}

func issuerFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:  "issuer",
		Usage: "issuer URL (default " + sandbox.KeyIssuer + ", or the running quick tunnel's URL)",
	}
}

func listenFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:  "listen",
		Value: defaultListen,
		Usage: "address the mirror serves on and the tunnel forwards to",
	}
}

// step returns an action that runs one step on a Local built from the
// repository and the settings file.
func step(run func(context.Context, *cli.Command, *sandbox.Local) error) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		local, err := newLocal(cmd)
		if err != nil {
			return err
		}
		return run(ctx, cmd, local)
	}
}

func newLocal(cmd *cli.Command) (*sandbox.Local, error) {
	root, err := pinned.Root()
	if err != nil {
		return nil, fmt.Errorf("finding the repository: %w", err)
	}
	settings, err := sandbox.LoadSettings(root, cmd.String("env"), os.Getenv)
	if err != nil {
		return nil, fmt.Errorf("loading the settings: %w", err)
	}
	return &sandbox.Local{Root: root, Settings: settings, Tool: pinned.Command, Out: cmd.Writer}, nil
}
