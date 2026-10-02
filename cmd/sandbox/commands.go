package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

// apiKeyField is the install form field that takes an API server's key.
const apiKeyField = "api_key"

var (
	errZoneFlags       = errors.New("pass --zone or --create-zone, not both")
	errNoZone          = errors.New("pass --zone or --create-zone")
	errNoResource      = errors.New("pass --resource SLUG=ENV_VAR for at least one API server")
	errUnknownResource = errors.New("no API server with this slug takes an API key")
	errNoKeyVariable   = errors.New("name the variable that holds its API key, as SLUG=ENV_VAR")
	errEmptyKey        = errors.New("the variable that holds its API key is empty")
	errNoEmail         = errors.New("pass --email")
	errNoRead          = errors.New("pass --read")
	errCatalog         = errors.New("could not read the catalog")
)

func setupCommand() *cli.Command {
	return &cli.Command{
		Name:  "setup",
		Usage: "install the API servers and the shared application, and start the sandbox cluster",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "zone", Usage: "the zone to set up"},
			&cli.StringFlag{Name: "create-zone", Usage: "create a zone with this name instead"},
			&cli.StringSliceFlag{
				Name:  "resource",
				Usage: "an API server package and the variable holding its API key, as SLUG=ENV_VAR (repeatable)",
			},
			&cli.StringFlag{
				Name:  "namespace",
				Value: sandbox.DefaultNamespace,
				Usage: "the namespace the sandboxes run in",
			},
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
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return withSession(ctx, cmd, nil, func(ctx context.Context, s *session) error {
				input, err := setupInput(ctx, s, cmd)
				if err != nil {
					return err
				}
				return reported(s.app.Setup(ctx, input))
			})
		},
	}
}

// setupInput reads setup's flags, asking for what is missing when it may.
func setupInput(ctx context.Context, sess *session, cmd *cli.Command) (sandbox.SetupInput, error) {
	input := sandbox.SetupInput{
		Namespace:      cmd.String("namespace"),
		Cluster:        cmd.String("cluster"),
		Issuer:         cmd.String("issuer"),
		EgressProxyDir: cmd.String("egress-proxy-dir"),
	}
	zoneID, err := setupZone(ctx, sess, cmd.String("zone"), cmd.String("create-zone"))
	if err != nil {
		return input, err
	}
	input.ZoneID = zoneID
	if err := setupResources(ctx, sess, &input, cmd.StringSlice("resource")); err != nil {
		return input, err
	}
	return input, askKeys(sess, &input)
}

// setupZone responds with the zone setup uses: the one the flags name, then
// the one setup used last, then the one the person picks. --create-zone
// creates the zone first.
func setupZone(ctx context.Context, sess *session, zoneID, createName string) (string, error) {
	switch {
	case zoneID != "" && createName != "":
		return "", errZoneFlags
	case createName != "":
		return createZone(ctx, sess.app, createName)
	case zoneID != "":
		return zoneID, nil
	case sess.app.State.ZoneID != "":
		return sess.app.State.ZoneID, nil
	case !sess.app.UI.Interactive:
		return "", errNoZone
	default:
		return askZone(ctx, sess)
	}
}

// setupResources adds the API servers the --resource flags name to input.
// With none, setup keeps the ones it installed before, or asks.
func setupResources(ctx context.Context, sess *session, input *sandbox.SetupInput, specs []string) error {
	app := sess.app
	if len(specs) > 0 {
		packages, err := app.KC.APIKeyPackages(ctx, input.ZoneID)
		if err != nil {
			return fmt.Errorf("%w: %w", errCatalog, err)
		}
		keys, err := parseResources(specs, packages, app.UI.Interactive, os.Getenv)
		input.APIKeys = append(input.APIKeys, keys...)
		return err
	}
	if app.State.ZoneID == input.ZoneID && len(app.State.Resources) > 0 {
		return nil
	}
	if !app.UI.Interactive {
		return errNoResource
	}
	return askResources(ctx, sess, input)
}

