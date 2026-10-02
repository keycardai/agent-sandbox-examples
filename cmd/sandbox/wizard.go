package main

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/urfave/cli/v3"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

// defaultRead is the file the agent reads when nothing else names one.
const defaultRead = "jerriclynsjohn/test-repo/demo.txt"

// Options the forms add to the ones the zone lists.
const (
	createZoneOption = "\x00create"
	otherUserOption  = "\x00other"
)

// The wizard's phases, in the order the tracker shows them.
const (
	phaseConnect = iota
	phaseZone
	phaseAPIs
	phaseSetup
	phaseUsers
	phaseAgent
)

var (
	errWizardNeedsTerminal = errors.New(
		"the wizard needs a terminal; use `sandbox setup`, `sandbox add-user` and `sandbox run` instead (see --help)",
	)
	errNoAPIKeyPackages = errors.New("the catalog has no API server that takes an API key")
	errPickOne          = errors.New("pick at least one")
	errRequired         = errors.New("required")
	errNotAnEmail       = errors.New("enter an email address")
	errListUsers        = errors.New("could not list the zone's users")
)

func wizardPhases() []string { return []string{"Connect", "Zone", "APIs", "Setup", "Users", "Agent"} }

// wizard asks for everything and runs setup, add-user and run in turn.
func wizard(ctx context.Context, cmd *cli.Command) error {
	if !interactive(cmd) {
		return errWizardNeedsTerminal
	}
	return withSession(ctx, cmd, wizardPhases(), runWizard)
}

func runWizard(ctx context.Context, sess *session) error {
	app := sess.app
	sess.phase(phaseZone)
	zoneID, err := askZone(ctx, sess)
	if err != nil {
		return err
	}

	sess.phase(phaseAPIs)
	app.UI.Warn("Enrolling a user makes the \"Kubernetes Sandbox\" policy set this zone's only active one. " +
		"`sandbox down` puts the previous one back.")
	input := sandbox.SetupInput{
		ZoneID:         zoneID,
		Namespace:      sandbox.DefaultNamespace,
		Cluster:        sandbox.DefaultCluster,
		Issuer:         sandbox.DefaultIssuer,
		EgressProxyDir: defaultEgressProxyDir(),
	}
	if err = askResources(ctx, sess, &input); err != nil {
		return err
	}
	if err = askKeys(sess, &input); err != nil {
		return err
	}

	sess.phase(phaseSetup)
	if err = app.Setup(ctx, input); err != nil {
		return errReported
	}

	sess.phase(phaseUsers)
	added, err := addUsers(ctx, sess)
	if err != nil {
		return err
	}
	if len(added) == 0 {
		app.UI.Warn("No user was added, so there is no sandbox to run the agent in.")
		return errReported
	}

	sess.phase(phaseAgent)
	run := sandbox.RunInput{Read: os.Getenv("SANDBOX_READ"), Emails: added}
	if app.State.HasResource(sandbox.SlugGitHub) {
		if err := askRead(sess, &run); err != nil {
			return err
		}
	}
	runErr := runAgent(ctx, sess, run)
	sess.show(func(width int) string { return sess.theme.commands(app.State, run, width) })
	return runErr
}

// addUsers adds users with access, then users without, and responds with
// the emails of those it added.
func addUsers(ctx context.Context, s *session) ([]string, error) {
	var added []string
	for _, access := range []bool{true, false} {
		if err := addUsersWith(ctx, s, access, &added); err != nil {
			return added, err
		}
	}
	return added, nil
}

// addUsersWith adds users one at a time, with or without access, until the
// person says to stop, and appends each one it added to added.
func addUsersWith(ctx context.Context, sess *session, access bool, added *[]string) error {
	add := sess.app.AddUser
	if !access {
		add = sess.app.AddUserWithoutAccess
	}
	round := userRound{access: access}
	for {
		users, err := candidates(ctx, sess.app, *added)
		if err != nil {
			return err
		}
		more, err := round.another(sess, len(users))
		if err != nil || !more {
			return err
		}
		email, err := askEmail(sess, "Who should get a sandbox "+accessLabel(access)+"?", users)
		if err != nil {
			return err
		}
		if _, err := add(ctx, email); err != nil {
			round.failed = true
			continue
		}
		round.added, round.failed = round.added+1, false
		*added = append(*added, email)
	}
}

// userRound is where adding users with or without access has got to.
type userRound struct {
	access bool
	// added counts the users this round added, and failed says the last one
	// did not work.
	added  int
	failed bool
}

