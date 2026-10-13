// Command sandbox runs the sandbox demo: an agent in a Kubernetes sandbox
// whose HTTPS goes through the Keycard egress proxy. Each subcommand is one
// step, and each step reads its settings from local/local.env.
// docs/local.md describes the steps.
//
//	go run ./cmd/sandbox tunnel        # in a terminal of its own
//	go run ./cmd/sandbox bootstrap
//	go run ./cmd/sandbox mirror        # in another terminal
//	go run ./cmd/sandbox check-issuer
//	go run ./cmd/sandbox run
//	go run ./cmd/sandbox down
//
// `install` installs the controller and the chart into any cluster. It does
// not read local.env.
//
//	go run ./cmd/sandbox install --context NAME --values FILE
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/keycardai/agent-sandbox-examples/internal/install"
	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const defaultListen = "127.0.0.1:8480"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
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
			tunnelCommand(),
			bootstrapCommand(),
			mirrorCommand(),
			checkIssuerCommand(),
			runCommand(),
			checkCommand(),
			downCommand(),
			installCommand(),
		},
	}
}

func tunnelCommand() *cli.Command {
	return &cli.Command{
		Name:  "tunnel",
		Usage: "publish the cluster's issuer on the internet through a Cloudflare tunnel, until stopped",
		Flags: []cli.Flag{listenFlag()},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.Tunnel(ctx, cmd.String("listen"))
		}),
	}
}

func bootstrapCommand() *cli.Command {
	return &cli.Command{
		Name:  "bootstrap",
		Usage: "create the local k3d cluster, or check the one you have",
		Flags: []cli.Flag{
			clusterFlag(), issuerFlag(),
			&cli.BoolFlag{Name: "dry-run", Usage: "print the k3d command without running it"},
		},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.Bootstrap(ctx, cmd.String("cluster"), cmd.String("issuer"), cmd.Bool("dry-run"))
		}),
	}
}

func mirrorCommand() *cli.Command {
	return &cli.Command{
		Name:  "mirror",
		Usage: "serve the cluster's issuer documents for the tunnel, until stopped",
		Flags: []cli.Flag{clusterFlag(), issuerFlag(), listenFlag()},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.Mirror(ctx, cmd.String("cluster"), cmd.String("issuer"), cmd.String("listen"))
		}),
	}
}

func checkIssuerCommand() *cli.Command {
	return &cli.Command{
		Name:  "check-issuer",
		Usage: "check that a Keycard zone can verify the cluster's tokens",
		Flags: []cli.Flag{clusterFlag(), issuerFlag()},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.CheckIssuer(ctx, cmd.String("cluster"), cmd.String("issuer"))
		}),
	}
}

func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "set up the cluster and Keycard, install the sandbox and the egress proxy, and run the checks",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "skip-zone-setup",
				Usage: "reuse the Keycard setup of the last run, in local/out/zone-setup.yaml",
			},
			&cli.BoolFlag{
				Name:  zonesetup.ReplaceActiveFlag,
				Usage: "make the sandbox policy set active, replacing the zone's active policy set",
			},
		},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.Run(ctx, sandbox.RunOptions{
				SkipZoneSetup: cmd.Bool("skip-zone-setup"),
				ReplaceActive: cmd.Bool(zonesetup.ReplaceActiveFlag),
			})
		}),
	}
}

func checkCommand() *cli.Command {
	return &cli.Command{
		Name:  "check",
		Usage: "run the checks again against the running sandbox",
		Flags: []cli.Flag{
			clusterFlag(),
			&cli.StringFlag{Name: "namespace", Usage: "sandbox namespace (default from the last run)"},
			&cli.StringFlag{Name: "pod", Usage: "sandbox pod (default the user's id)"},
			&cli.StringFlag{Name: "user-id", Usage: "the user's Keycard id (default from the last run)"},
		},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			target, err := local.CheckTargetFor(
				cmd.String("cluster"), cmd.String("namespace"), cmd.String("pod"), cmd.String("user-id"),
			)
			if err != nil {
				return fmt.Errorf("finding the sandbox: %w", err)
			}
			return local.Check(ctx, target)
		}),
	}
}

func downCommand() *cli.Command {
	return &cli.Command{
		Name:  "down",
		Usage: "delete the local k3d cluster. The Keycard zone is not changed",
		Flags: []cli.Flag{
			clusterFlag(),
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "delete without asking"},
		},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.Down(ctx, cmd.String("cluster"), cmd.Bool("yes"), cmd.Reader)
		}),
	}
}

func installCommand() *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "install the agent-sandbox controller, then the keycard-sandbox chart, into a cluster",
		// A values file path may hold a comma. The command that owns a flag
		// decides whether it splits.
		DisableSliceFlagSeparator: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "context", Required: true, Usage: "the kube context to install into"},
			&cli.StringSliceFlag{
				Name:     "values",
				Required: true,
				Usage:    "the chart's values file. Start from install/values.example.yaml. Repeat it; a later file wins",
			},
			&cli.StringFlag{Name: "namespace", Value: "agents", Usage: "the sandbox namespace"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			root, err := pinned.Root()
			if err != nil {
				return fmt.Errorf("finding the repository: %w", err)
			}
			installer := &install.Install{
				Root:      root,
				Context:   cmd.String("context"),
				Namespace: cmd.String("namespace"),
				Values:    cmd.StringSlice("values"),
				Tool:      sandbox.Tool,
				Out:       cmd.Writer,
			}
			if err = installer.Run(ctx); err != nil {
				return fmt.Errorf("installing into %s: %w", installer.Namespace, err)
			}
			return nil
		},
	}
}

func clusterFlag() *cli.StringFlag {
	return &cli.StringFlag{Name: "cluster", Usage: "k3d cluster (default " + sandbox.KeyCluster + ")"}
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
	return &sandbox.Local{Root: root, Settings: settings, Tool: sandbox.Tool, Out: cmd.Writer}, nil
}
