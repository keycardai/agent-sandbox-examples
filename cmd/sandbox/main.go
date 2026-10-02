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
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := newCommand().Run(ctx, os.Args); err != nil {
		switch {
		case errors.Is(err, errCancelled):
			os.Exit(130)
		case !errors.Is(err, errReported):
			fmt.Fprintf(os.Stderr, "sandbox: %v\n", err)
		}
		os.Exit(1)
	}
}

// errReported is a failure the CLI already showed.
var errReported = errors.New("reported")

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
	app *sandbox.App
	tui *presenter
}

// ask shows a form and waits for the answer.
func (s *session) ask(form *huh.Form) error {
	if s.tui == nil {
		return errors.New("this needs an answer; run it in a terminal or pass the flags")
	}
	return s.tui.ask(form)
}

// answer records what the person chose, once the form is gone.
func (s *session) answer(label, value string) {
	s.show(func(int) string {
		return "  " + styleAccent.Render("◆") + " " + styleDim.Render(label) + "  " + value
	})
}

// phase moves the tracker to phase i.
func (s *session) phase(i int) {
	if s.tui != nil {
		s.tui.send(phaseMsg(i))
	}
}

// show adds a block to the output, rendered at the terminal's width.
func (s *session) show(render func(width int) string) {
	if s.tui != nil {
		s.tui.show(render)
		return
	}
	lipgloss.Println(render(100))
}

// withSession signs in and runs fn. In a terminal, fn runs under the terminal
// UI, with a tracker over phases when they are set. Otherwise it prints plain
// lines and never prompts.
func withSession(
	ctx context.Context,
	cmd *cli.Command,
	phases []string,
	fn func(context.Context, *session) error,
) error {
	if !interactive(cmd) {
		s := &session{}
		if err := open(ctx, cmd, s, &sandbox.UI{Out: os.Stdout}); err != nil {
			return err
		}
		return fn(ctx, s)
	}
	return runTUI(ctx, phases, func(ctx context.Context, p *presenter) error {
		s := &session{tui: p}
		ui := &sandbox.UI{Out: os.Stdout, Interactive: true, Emit: func(e sandbox.Event) { p.send(e) }}
		if err := open(ctx, cmd, s, ui); err != nil {
			return err
		}
		return fn(ctx, s)
	})
}

// open signs in and loads the state into s.app. It asks for missing
// credentials when it may.
func open(ctx context.Context, cmd *cli.Command, s *session, ui *sandbox.UI) error {
	statePath := cmd.String("state")
	state, err := sandbox.LoadState(statePath)
	if err != nil {
		return err
	}
	apiURL := cmd.String("api-url")
	state.APIURL = apiURL
	ui.Header(apiURL)
	clientID, secret := cmd.String("client-id"), cmd.String("client-secret")
	if clientID == "" || secret == "" {
		if !ui.Interactive {
			return errors.New("set KEYCARD_CLIENT_ID and KEYCARD_CLIENT_SECRET to an organization service account")
		}
		if err := s.ask(credentialsForm(&clientID, &secret)); err != nil {
			return err
		}
	}
	kc, err := sandbox.NewKeycard(apiURL, clientID, secret)
	if err != nil {
		return err
	}
	s.app = &sandbox.App{KC: kc, State: state, StatePath: statePath, UI: ui}
	s.phase(0)
	if err := ui.Step(ctx, "Prerequisites", func(context.Context) (string, error) {
		if err := sandbox.RequireTools(); err != nil {
			return "", err
		}
		if _, err := sandbox.LocaldevCA(); err != nil {
			return "", fmt.Errorf("%w; is localdev set up?", err)
		}
		return "docker, k3d, kubectl, localdev CA", nil
	}); err != nil {
		return errReported
	}
	if err := ui.Step(ctx, "Signed in as the service account", func(ctx context.Context) (string, error) {
		org, err := kc.SignIn(ctx)
		state.OrgID = org
		return "org " + org, err
	}); err != nil {
		return errReported
	}
	return nil
}

func setupFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "zone", Usage: "the zone to set up"},
		&cli.StringFlag{Name: "create-zone", Usage: "create a zone with this name instead"},
		&cli.StringSliceFlag{
			Name:  "resource",
			Usage: "an API server package and the variable holding its API key, as SLUG=ENV_VAR (repeatable)",
		},
		&cli.StringFlag{Name: "namespace", Value: sandbox.DefaultNamespace, Usage: "the namespace the sandboxes run in"},
		&cli.StringFlag{Name: "cluster", Value: sandbox.DefaultCluster, Usage: "the k3d cluster the sandboxes run in"},
		&cli.StringFlag{
			Name:  "issuer",
			Value: sandbox.DefaultIssuer,
			Usage: "the cluster's ServiceAccount issuer, served through localdev's gateway",
		},
		&cli.StringFlag{
			Name:  "egress-proxy-dir",
			Value: defaultEgressProxyDir(),
			Usage: "an egress-proxy checkout, to build the images from",
		},
	}
}

func defaultEgressProxyDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return "../egress-proxy"
	}
	return filepath.Join(filepath.Dir(wd), "egress-proxy")
}

func setupCommand() *cli.Command {
	return &cli.Command{
		Name:  "setup",
		Usage: "install the API servers and the shared application, and start the sandbox cluster",
		Flags: setupFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return withSession(ctx, cmd, nil, func(ctx context.Context, s *session) error {
				in, err := setupInput(ctx, s, cmd)
				if err != nil {
					return err
				}
				return reported(s.app.Setup(ctx, in))
			})
		},
	}
}