// another says whether to add another user, given how many zone users are
// left to offer. The first user with access is not optional; anyone after
// that the person asks for.
func (r userRound) another(sess *session, candidates int) (bool, error) {
	if candidates == 0 && (r.added > 0 || !r.access) {
		if !r.access && r.added == 0 {
			sess.app.UI.Info("Everyone in the zone already has a sandbox. To add a user without access, " +
				"have someone else sign in to " + sess.app.State.ZoneURL + " once, then run `sandbox add-user --no-access`.")
		}
		return false, nil
	}
	if r.access && r.added == 0 && !r.failed {
		return true, nil
	}
	return askMore(sess, r)
}

// askMore asks whether to add another user.
func askMore(sess *session, round userRound) (bool, error) {
	label := accessLabel(round.access)
	question, description := "Add another user "+label+"?", ""
	switch {
	case round.failed:
		question = "Try another user " + label + "?"
	case !round.access && round.added == 0:
		question = "Add a user without access?"
		description = "They are in the zone, but nothing lets the sandbox act for them, so the zone refuses " +
			"every request it makes. This shows the policies doing their job."
	}
	more := round.added == 0
	err := sess.ask(huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title(question).Description(description).Affirmative("Yes").Negative("No").Value(&more),
	)))
	return more, err
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

// askZone asks which zone the sandboxes use, and creates a new one when the
// person asks for it.
func askZone(ctx context.Context, sess *session) (string, error) {
	app := sess.app
	var zones []sandbox.Zone
	if err := app.UI.Step(ctx, "Zones listed", func(ctx context.Context) (string, error) {
		var err error
		zones, err = app.KC.ListZones(ctx)
		return fmt.Sprintf("%d in the organization", len(zones)), err
	}); err != nil {
		return "", errReported
	}
	labels := map[string]string{}
	options := make([]huh.Option[string], 0, len(zones)+1)
	for _, zone := range zones {
		labels[zone.ID] = zone.Name + " (" + zone.ID + ")"
		options = append(options, huh.NewOption(labels[zone.ID], zone.ID))
	}
	options = append(options, huh.NewOption("+ Create a new zone", createZoneOption))
	choice, name := app.State.ZoneID, "sandbox-demo"
	if err := sess.ask(huh.NewForm(
		huh.NewGroup(huh.NewSelect[string]().
			Title("Which zone should the sandboxes use?").
			Description("The zone holds the API servers, the sandbox application and its users.").
			Options(options...).Value(&choice)),
		huh.NewGroup(huh.NewInput().Title("Name the new zone").Value(&name).Validate(required)).
			WithHideFunc(func() bool { return choice != createZoneOption }),
	)); err != nil {
		return "", err
	}
	if choice != createZoneOption {
		sess.answer("Zone", labels[choice])
		return choice, nil
	}
	name = strings.TrimSpace(name)
	sess.answer("Zone", name+" (new)")
	return createZone(ctx, app, name)
}

// createZone creates a zone and responds with its ID.
func createZone(ctx context.Context, app *sandbox.App, name string) (string, error) {
	var zoneID string
	if err := app.UI.Step(ctx, "Zone "+name+" created", func(ctx context.Context) (string, error) {
		zone, err := app.KC.CreateZone(ctx, name)
		zoneID = zone.ID
		return zone.ID, err //nolint:wrapcheck // The step shows it.
	}); err != nil {
		return "", errReported
	}
	return zoneID, nil
}

// askResources asks which API servers the sandboxes may reach, and adds them
// to input without their keys.
func askResources(ctx context.Context, sess *session, input *sandbox.SetupInput) error {
	packages, err := apiKeyPackages(ctx, sess.app, input.ZoneID)
	if err != nil {
		return err
	}
	// What setup installed before is picked already, but only in its own zone.
	installed := func(slug string) bool {
		return sess.app.State.ZoneID == input.ZoneID && sess.app.State.HasResource(slug)
	}
	options := make([]huh.Option[string], 0, len(packages))
	var picked []string
	for _, pkg := range packages {
		label := pkg.Name
		if keyName := keyLabel(pkg); keyName != "" {
			label += " (" + keyName + ")"
		}
		options = append(options, huh.NewOption(label, pkg.Slug))
		if installed(pkg.Slug) || len(packages) == 1 {
			picked = append(picked, pkg.Slug)
		}
	}
	if err := sess.ask(huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Which APIs may the sandboxes reach?").
			Description("Space to pick, enter to continue. The agent calls GitHub and Linear in the demo.").
			Options(options...).Value(&picked).Validate(pickedOne),
	))); err != nil {
		return err
	}
	names := make([]string, 0, len(picked))
	for _, pkg := range packages {
		if slices.Contains(picked, pkg.Slug) {
			names = append(names, pkg.Name)
			input.APIKeys = append(input.APIKeys, sandbox.APIKey{Slug: pkg.Slug, Name: pkg.Name, Label: keyLabel(pkg)})
		}
	}
	sess.answer("APIs", strings.Join(names, ", "))
	return nil
}

