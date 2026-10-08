// Command new-sandbox creates one sandbox for one user. It renders the
// sandbox chart and applies the result with kubectl. sandbox/README.md lists
// the objects it creates.
//
// The identity flags take the zone-setup outputs, under the names zone-setup
// prints.
//
// With --dry-run, it renders and does not apply. The manifests go to stdout
// and progress goes to stderr.
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

	"github.com/keycardai/agent-sandbox-poc/internal/chart"
	"github.com/keycardai/agent-sandbox-poc/internal/newsandbox"
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
	// Turn off kuberc. A kuberc default for --server, --cluster, --user or
	// --token would override the cluster and credentials of the --kube-context
	// context.
	if err := os.Setenv("KUBERC", "off"); err != nil {
		fmt.Fprintf(os.Stderr, "new-sandbox: %v\n", err)
		return 1
	}
	if err := newCommand(os.Stdout, os.Stderr).Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "new-sandbox: %v\n", err)
		return 1
	}
	return 0
}

func newCommand(stdout, stderr io.Writer) *cli.Command {
	return &cli.Command{
		Name:  "new-sandbox",
		Usage: "create one sandbox for one user from the zone-setup outputs",
		Description: "Renders the sandbox chart and applies it: a ServiceAccount named for the\n" +
			"user's public id, the proxy's configuration document, and the Sandbox. The\n" +
			"identity flags take the zone-setup outputs by name.",
		Writer:          stdout,
		ErrWriter:       stderr,
		HideHelpCommand: true,
		Flags:           append(identityFlags(), renderFlags()...),
		Action:          run,
	}
}

// identityFlags take the zone-setup outputs, under the names zone-setup
// prints. The k8s_sandbox ids go in annotations on every object, to record
// the zone entities the sandbox depends on.
func identityFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "namespace",
			Aliases: []string{"n"},
			Value:   zonesetup.DefaultNamespace,
			Usage:   "the sandbox namespace (k8s_sandbox.namespace)",
		},
		&cli.StringFlag{Name: "user-id", Usage: "the IAM user public id (sandbox_user_access.user_id)"},
		&cli.StringFlag{Name: "user-email", Usage: "the user's email (sandbox_user_access.user_email)"},
		&cli.StringFlag{
			Name:  "service-account",
			Usage: "the subject the credential is bound to (sandbox_user_access.service_account)",
		},
		&cli.StringFlag{
			Name:  "client-id",
			Usage: "the id of the workload-identity credential (sandbox_user_access.client_id)",
		},
		&cli.StringFlag{Name: "provider-id", Usage: "the cluster's OIDC provider (k8s_sandbox.provider_id)"},
		&cli.StringFlag{Name: "application-id", Usage: "the egress proxy application (k8s_sandbox.application_id)"},
		&cli.StringFlag{
			Name:  "application-identifier",
			Usage: "the application's well-known identifier (k8s_sandbox.application_identifier)",
		},
		&cli.StringFlag{
			Name:  "zone-url",
			Usage: "the zone that decides every token exchange, if the values file does not set it",
		},
	}
}

func renderFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringSliceFlag{
			Name:    "values",
			Aliases: []string{"f"},
			Usage:   "a values file with the endpoints, the cluster ranges and the zone URL; repeatable",
		},
		&cli.StringFlag{Name: "chart", Value: "./sandbox", Usage: "the sandbox chart"},
		&cli.StringFlag{Name: "name", Usage: "the Sandbox name; it defaults to the user's public id"},
		&cli.StringFlag{Name: "kube-context", Usage: "the kubeconfig context"},
		&cli.BoolFlag{Name: "dry-run", Usage: "render to stdout and do not apply"},
		&cli.DurationFlag{Name: "timeout", Value: defaultTimeout, Usage: "give up after this long"},
	}
}

func run(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() > 0 {
		return fmt.Errorf("%w %q", errUnexpectedArgument, cmd.Args().First())
	}
	ctx, cancel := context.WithTimeout(ctx, cmd.Duration("timeout"))
	defer cancel()

	sandbox := &newsandbox.Command{
		Inputs: newsandbox.Inputs{
			Namespace:             cmd.String("namespace"),
			ProviderID:            cmd.String("provider-id"),
			ApplicationID:         cmd.String("application-id"),
			ApplicationIdentifier: cmd.String("application-identifier"),
			UserEmail:             cmd.String("user-email"),
			UserID:                cmd.String("user-id"),
			ServiceAccount:        cmd.String("service-account"),
			ClientID:              cmd.String("client-id"),
			ZoneURL:               cmd.String("zone-url"),
		},
		ChartDir:    cmd.String("chart"),
		Name:        cmd.String("name"),
		ValuesFiles: cmd.StringSlice("values"),
		Apply:       !cmd.Bool("dry-run"),
		Kube:        kubectl(cmd.String("kube-context")),
		Log:         cmd.ErrWriter,
	}
	manifests, err := sandbox.Run(ctx)
	if err != nil {
		return fmt.Errorf("creating the sandbox: %w", err)
	}
	if _, err = cmd.Writer.Write(manifests); err != nil {
		return fmt.Errorf("writing the manifests: %w", err)
	}
	return nil
}

// kubectl runs the pinned kubectl in one kubeconfig context.
func kubectl(kubeContext string) newsandbox.Runner {
	return func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		if kubeContext != "" {
			args = append([]string{"--context", kubeContext}, args...)
		}
		return chart.Pinned(ctx, stdin, "kubectl", args...)
	}
}