// setupInput reads setup's flags, asking for what is missing when it may.
func setupInput(ctx context.Context, s *session, cmd *cli.Command) (sandbox.SetupInput, error) {
	app := s.app
	in := sandbox.SetupInput{
		ZoneID: cmd.String("zone"), CreateZone: cmd.String("create-zone"),
		Namespace: cmd.String("namespace"), Cluster: cmd.String("cluster"),
		Issuer: cmd.String("issuer"), EgressProxyDir: cmd.String("egress-proxy-dir"),
	}
	if in.ZoneID != "" && in.CreateZone != "" {
		return in, errors.New("pass --zone or --create-zone, not both")
	}
	if in.ZoneID == "" && in.CreateZone == "" {
		in.ZoneID = app.State.ZoneID
	}
	if in.ZoneID == "" && in.CreateZone == "" {
		if !app.UI.Interactive {
			return in, errors.New("pass --zone or --create-zone")
		}
		var err error
		if in.ZoneID, in.CreateZone, err = askZone(ctx, s); err != nil {
			return in, err
		}
	}
	if specs := cmd.StringSlice("resource"); len(specs) > 0 {
		if err := resourceKeys(ctx, app, &in, specs); err != nil {
			return in, err
		}
	}
	if len(in.APIKeys) == 0 && len(app.State.Resources) == 0 {
		if !app.UI.Interactive {
			return in, errors.New("pass --resource SLUG=ENV_VAR for at least one API server")
		}
		if err := askResources(ctx, s, &in); err != nil {
			return in, err
		}
	}
	return in, askKeys(s, &in)
}

// resourceKeys reads --resource SLUG=ENV_VAR flags into in.APIKeys.
func resourceKeys(ctx context.Context, app *sandbox.App, in *sandbox.SetupInput, specs []string) error {
	if in.ZoneID == "" {
		return errors.New("--create-zone and --resource cannot be checked together; create the zone first")
	}
	packages, err := app.KC.APIKeyPackages(ctx, in.ZoneID)
	if err != nil {
		return err
	}
	bySlug := map[string]sandbox.Package{}
	for _, p := range packages {
		bySlug[p.Slug] = p
	}
	for _, spec := range specs {
		slug, envVar, _ := strings.Cut(spec, "=")
		pkg, ok := bySlug[slug]
		if !ok {
			return fmt.Errorf("--resource %s: no API server with slug %q takes an API key", spec, slug)
		}
		key := ""
		if envVar != "" {
			if key = os.Getenv(envVar); key == "" {
				return fmt.Errorf("--resource %s: %s is empty", spec, envVar)
			}
		} else if !app.UI.Interactive {
			return fmt.Errorf("--resource %s needs =ENV_VAR naming its API key", slug)
		}
		in.APIKeys = append(
			in.APIKeys,
			sandbox.APIKey{Slug: slug, Name: pkg.Name, Label: pkg.FieldLabel("api_key"), Key: key},
		)
	}
	return nil
}

func addUserCommand() *cli.Command {
	return &cli.Command{
		Name:  "add-user",
		Usage: "start a sandbox for a zone user, enrolled so it can act for them unless --no-access",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "email", Usage: "the user to add; they must already be in the zone"},
			&cli.BoolFlag{Name: "no-access", Usage: "give the sandbox a credential but no policies, so the zone refuses it"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return withSession(ctx, cmd, nil, func(ctx context.Context, s *session) error {
				app := s.app
				access := !cmd.Bool("no-access")
				email := cmd.String("email")
				if email == "" {
					if !app.UI.Interactive {
						return errors.New("pass --email")
					}
					if err := app.RequireSetup(); err != nil {
						return err
					}
					users, err := candidates(ctx, app, nil)
					if err != nil {
						return err
					}
					if email, err = askEmail(s, "User to add "+accessLabel(access), users); err != nil {
						return err
					}
				}
				var err error
				if access {
					_, err = app.AddUser(ctx, email)
				} else {
					_, err = app.AddUserWithoutAccess(ctx, email)
				}
				return reported(err)
			})
		},
	}
}

func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "run the agent in each user's sandbox, calling each API it has a call for, and show what each call got",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "email", Usage: "run only this user's sandbox; repeat for more (default: every user)"},
			&cli.StringFlag{
				Name:    "read",
				Sources: cli.EnvVars("SANDBOX_READ"),
				Usage:   "the GitHub file the agent reads, as OWNER/REPO/PATH, when setup installed GitHub",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return withSession(ctx, cmd, nil, func(ctx context.Context, s *session) error {
				app := s.app
				in := sandbox.RunInput{Read: cmd.String("read"), Emails: cmd.StringSlice("email")}
				if in.Read == "" && app.State.HasResource("github") {
					if !app.UI.Interactive {
						return errors.New("pass --read")
					}
					if err := askRead(s, &in); err != nil {
						return err
					}
				}
				results, err := app.Run(ctx, in)
				if len(results) > 0 {
					s.show(func(width int) string { return renderResults(results, width) })
				}
				return reported(err)
			})
		},
	}
}

func statusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show what setup recorded and check each piece",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return withSession(ctx, cmd, nil, func(ctx context.Context, s *session) error {
				s.app.Status(ctx)
				return nil
			})
		},
	}
}

func downCommand() *cli.Command {
	return &cli.Command{
		Name:  "down",
		Usage: "delete the sandbox cluster and the issuer route, and reactivate the zone's previous policy set",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return withSession(ctx, cmd, nil, func(ctx context.Context, s *session) error {
				return reported(s.app.Down(ctx))
			})
		},
	}
}

func reported(err error) error {
	if err != nil {
		return errReported
	}
	return nil
}
