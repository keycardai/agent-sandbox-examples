package main

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"slices"
	"sort"
	"strings"

	"charm.land/huh/v2"
	"github.com/urfave/cli/v3"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

// defaultRead is the file the agent reads when nothing else names one.
const defaultRead = "jerriclynsjohn/test-repo/demo.txt"

const (
	createZoneOption = "\x00create"
	otherUserOption  = "\x00other"
)

// wizardPhases are the wizard's steps, in the order the tracker shows them.
var wizardPhases = []string{"Connect", "Zone", "APIs", "Setup", "Users", "Agent"}

// wizard asks for everything and runs setup, add-user and run in turn.
func wizard(ctx context.Context, cmd *cli.Command) error {
	if !interactive(cmd) {
		return errors.New(
			"the wizard needs a terminal; use `sandbox setup`, `sandbox add-user` and `sandbox run` instead (see --help)",
		)
	}
	return withSession(ctx, cmd, wizardPhases, func(ctx context.Context, s *session) error {
		app := s.app
		ui := app.UI

		s.phase(1)
		zoneID, createName, err := askZone(ctx, s)
		if err != nil {
			return err
		}
		if createName != "" {
			if err := ui.Step(ctx, "Zone "+createName+" created", func(ctx context.Context) (string, error) {
				z, err := app.KC.CreateZone(ctx, createName)
				zoneID = z.ID
				return z.ID, err
			}); err != nil {
				return errReported
			}
		}

		s.phase(2)
		ui.Warn(
			"Enrolling a user makes the \"Kubernetes Sandbox\" policy set this zone's only active one. `sandbox down` puts the previous one back.",
		)
		in := sandbox.SetupInput{
			ZoneID: zoneID, Namespace: sandbox.DefaultNamespace, Cluster: sandbox.DefaultCluster,
			Issuer: sandbox.DefaultIssuer, EgressProxyDir: defaultEgressProxyDir(),
		}
		if app.State.ZoneID != zoneID {
			app.State.Resources = nil
		}
		if err := askResources(ctx, s, &in); err != nil {
			return err
		}
		if err := askKeys(s, &in); err != nil {
			return err
		}

		s.phase(3)
		if err := app.Setup(ctx, in); err != nil {
			return errReported
		}

		s.phase(4)
		var added []string
		if err := addUsers(ctx, s, true, &added); err != nil {
			return err
		}
		if err := addUsers(ctx, s, false, &added); err != nil {
			return err
		}
		if len(added) == 0 {
			ui.Warn("No user was added, so there is no sandbox to run the agent in.")
			return errReported
		}

		s.phase(5)
		run := sandbox.RunInput{Read: os.Getenv("SANDBOX_READ"), Emails: added}
		if app.State.HasResource("github") {
			if err := askRead(s, &run); err != nil {
				return err
			}
		}
		results, runErr := app.Run(ctx, run)
		if len(results) > 0 {
			s.show(func(width int) string { return renderResults(results, width) })
		}
		s.show(func(width int) string { return renderCommands(app.State, run, width) })
		if runErr != nil {
			return errReported
		}
		return nil
	})
}

// addUsers adds users one at a time, with or without access, until the person
// says to stop, and appends each one it added to added. Adding a user without
// access is optional; the first user with access is not.
func addUsers(ctx context.Context, s *session, access bool, added *[]string) error {
	app := s.app
	add := app.AddUser
	if !access {
		add = app.AddUserWithoutAccess
	}
	label := accessLabel(access)
	count, failed := 0, false
	for {
		users, err := candidates(ctx, app, *added)
		if err != nil {
			return err
		}
		if len(users) == 0 && (count > 0 || !access) {
			if !access && count == 0 {
				app.UI.Info(
					"Everyone in the zone already has a sandbox. To add a user without access, have someone else sign in to " + app.State.ZoneURL + " once, then run `sandbox add-user --no-access`.",
				)
			}
			return nil
		}
		if count > 0 || failed || !access {
			more, err := askMore(s, access, count, failed)
			if err != nil || !more {
				return err
			}
		}
		email, err := askEmail(s, "Who should get a sandbox "+label+"?", users)
		if err != nil {
			return err
		}
		if _, err := add(ctx, email); err != nil {
			failed = true
			continue
		}
		count, failed = count+1, false
		*added = append(*added, email)
	}
}

