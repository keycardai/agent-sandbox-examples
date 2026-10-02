// Command sandbox sets up a Keycard sandbox in localdev through svc-catalog
// and runs an agent in it.
//
// With no command it runs a wizard: pick a zone, pick the API servers the
// sandboxes may reach and enter their keys, add users with and without
// access, and run the agent for each. The same steps run as commands with
// flags:
//
//	sandbox setup    --zone ID | --create-zone NAME --resource SLUG=ENV_VAR
//	sandbox add-user --email EMAIL [--no-access]
//	sandbox run      [--email EMAIL] --read OWNER/REPO/PATH
//	sandbox status
//	sandbox down
//
// It signs in as an organization service account from KEYCARD_CLIENT_ID and
// KEYCARD_CLIENT_SECRET, and records what setup did in
// ~/.keycard/sandbox/state.json so later commands need only an email.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

// Exit codes.
const (
	exitOK     = 0
	exitFailed = 1
	// exitCancelled is what a shell reports for a command stopped by ctrl+c.
	exitCancelled = 130
)

var (
	// errReported is a failure the CLI already showed.
	errReported      = errors.New("reported")
	errNeedsTerminal = errors.New("this needs an answer; run it in a terminal or pass the flags")
	errNoCredentials = errors.New(
		"set KEYCARD_CLIENT_ID and KEYCARD_CLIENT_SECRET to an organization service account",
	)
	errLoadState  = errors.New("could not load the state")
	errNoLocaldev = errors.New("is localdev set up?")
)

func main() { os.Exit(run()) }

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := newCommand().Run(ctx, os.Args)
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errCancelled):
		return exitCancelled
	case !errors.Is(err, errReported):
		fmt.Fprintf(os.Stderr, "sandbox: %v\n", err)
	}
	return exitFailed
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:            "sandbox",
		Usage:           "set up a Keycard sandbox in localdev and run an agent in it",
		HideHelpCommand: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "api-url",
				Value:   sandbox.DefaultAPIURL,
				Sources: cli.EnvVars("KEYCARD_API_URL"),
				Usage:   "the management API",
			},
			&cli.StringFlag{
				Name:    "client-id",
				Sources: cli.EnvVars("KEYCARD_CLIENT_ID"),
				Usage:   "the organization service account's client ID",
			},
			&cli.StringFlag{
				Name:    "client-secret",
				Sources: cli.EnvVars("KEYCARD_CLIENT_SECRET"),
				Usage:   "its secret; prefer the environment variable",
			},
			&cli.StringFlag{Name: "state", Value: sandbox.DefaultStatePath(), Usage: "where setup records what it did"},
			&cli.BoolFlag{Name: "no-input", Usage: "never prompt or animate; print plain lines"},
		},
		Action:   wizard,
		Commands: []*cli.Command{setupCommand(), addUserCommand(), runCommand(), statusCommand(), downCommand()},
	}
}

// interactive reports whether the CLI may prompt.
func interactive(cmd *cli.Command) bool {
	return !cmd.Bool("no-input") && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// session is one run of a command: the app it drives and, in a terminal, the
// terminal UI that shows its steps and asks its questions.
type session struct {
	app   *sandbox.App
	theme theme
	tui   *presenter
}

// ask shows a form and waits for the answer.
func (s *session) ask(form *huh.Form) error {
	if s.tui == nil {
		return errNeedsTerminal
	}
	return s.tui.ask(form)
}

// answer records what the person chose, once the form is gone.
func (s *session) answer(label, value string) {
	s.show(func(int) string { return s.theme.answer(label, value) })
}

// phase moves the tracker to a phase.
func (s *session) phase(phase int) {
	if s.tui != nil {
		s.tui.send(phaseMsg(phase))
	}
}

// show adds a block to the output, rendered at the terminal's width.
func (s *session) show(render func(width int) string) {
	if s.tui != nil {
		s.tui.show(render)
		return
	}
	// Without a terminal, the output is a log; a failed write has nowhere
	// to go.
	_, _ = lipgloss.Println(render(maxWidth))
}

// withSession signs in and runs fn. In a terminal, fn runs under the terminal
// UI, with a tracker over phases when they are set. Otherwise it prints plain
// lines and never prompts.
func withSession(
	ctx context.Context,
	cmd *cli.Command,
	phases []string,
	action func(context.Context, *session) error,
) error {
	look := newTheme()
	if !interactive(cmd) {
		plain := &session{theme: look}
		if err := open(ctx, cmd, plain, &sandbox.UI{Out: os.Stdout}); err != nil {
			return err
		}
		return action(ctx, plain)
	}
	return runTUI(ctx, look, phases, func(ctx context.Context, view *presenter) error {
		live := &session{theme: look, tui: view}
		ui := &sandbox.UI{Out: os.Stdout, Interactive: true, Emit: func(event sandbox.Event) { view.send(event) }}
		if err := open(ctx, cmd, live, ui); err != nil {
			return err
		}
		return action(ctx, live)
	})
}

// open loads the state, signs in, and sets sess.app.
func open(ctx context.Context, cmd *cli.Command, sess *session, report *sandbox.UI) error {
	statePath := cmd.String("state")
	state, err := sandbox.LoadState(statePath)
	if err != nil {
		return fmt.Errorf("%w: %w", errLoadState, err)
	}
	apiURL := cmd.String("api-url")
	state.APIURL = apiURL
	report.Header(apiURL)
	clientID, secret, err := credentials(cmd, sess, report)
	if err != nil {
		return err
	}
	sess.app = &sandbox.App{
		KC:        sandbox.NewKeycard(apiURL, clientID, secret),
		State:     state,
		StatePath: statePath,
		UI:        report,
	}
	sess.phase(phaseConnect)
	return signIn(ctx, sess.app)
}

// credentials responds with the service account's, asking for what is
// missing when the CLI may.
func credentials(cmd *cli.Command, sess *session, ui *sandbox.UI) (string, string, error) {
	clientID, secret := cmd.String("client-id"), cmd.String("client-secret")
	if clientID != "" && secret != "" {
		return clientID, secret, nil
	}
	if !ui.Interactive {
		return "", "", errNoCredentials
	}
	err := sess.ask(credentialsForm(&clientID, &secret))
	return clientID, secret, err
}

// signIn checks the tools setup needs, then signs in.
func signIn(ctx context.Context, app *sandbox.App) error {
	if err := app.UI.Step(ctx, "Prerequisites", checkTools); err != nil {
		return errReported
	}
	if err := app.UI.Step(ctx, "Signed in as the service account", func(ctx context.Context) (string, error) {
		org, err := app.KC.SignIn(ctx)
		app.State.OrgID = org
		return "org " + org, err //nolint:wrapcheck // The step shows the sign-in's own error.
	}); err != nil {
		return errReported
	}
	return nil
}

func checkTools(context.Context) (string, error) {
	if err := sandbox.RequireTools(); err != nil {
		return "", err //nolint:wrapcheck // The error names the missing tool.
	}
	if _, err := sandbox.LocaldevCA(); err != nil {
		return "", fmt.Errorf("%w: %w", err, errNoLocaldev)
	}
	return "docker, k3d, kubectl, localdev CA", nil
}

func defaultEgressProxyDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "../egress-proxy"
	}
	return filepath.Join(filepath.Dir(wd), "egress-proxy")
}

// reported stands in for an error the CLI already showed.
func reported(err error) error {
	if err != nil {
		return errReported
	}
	return nil
}
