// Command sandbox runs the sandbox demo: an agent in a Kubernetes sandbox
// whose HTTPS goes through the Keycard egress proxy. Each subcommand is one
// step, and each step reads its settings from local/local.env.
// docs/local.md describes the steps. `up` runs them all:
//
//	go run ./cmd/sandbox up
//
// or one at a time:
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
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/keycardai/agent-sandbox-examples/internal/install"
	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

var errNotInitialized = errors.New("there are no settings yet. " +
	"Run go run ./cmd/sandbox init in a terminal to create local/local.env and zone-setup.yaml")

const (
	defaultListen     = "127.0.0.1:8480"
	defaultIssuerWait = 2 * time.Minute
	// The help lists the subcommands under these headings, sorted by name.
	startCategory = "Get started"
	stepCategory  = "Steps, one at a time"
)

// intro is what `sandbox` prints with no subcommand.
const intro = `This runs an AI agent in a Kubernetes sandbox. The agent's HTTPS requests
go through the Keycard egress proxy, which adds the user's credentials from
your Keycard zone.

To set up the demo and run it:

    go run ./cmd/sandbox up

The first time, up asks for your Keycard service account, zone and user.
Then it runs each step and prints the command for it, so you can run the
steps yourself later. To list the steps, run go run ./cmd/sandbox --help.

`

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
		Action:          start,
		// A values file path may hold a comma.
		DisableSliceFlagSeparator: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "env", Usage: "settings file (default local/local.env)"},
		},
		Commands: []*cli.Command{
			initCommand(),
			upCommand(),
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

// start prints intro. On a terminal, it offers to run up.
func start(ctx context.Context, cmd *cli.Command) error {
	_, _ = fmt.Fprint(cmd.Writer, intro)
	if !terminal(os.Stdin) {
		return nil
	}
	run, err := terminalAsker{}.Confirm("Run go run ./cmd/sandbox up now?", true)
	if err != nil || !run {
		return nil //nolint:nilerr // Declining or pressing Ctrl-C ends the intro.
	}
	args := []string{cmd.Name}
	if env := cmd.String("env"); env != "" {
		args = append(args, "--env", env)
	}
	if err = newCommand().Run(ctx, append(args, "up")); err != nil {
		return fmt.Errorf("up: %w", err)
	}
	return nil
}

func initCommand() *cli.Command {
	return &cli.Command{
		Name:     "init",
		Category: startCategory,
		Usage:    "ask for your Keycard service account, zone and user, and write the settings files",
		Action: step(func(ctx context.Context, _ *cli.Command, local *sandbox.Local) error {
			if _, err := local.Init(ctx, terminalAsker{}); err != nil {
				return fmt.Errorf("init: %w", err)
			}
			return nil
		}),
	}
}

func upCommand() *cli.Command {
	return &cli.Command{
		Name:     "up",
		Category: startCategory,
		Usage:    "set up and run the whole demo, and print the command for each step",
		Flags: []cli.Flag{
			listenFlag(),
			&cli.BoolFlag{Name: "print", Usage: "print the commands and do not run them"},
			&cli.DurationFlag{
				Name:  "issuer-wait",
				Value: defaultIssuerWait,
				Usage: "how long to wait for the cluster's issuer to answer",
			},
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
			replace := cmd.Bool(zonesetup.ReplaceActiveFlag)
			if cmd.String("env") == "" && !cmd.Bool("print") && !local.Initialized() {
				if !terminal(os.Stdin) {
					return errNotInitialized
				}
				result, err := local.Init(ctx, terminalAsker{})
				if err != nil {
					return fmt.Errorf("init: %w", err)
				}
				if local, err = newLocal(cmd); err != nil {
					return err
				}
				replace = replace || result.ReplaceActive
			}
			return local.Up(ctx, sandbox.UpOptions{
				Run: sandbox.RunOptions{
					SkipZoneSetup: cmd.Bool("skip-zone-setup"),
					ReplaceActive: replace,
				},
				Listen:     cmd.String("listen"),
				EnvFile:    cmd.String("env"),
				Print:      cmd.Bool("print"),
				IssuerWait: cmd.Duration("issuer-wait"),
				Answers:    cmd.Reader,
			})
		}),
	}
}

func tunnelCommand() *cli.Command {
	return &cli.Command{
		Name:     "tunnel",
		Category: stepCategory,
		Usage:    "publish the cluster's issuer on the internet through a Cloudflare tunnel, until stopped",
		Flags:    []cli.Flag{listenFlag()},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.Tunnel(ctx, cmd.String("listen"))
		}),
	}
}

func bootstrapCommand() *cli.Command {
	return &cli.Command{
		Name:     "bootstrap",
		Category: stepCategory,
		Usage:    "create the local k3d cluster, or check the one you have",
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
		Name:     "mirror",
		Category: stepCategory,
		Usage:    "serve the cluster's issuer documents for the tunnel, until stopped",
		Flags:    []cli.Flag{clusterFlag(), issuerFlag(), listenFlag()},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.Mirror(ctx, cmd.String("cluster"), cmd.String("issuer"), cmd.String("listen"))
		}),
	}
}

func checkIssuerCommand() *cli.Command {
	return &cli.Command{
		Name:     "check-issuer",
		Category: stepCategory,
		Usage:    "check that a Keycard zone can verify the cluster's tokens",
		Flags:    []cli.Flag{clusterFlag(), issuerFlag()},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.CheckIssuer(ctx, cmd.String("cluster"), cmd.String("issuer"))
		}),
	}
}

func runCommand() *cli.Command {
	return &cli.Command{
		Name:     "run",
		Category: stepCategory,
		Usage:    "set up the cluster and Keycard, install the sandbox and the egress proxy, and run the checks",
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
		Name:     "check",
		Category: stepCategory,
		Usage:    "run the checks again against the running sandbox",
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
		Name:     "down",
		Category: stepCategory,
		Usage:    "delete the local k3d cluster. The Keycard zone is not changed",
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
	return &cli.StringFlag{
		Name: "cluster",
		Usage: "k3d cluster name, or kubeconfig context of an existing cluster (default " +
			sandbox.KeyCluster + " or " + sandbox.KeyKubeContext + ")",
	}
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

// terminal reports whether file is a terminal.
func terminal(file *os.File) bool {
	return term.IsTerminal(int(file.Fd()))
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
	return &sandbox.Local{
		Root: root, Settings: settings, Tool: sandbox.Tool, Out: cmd.Writer, Getenv: os.Getenv,
	}, nil
}