// askMore asks whether to add another user.
func askMore(s *session, access bool, count int, failed bool) (bool, error) {
	label := accessLabel(access)
	question, description := "Add another user "+label+"?", ""
	switch {
	case failed:
		question = "Try another user " + label + "?"
	case !access && count == 0:
		question = "Add a user without access?"
		description = "They are in the zone, but nothing lets the sandbox act for them, so the zone refuses every request it makes. This shows the policies doing their job."
	}
	more := count == 0
	err := s.ask(huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title(question).Description(description).Affirmative("Yes").Negative("No").Value(&more),
	)))
	return more, err
}

func accessLabel(access bool) string {
	if access {
		return "with access"
	}
	return "without access"
}

func credentialsForm(clientID, secret *string) *huh.Form {
	fields := []huh.Field{}
	if *clientID == "" {
		fields = append(fields, huh.NewInput().Title("Service account client ID").
			Description("An organization service account. Set KEYCARD_CLIENT_ID to skip this.").
			Value(clientID).Validate(required))
	}
	fields = append(fields, huh.NewInput().Title("Service account client secret").
		Description("Set KEYCARD_CLIENT_SECRET to skip this.").
		EchoMode(huh.EchoModePassword).Value(secret).Validate(required))
	return huh.NewForm(huh.NewGroup(fields...))
}

func askZone(ctx context.Context, s *session) (string, string, error) {
	app := s.app
	var zones []sandbox.Zone
	if err := app.UI.Step(ctx, "Zones listed", func(ctx context.Context) (string, error) {
		var err error
		zones, err = app.KC.ListZones(ctx)
		return fmt.Sprintf("%d in the organization", len(zones)), err
	}); err != nil {
		return "", "", errReported
	}
	labels := map[string]string{}
	options := make([]huh.Option[string], 0, len(zones)+1)
	for _, z := range zones {
		labels[z.ID] = fmt.Sprintf("%s (%s)", z.Name, z.ID)
		options = append(options, huh.NewOption(labels[z.ID], z.ID))
	}
	options = append(options, huh.NewOption("+ Create a new zone", createZoneOption))
	choice, name := app.State.ZoneID, "sandbox-demo"
	if err := s.ask(huh.NewForm(
		huh.NewGroup(huh.NewSelect[string]().
			Title("Which zone should the sandboxes use?").
			Description("The zone holds the API servers, the sandbox application and its users.").
			Options(options...).Value(&choice)),
		huh.NewGroup(huh.NewInput().Title("Name the new zone").Value(&name).Validate(required)).
			WithHideFunc(func() bool { return choice != createZoneOption }),
	)); err != nil {
		return "", "", err
	}
	if choice == createZoneOption {
		s.answer("Zone", strings.TrimSpace(name)+" (new)")
		return "", strings.TrimSpace(name), nil
	}
	s.answer("Zone", labels[choice])
	return choice, "", nil
}

func askResources(ctx context.Context, s *session, in *sandbox.SetupInput) error {
	app := s.app
	if in.ZoneID == "" {
		return errors.New("pass --resource SLUG=ENV_VAR together with --create-zone")
	}
	var packages []sandbox.Package
	if err := app.UI.Step(ctx, "Catalog read", func(ctx context.Context) (string, error) {
		var err error
		packages, err = app.KC.APIKeyPackages(ctx, in.ZoneID)
		if err == nil && len(packages) == 0 {
			err = errors.New("the catalog has no API server that takes an API key")
		}
		return fmt.Sprintf("%d API servers take an API key", len(packages)), err
	}); err != nil {
		return errReported
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	bySlug := map[string]sandbox.Package{}
	options := make([]huh.Option[string], 0, len(packages))
	var picked []string
	for _, p := range packages {
		bySlug[p.Slug] = p
		label := p.Name
		if l := p.FieldLabel("api_key"); l != "" {
			label += " (" + l + ")"
		}
		options = append(options, huh.NewOption(label, p.Slug))
		if app.State.HasResource(p.Slug) || len(packages) == 1 {
			picked = append(picked, p.Slug)
		}
	}
	if err := s.ask(huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Which APIs may the sandboxes reach?").
			Description("Space to pick, enter to continue. The agent calls GitHub and Linear in the demo.").
			Options(options...).Value(&picked).
			Validate(func(v []string) error {
				if len(v) == 0 {
					return errors.New("pick at least one")
				}
				return nil
			}),
	))); err != nil {
		return err
	}
	names := make([]string, 0, len(picked))
	for _, slug := range picked {
		p := bySlug[slug]
		names = append(names, p.Name)
		in.APIKeys = append(in.APIKeys, sandbox.APIKey{Slug: slug, Name: p.Name, Label: p.FieldLabel("api_key")})
	}
	s.answer("APIs", strings.Join(names, ", "))
	return nil
}