// apiKeyPackages lists the zone's API server packages that take an API key,
// by name.
func apiKeyPackages(ctx context.Context, app *sandbox.App, zoneID string) ([]sandbox.Package, error) {
	var packages []sandbox.Package
	if err := app.UI.Step(ctx, "Catalog read", func(ctx context.Context) (string, error) {
		var err error
		if packages, err = app.KC.APIKeyPackages(ctx, zoneID); err != nil {
			return "", err //nolint:wrapcheck // The step shows it.
		}
		if len(packages) == 0 {
			return "", errNoAPIKeyPackages
		}
		return fmt.Sprintf("%d API servers take an API key", len(packages)), nil
	}); err != nil {
		return nil, errReported
	}
	slices.SortFunc(packages, func(a, b sandbox.Package) int { return strings.Compare(a.Name, b.Name) })
	return packages, nil
}

// askKeys asks, in one form, for each key setup does not have yet.
func askKeys(sess *session, input *sandbox.SetupInput) error {
	var fields []huh.Field
	var names []string
	for i := range input.APIKeys {
		key := &input.APIKeys[i]
		if key.Key != "" {
			continue
		}
		label := key.Label
		if label == "" {
			label = "API key"
		}
		names = append(names, key.Name)
		fields = append(fields, huh.NewInput().
			Title(key.Name+" · "+label).
			Description("Goes to the zone's vault. The sandbox never sees it.").
			EchoMode(huh.EchoModePassword).Value(&key.Key).Validate(required))
	}
	if len(fields) == 0 {
		return nil
	}
	if err := sess.ask(huh.NewForm(huh.NewGroup(fields...))); err != nil {
		return err
	}
	for i := range input.APIKeys {
		input.APIKeys[i].Key = strings.TrimSpace(input.APIKeys[i].Key)
	}
	sess.answer("Keys", "entered for "+strings.Join(names, ", "))
	return nil
}

// candidates lists the zone's users that are not in skip.
func candidates(ctx context.Context, app *sandbox.App, skip []string) ([]sandbox.ZoneUser, error) {
	users, err := app.KC.ListUsers(ctx, app.State.ZoneID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errListUsers, err)
	}
	return slices.DeleteFunc(users, func(user sandbox.ZoneUser) bool {
		return slices.ContainsFunc(skip, func(email string) bool { return strings.EqualFold(email, user.Email) })
	}), nil
}

// askEmail offers the given zone users, plus any other email.
func askEmail(sess *session, title string, users []sandbox.ZoneUser) (string, error) {
	options := make([]huh.Option[string], 0, len(users)+1)
	for _, user := range users {
		options = append(options, huh.NewOption(user.Email, user.Email))
	}
	options = append(options, huh.NewOption("+ Someone else", otherUserOption))
	choice, typed := "", ""
	typing := func() bool { return len(users) == 0 || choice == otherUserOption }
	if err := sess.ask(huh.NewForm(
		huh.NewGroup(huh.NewSelect[string]().Title(title).
			Description("People in the zone. Someone new has to sign in to the zone once first.").
			Options(options...).Value(&choice)).
			WithHideFunc(func() bool { return len(users) == 0 }),
		huh.NewGroup(huh.NewInput().Title(title).
			Description("Their email. They must already be in the zone.").
			Value(&typed).Validate(validEmail)).
			WithHideFunc(func() bool { return !typing() }),
	)); err != nil {
		return "", err
	}
	if typing() {
		return strings.TrimSpace(typed), nil
	}
	return choice, nil
}

func askRead(sess *session, input *sandbox.RunInput) error {
	if input.Read == "" {
		input.Read = defaultRead
	}
	if err := sess.ask(huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Which GitHub file should the agent read?").
			Description("OWNER/REPO/PATH. A private repo shows the agent using the user's access.").
			Value(&input.Read).Validate(required),
	))); err != nil {
		return err
	}
	sess.answer("File", input.Read)
	return nil
}

func required(value string) error {
	if strings.TrimSpace(value) == "" {
		return errRequired
	}
	return nil
}

func pickedOne(picked []string) error {
	if len(picked) == 0 {
		return errPickOne
	}
	return nil
}

func validEmail(value string) error {
	if _, err := mail.ParseAddress(strings.TrimSpace(value)); err != nil {
		return errNotAnEmail
	}
	return nil
}
