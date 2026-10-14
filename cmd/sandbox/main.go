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
//	go run ./cmd/sandbox shell         # for a scenario in shell mode
//	go run ./cmd/sandbox logs          # for a scenario in background mode
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

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v3"

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

var errUnexpectedArgument = errors.New("unexpected argument")

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
			shellCommand(),
			logsCommand(),
			downCommand(),
			installCommand(),
		},
	}
}

// start prints intro. On a terminal, it offers to run up.
func start(ctx context.Context, cmd *cli.Command) error {
	_, _ = fmt.Fprint(cmd.Writer, intro)
	if !onTerminal() {
		return nil
	}
	run, err := terminalAsker{}.Confirm(ctx, "Run go run ./cmd/sandbox up now?", true)
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
			_, err := runInit(ctx, local)
			return err
		}),
	}
}

func upCommand() *cli.Command {
	return &cli.Command{
		Name:     "up",
		Category: startCategory,
		Usage:    "set up and run the whole demo, and print the command for each step",
		Flags: append([]cli.Flag{
			listenFlag(),
			&cli.BoolFlag{Name: "print", Usage: "print the commands and do not run them"},
			&cli.DurationFlag{
				Name:  "issuer-wait",
				Value: defaultIssuerWait,
				Usage: "how long to wait for the cluster's issuer to answer",
			},
		}, runFlags()...),
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			run := runOptions(cmd)
			if !cmd.Bool("print") && !local.Initialized() {
				if local.Ask == nil {
					return errNotInitialized
				}
				result, err := runInit(ctx, local)
				if err != nil {
					return err
				}
				if local, err = newLocal(cmd); err != nil {
					return err
				}
				run.ReplaceActive = run.ReplaceActive || result.ReplaceActive
			}
			return local.Up(ctx, sandbox.UpOptions{
				Run:        run,
				Listen:     cmd.String("listen"),
				EnvFile:    cmd.String("env"),
				Print:      cmd.Bool("print"),
				IssuerWait: cmd.Duration("issuer-wait"),
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
		Flags:    runFlags(),
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.Run(ctx, runOptions(cmd))
		}),
	}
}

func checkCommand() *cli.Command {
	return &cli.Command{
		Name:     "check",
		Category: stepCategory,
		Usage:    "run the checks again against the running sandbox",
		Flags:    targetFlags(),
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			return local.CheckSandbox(ctx,
				cmd.String("cluster"), cmd.String("namespace"), cmd.String("pod"), cmd.String("user-id"))
		}),
	}
}

func shellCommand() *cli.Command {
	return &cli.Command{
		Name:     "shell",
		Category: stepCategory,
		Usage:    "run the scenario's command, or sh, in the sandbox's agent container, attached to this terminal",
		Flags:    targetFlags(),
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			target, err := local.CheckTargetFor(
				cmd.String("cluster"), cmd.String("namespace"), cmd.String("pod"), cmd.String("user-id"),
			)
			if err != nil {
				return fmt.Errorf("finding the sandbox: %w", err)
			}
			return local.Shell(ctx, target)
		}),
	}
}

func logsCommand() *cli.Command {
	return &cli.Command{
		Name:     "logs",
		Category: stepCategory,
		Usage:    "follow the agent's log until Ctrl-C or the agent exits",
		Flags:    targetFlags(),
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			target, err := local.CheckTargetFor(
				cmd.String("cluster"), cmd.String("namespace"), cmd.String("pod"), cmd.String("user-id"),
			)
			if err != nil {
				return fmt.Errorf("finding the sandbox: %w", err)
			}
			return local.Logs(ctx, target)
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
			return local.Down(ctx, cmd.String("cluster"), cmd.Bool("yes"))
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
			if err := noArgs(cmd); err != nil {
				return err
			}
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

// targetFlags select the running sandbox a step acts on.
func targetFlags() []cli.Flag {
	return []cli.Flag{
		clusterFlag(),
		&cli.StringFlag{Name: "namespace", Usage: "sandbox namespace (default from the last run)"},
		&cli.StringFlag{Name: "pod", Usage: "sandbox pod (default the user's id)"},
		&cli.StringFlag{Name: "user-id", Usage: "the user's Keycard id (default from the last run)"},
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

// runFlags are the flags of run. up takes them too, and passes them to run.
func runFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:  "skip-zone-setup",
			Usage: "reuse the Keycard setup of the last run, in local/out/zone-setup.yaml",
		},
		&cli.BoolFlag{
			Name:  zonesetup.ReplaceActiveFlag,
			Usage: "make the sandbox policy set active, replacing the zone's active policy set",
		},
	}
}

func runOptions(cmd *cli.Command) sandbox.RunOptions {
	return sandbox.RunOptions{
		SkipZoneSetup: cmd.Bool("skip-zone-setup"),
		ReplaceActive: cmd.Bool(zonesetup.ReplaceActiveFlag),
	}
}

// step returns an action that runs one step on a Local built from the
// repository and the settings file.
func step(run func(context.Context, *cli.Command, *sandbox.Local) error) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		if err := noArgs(cmd); err != nil {
			return err
		}
		local, err := newLocal(cmd)
		if err != nil {
			return err
		}
		return run(ctx, cmd, local)
	}
}

// noArgs refuses a positional argument. Every subcommand takes only flags,
// so `down mycluster` must not act on the cluster from the settings.
func noArgs(cmd *cli.Command) error {
	if cmd.Args().Present() {
		return fmt.Errorf("%w %q. %s takes only flags", errUnexpectedArgument, cmd.Args().First(), cmd.Name)
	}
	return nil
}

// onTerminal reports whether stdin is a terminal. huh reads os.Stdin and
// needs a terminal.
func onTerminal() bool {
	return term.IsTerminal(os.Stdin.Fd())
}

// runInit runs Init. On Ctrl-C it says that the settings files are
// unchanged.
func runInit(ctx context.Context, local *sandbox.Local) (sandbox.InitResult, error) {
	result, err := local.Init(ctx)
	if errors.Is(err, sandbox.ErrInterrupted) {
		return result, fmt.Errorf("init: %w. The settings files are unchanged", err)
	}
	if err != nil {
		return result, fmt.Errorf("init: %w", err)
	}
	return result, nil
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
	local := &sandbox.Local{
		Root: root, Settings: settings, SettingsFile: cmd.String("env"),
		Tool: sandbox.Tool, Out: cmd.Writer, Stdin: os.Stdin, Getenv: os.Getenv,
	}
	if onTerminal() {
		local.Ask = terminalAsker{}
	}
	return local, nil
}