// askKeys asks, in one form, for each key setup does not have yet.
func askKeys(s *session, in *sandbox.SetupInput) error {
	var fields []huh.Field
	var names []string
	for i := range in.APIKeys {
		k := &in.APIKeys[i]
		if k.Key != "" {
			continue
		}
		label := k.Label
		if label == "" {
			label = "API key"
		}
		names = append(names, k.Name)
		fields = append(fields, huh.NewInput().
			Title(k.Name+" · "+label).
			Description("Goes to the zone's vault. The sandbox never sees it.").
			EchoMode(huh.EchoModePassword).Value(&k.Key).Validate(required))
	}
	if len(fields) == 0 {
		return nil
	}
	if s.tui == nil {
		return fmt.Errorf("pass the keys for %s as --resource SLUG=ENV_VAR", strings.Join(names, ", "))
	}
	if err := s.ask(huh.NewForm(huh.NewGroup(fields...))); err != nil {
		return err
	}
	for i := range in.APIKeys {
		in.APIKeys[i].Key = strings.TrimSpace(in.APIKeys[i].Key)
	}
	s.answer("Keys", "entered for "+strings.Join(names, ", "))
	return nil
}

// candidates lists the zone's users that are not in skip.
func candidates(ctx context.Context, app *sandbox.App, skip []string) ([]sandbox.ZoneUser, error) {
	users, err := app.KC.ListUsers(ctx, app.State.ZoneID)
	if err != nil {
		return nil, err
	}
	out := users[:0]
	for _, u := range users {
		if !slices.ContainsFunc(skip, func(email string) bool { return strings.EqualFold(email, u.Email) }) {
			out = append(out, u)
		}
	}
	return out, nil
}

// askEmail offers the given zone users, plus any other email.
func askEmail(s *session, title string, users []sandbox.ZoneUser) (string, error) {
	options := make([]huh.Option[string], 0, len(users)+1)
	for _, u := range users {
		options = append(options, huh.NewOption(u.Email, u.Email))
	}
	options = append(options, huh.NewOption("+ Someone else", otherUserOption))
	choice, typed := "", ""
	typing := func() bool { return len(users) == 0 || choice == otherUserOption }
	if err := s.ask(huh.NewForm(
		huh.NewGroup(huh.NewSelect[string]().Title(title).
			Description("People in the zone. Someone new has to sign in to the zone once first.").
			Options(options...).Value(&choice)).
			WithHideFunc(func() bool { return len(users) == 0 }),
		huh.NewGroup(huh.NewInput().Title(title).
			Description("Their email. They must already be in the zone.").
			Value(&typed).Validate(func(v string) error {
			_, err := mail.ParseAddress(v)
			return err
		})).WithHideFunc(func() bool { return !typing() }),
	)); err != nil {
		return "", err
	}
	if typing() {
		return strings.TrimSpace(typed), nil
	}
	return choice, nil
}

func askRead(s *session, in *sandbox.RunInput) error {
	if in.Read == "" {
		in.Read = defaultRead
	}
	if err := s.ask(huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Which GitHub file should the agent read?").
			Description("OWNER/REPO/PATH. A private repo shows the agent using the user's access.").
			Value(&in.Read).Validate(required),
	))); err != nil {
		return err
	}
	s.answer("File", in.Read)
	return nil
}

func required(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("required")
	}
	return nil
}
