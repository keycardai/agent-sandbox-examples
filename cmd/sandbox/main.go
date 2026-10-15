// Command sandbox runs the sandbox demo: an agent in a Kubernetes sandbox
// whose HTTPS goes through the Keycard egress proxy. On a terminal, with no
// subcommand, it opens the TUI, which sets the sandbox up, brings it up and
// shows it:
//
//	go run ./cmd/sandbox
//
// Each subcommand is one step, and each step reads its settings from
// ~/.config/keycard/sandbox.yaml, which setup saves when you ask it to, and
// from the environment. docs/local.md describes the steps. `up` runs them all
// without the TUI:
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
// not read the settings.
//
//	go run ./cmd/sandbox install --context NAME --values FILE
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/urfave/cli/v3"

	"github.com/keycardai/agent-sandbox-examples/internal/install"
	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

// errNotInitialized means a step ran before setup, without a terminal to
// run setup on.
var errNotInitialized = errors.New("there are no settings yet")

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

On a terminal, go run ./cmd/sandbox opens the TUI, which sets the sandbox
up, brings it up and shows it. To run every step without the TUI, run:

    go run ./cmd/sandbox up

On a terminal, up asks the first time for your Keycard service account, the
zone and who the agent acts as, and whether to save the settings.
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
			&cli.StringFlag{
				Name:    "config",
				Usage:   "settings file (default ~/.config/keycard/sandbox.yaml). - reads the environment alone",
				Sources: cli.EnvVars(sandbox.EnvConfig),
			},
		},
		Commands: []*cli.Command{
			initCommand(),
			upCommand(),
			forgetCommand(),
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

// start opens the TUI on a terminal. Elsewhere it prints intro. It refuses
// an argument, such as a mistyped subcommand, which would otherwise open the
// TUI.
func start(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Present() {
		return fmt.Errorf("%w %q. Run go run ./cmd/sandbox --help to list the subcommands",
			errUnexpectedArgument, cmd.Args().First())
	}
	if !onTerminal() || !term.IsTerminal(os.Stdout.Fd()) {
		_, _ = fmt.Fprint(cmd.Writer, intro)
		return nil
	}
	local, err := newLocal(cmd)
	if err != nil {
		return err
	}
	session := local.NewSession(defaultListen, defaultIssuerWait)
	if err = tui.Run(ctx, tui.Options{Session: session}); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

func initCommand() *cli.Command {
	return &cli.Command{
		Name:     "init",
		Category: startCategory,
		Usage:    "ask for your Keycard service account, the zone and who the agent acts as, and save the settings",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := noArgs(cmd); err != nil {
				return err
			}
			local, err := loadLocal(cmd, false)
			if err != nil {
				return err
			}
			_, err = runInit(ctx, local)
			return err
		},
	}
}

func forgetCommand() *cli.Command {
	return &cli.Command{
		Name:     "forget",
		Category: startCategory,
		Usage:    "delete the saved settings and the secrets setup put in the keychain",
		Action: func(_ context.Context, cmd *cli.Command) error {
			if err := noArgs(cmd); err != nil {
				return err
			}
			local, err := loadLocal(cmd, false)
			if err != nil {
				return err
			}
			_, statErr := os.Stat(local.Store.Path())
			saved := statErr == nil
			deleted, err := local.Forget()
			if err != nil {
				return fmt.Errorf("forget: %w", err)
			}
			path := sandbox.HomeRelative(local.Store.Path())
			if saved {
				_, _ = fmt.Fprintf(cmd.Writer, "Deleted %s.\n", path)
			} else {
				_, _ = fmt.Fprintf(cmd.Writer, "There are no saved settings in %s.\n", path)
			}
			_, _ = fmt.Fprintf(cmd.Writer, "Deleted %d secrets from %s.\n", deleted, local.Store.KeychainWhere())
			return nil
		},
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
					return fmt.Errorf("%w. Run %s in a terminal, and save the settings",
						errNotInitialized, local.Command("init"))
				}
				result, err := runInit(ctx, local)
				if err != nil {
					return err
				}
				local.UseConfig()
				run.ReplaceActive = run.ReplaceActive || result.ReplaceActive
			}
			return local.Up(ctx, sandbox.UpOptions{
				Run:        run,
				Listen:     cmd.String("listen"),
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
		Usage: "stop the tunnel and the mirror, and delete the local k3d cluster. " +
			"The Keycard zone is not changed",
		Flags: []cli.Flag{
			clusterFlag(),
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "delete without asking"},
		},
		Action: step(func(ctx context.Context, cmd *cli.Command, local *sandbox.Local) error {
			// Down asks first, so an answer of no leaves everything running.
			if err := local.Down(ctx, cmd.String("cluster"), cmd.Bool("yes")); err != nil {
				return err //nolint:wrapcheck // Down's errors say what failed, as the other steps' do.
			}
			for _, name := range []string{sandbox.ServiceMirror, sandbox.ServiceTunnel} {
				if err := local.StopService(ctx, name); err == nil {
					_, _ = fmt.Fprintf(cmd.Writer, "Stopped the %s.\n", name)
				} else if !errors.Is(err, sandbox.ErrNotRunning) {
					return fmt.Errorf("stopping the %s: %w", name, err)
				}
			}
			return nil
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
		return result, fmt.Errorf("init: %w. Nothing was saved", err)
	}
	if err != nil {
		return result, fmt.Errorf("init: %w", err)
	}
	return result, nil
}

func newLocal(cmd *cli.Command) (*sandbox.Local, error) {
	return loadLocal(cmd, true)
}

// configPath returns the settings file --config names, as an absolute path,
// or the default one, or NoConfig.
func configPath(cmd *cli.Command) (string, error) {
	path := cmd.String("config")
	var err error
	switch path {
	case "":
		path, err = sandbox.DefaultConfigPath(os.Getenv)
	case sandbox.NoConfig:
	default:
		path, err = filepath.Abs(path)
	}
	if err != nil {
		return "", fmt.Errorf("finding the settings file: %w", err)
	}
	return path, nil
}

// loadLocal returns the checkout and its settings. With strict unset, it
// ignores a settings file that does not parse, so init can replace the file
// and forget can delete it.
func loadLocal(cmd *cli.Command, strict bool) (*sandbox.Local, error) {
	root, err := pinned.Root()
	if err != nil {
		return nil, fmt.Errorf("finding the repository: %w", err)
	}
	path, err := configPath(cmd)
	if err != nil {
		return nil, err
	}
	config, err := sandbox.LoadConfig(path)
	switch {
	case err != nil && strict:
		return nil, fmt.Errorf("loading the settings: %w", err)
	case err != nil:
		_, _ = fmt.Fprintf(cmd.Writer, "Ignoring the settings, which do not parse: %v\n", err)
	}
	var keychain sandbox.Keychain
	if path != sandbox.NoConfig {
		keychain = sandbox.NewOSKeychain(path)
	}
	local := &sandbox.Local{
		Root: root, Settings: sandbox.SettingsFrom(config, os.Getenv), Store: sandbox.NewStore(path, config, keychain),
		Tool: sandbox.Tool, Out: cmd.Writer, Stdin: os.Stdin, Getenv: os.Getenv,
	}
	if onTerminal() {
		local.Ask = terminalAsker{}
	}
	return local, nil
}
