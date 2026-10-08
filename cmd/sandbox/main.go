// Command sandbox sets up the sandbox demo on a local k3d cluster against a
// Keycard zone. Each subcommand is one step, and each step reads its settings
// from local/local.env. docs/local.md lists the steps in order.
//
//	go run ./cmd/sandbox tunnel        # in a terminal of its own
//	go run ./cmd/sandbox mirror        # in another, after the cluster exists
//	go run ./cmd/sandbox check-issuer
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
	listen := &cli.StringFlag{
		Name:  "listen",
		Value: defaultListen,
		Usage: "the mirror's plain HTTP address, which the tunnel forwards to",
	}
	cluster := &cli.StringFlag{Name: "cluster", Usage: "the k3d cluster; " + sandbox.KeyCluster + " when empty"}
	issuerURL := &cli.StringFlag{
		Name:  "issuer",
		Usage: "the https issuer URL; " + sandbox.KeyIssuer + ", or the quick tunnel's URL, when empty",
	}
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
				Flags: []cli.Flag{listen},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					local, err := newLocal(cmd)
					if err != nil {
						return err
					}
					return local.Tunnel(ctx, cmd.String("listen"))
				},
			},
			{
				Name:  "mirror",
				Usage: "serve the cluster's issuer discovery document and JWKS, until interrupted",
				Flags: []cli.Flag{cluster, issuerURL, listen},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					local, err := newLocal(cmd)
					if err != nil {
						return err
					}
					return local.Mirror(ctx, cmd.String("cluster"), cmd.String("issuer"), cmd.String("listen"))
				},
			},
			{
				Name:  "check-issuer",
				Usage: "verify a new ServiceAccount token against the published issuer, as the zone does",
				Flags: []cli.Flag{cluster, issuerURL},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					local, err := newLocal(cmd)
					if err != nil {
						return err
					}
					return local.CheckIssuer(ctx, cmd.String("cluster"), cmd.String("issuer"))
				},
			},
		},
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