// parseResources reads --resource SLUG=ENV_VAR flags against the packages
// that take an API key. Without =ENV_VAR the key is left for the person to
// enter, when they may.
func parseResources(
	specs []string,
	packages []sandbox.Package,
	interactive bool,
	getenv func(string) string,
) ([]sandbox.APIKey, error) {
	keys := make([]sandbox.APIKey, 0, len(specs))
	for _, spec := range specs {
		slug, variable, _ := strings.Cut(spec, "=")
		index := slices.IndexFunc(packages, func(pkg sandbox.Package) bool { return pkg.Slug == slug })
		if index < 0 {
			return nil, fmt.Errorf("--resource %s: %w", spec, errUnknownResource)
		}
		key := ""
		switch {
		case variable != "":
			if key = getenv(variable); key == "" {
				return nil, fmt.Errorf("--resource %s: %w", spec, errEmptyKey)
			}
		case !interactive:
			return nil, fmt.Errorf("--resource %s: %w", spec, errNoKeyVariable)
		}
		pkg := packages[index]
		keys = append(keys, sandbox.APIKey{Slug: slug, Name: pkg.Name, Label: keyLabel(pkg), Key: key})
	}
	return keys, nil
}

// keyLabel is the package's name for its API key, such as Personal API Key.
func keyLabel(pkg sandbox.Package) string {
	field, _ := pkg.Field(apiKeyField)
	return field.Label
}

func addUserCommand() *cli.Command {
	return &cli.Command{
		Name:  "add-user",
		Usage: "start a sandbox for a zone user, enrolled so it can act for them unless --no-access",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "email", Usage: "the user to add; they must already be in the zone"},
			&cli.BoolFlag{
				Name:  "no-access",
				Usage: "give the sandbox a credential but no policies, so the zone refuses it",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return withSession(ctx, cmd, nil, func(ctx context.Context, s *session) error {
				return addUser(ctx, s, cmd.String("email"), !cmd.Bool("no-access"))
			})
		},
	}
}

// addUser starts a sandbox for the user with this email, or for the one the
// person picks.
func addUser(ctx context.Context, sess *session, email string, access bool) error {
	app := sess.app
	if email == "" {
		if !app.UI.Interactive {
			return errNoEmail
		}
		if err := app.RequireSetup(); err != nil {
			return err //nolint:wrapcheck // The error says what to run.
		}
		users, err := candidates(ctx, app, nil)
		if err != nil {
			return err
		}
		if email, err = askEmail(sess, "User to add "+accessLabel(access), users); err != nil {
			return err
		}
	}
	add := app.AddUser
	if !access {
		add = app.AddUserWithoutAccess
	}
	_, err := add(ctx, email)
	return reported(err)
}

func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "run the agent in each user's sandbox, calling each API it has a call for, and show what each call got",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:  "email",
				Usage: "run only this user's sandbox; repeat for more (default: every user)",
			},
			&cli.StringFlag{
				Name:    "read",
				Sources: cli.EnvVars("SANDBOX_READ"),
				Usage:   "the GitHub file the agent reads, as OWNER/REPO/PATH, when setup installed GitHub",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return withSession(ctx, cmd, nil, func(ctx context.Context, sess *session) error {
				input := sandbox.RunInput{Read: cmd.String("read"), Emails: cmd.StringSlice("email")}
				if input.Read == "" && sess.app.State.HasResource(sandbox.SlugGitHub) {
					if !sess.app.UI.Interactive {
						return errNoRead
					}
					if err := askRead(sess, &input); err != nil {
						return err
					}
				}
				return runAgent(ctx, sess, input)
			})
		},
	}
}

// runAgent runs the agent and shows what each call got.
func runAgent(ctx context.Context, s *session, input sandbox.RunInput) error {
	results, err := s.app.Run(ctx, input)
	if len(results) > 0 {
		s.show(func(width int) string { return s.theme.results(results, width) })
	}
	return reported(err)
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
