package sandbox

import (
	"context"
	"fmt"
	"slices"

	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

// AddAgent gives one more of the zone's users an agent in the demo the
// settings name. It signs in with the saved account, asks who with l.Ask,
// asks the demo's per-user inputs for them, and keeps the settings, saved
// if they were. It returns their email, or ErrNoTerminal when l.Ask is nil.
func (l *Local) AddAgent(ctx context.Context) (string, error) {
	if l.Ask == nil {
		return "", ErrNoTerminal
	}
	w := l.newWizard()
	if err := w.signInAgain(ctx); err != nil {
		return "", err
	}
	if err := w.keepZone(ctx); err != nil {
		return "", err
	}
	email, err := w.chooseNewUser(ctx)
	if err != nil {
		return "", err
	}
	w.addPerson(email)
	if err = w.keepScenario(ctx); err != nil {
		return "", err
	}
	if err = w.keepCluster(ctx); err != nil {
		return "", err
	}
	return email, w.write(l.Store.Saved())
}

// QuestionNewUser is the question AddAgent asks.
const QuestionNewUser = "Who else gets an agent in this demo?"

// chooseNewUser asks which of the zone's users without an agent in the demo
// gets one. It lists them again until the operator picks one. It says once
// that everyone has an agent, however often the operator checks again.
func (w *wizard) chooseNewUser(ctx context.Context) (string, error) {
	w.section(SectionZone)
	have := w.people()
	told := false
	for {
		users, err := w.zone.Users(ctx)
		if err != nil {
			return "", fmt.Errorf("listing the zone's users: %w", err)
		}
		var options []Option
		for _, user := range users {
			if !slices.Contains(have, user.Email) {
				options = append(options, Option{Label: user.Email, Value: user.Email})
			}
		}
		if len(options) == 0 && !told {
			told = true
			_, _ = fmt.Fprintln(w.l.Out, "Everyone in the zone has an agent in this demo. Add the person in the Keycard "+
				"console, or have them sign in to the zone. Then check again.")
		}
		options = append(options, Option{Label: "Not in the list? Check again", Value: checkAgain})
		choice, err := w.choose(ctx, QuestionNewUser, options, options[0].Value)
		if err != nil || choice != checkAgain {
			return choice, err
		}
	}
}

// addPerson gives email an agent in the demo the wizard asks about.
func (w *wizard) addPerson(email string) {
	if w.demos == nil {
		w.demos = map[string][]string{}
	}
	demo := w.demo()
	w.demos[demo] = append(w.demos[demo], email)
}

// removeUser takes email out of the people with an agent in the demo the
// settings name, and drops their answers when no demo has an agent for
// them.
func (l *Local) removeUser(ctx context.Context, email string) error {
	w := l.newWizard()
	demo := l.demo()
	switch {
	case email == w.prev.agents.Default:
		return errDefaultAgent(email)
	case !slices.Contains(w.demos[demo], email):
		return fmt.Errorf("%w: %s has no agent in this demo", ErrSettings, email)
	}
	// Without a sign-in, keeping the zone reads only the files.
	w.keepAccount()
	if err := w.keepZone(ctx); err != nil {
		return err
	}
	w.demos[demo] = slices.DeleteFunc(slices.Clone(w.demos[demo]), func(user string) bool { return user == email })
	if len(w.demos[demo]) == 0 {
		delete(w.demos, demo)
	}
	w.keepAnswers()
	if err := w.keepCluster(ctx); err != nil {
		return err
	}
	return w.write(l.Store.Saved())
}

// errDefaultAgent is why the agent of email, the person setup picked,
// cannot be removed.
func errDefaultAgent(email string) error {
	return fmt.Errorf("%w: every demo has an agent for %s, the person setup picked. To remove it, pick someone "+
		"else in setup", ErrSettings, email)
}

// agentProblems returns why the demo cannot load, or why it has no values
// for the agent of one of users, such as an answer missing.
func (l *Local) agentProblems(users []zonesetup.UserValues) []error {
	scenario, err := l.scenario()
	if err != nil {
		return []error{err}
	}
	answers := l.answers()
	var problems []error
	for _, user := range users {
		if _, err = scenario.Values(answers.forUser(user.Email)); err != nil {
			problems = append(problems, fmt.Errorf("%s's agent: %w", user.Email, err))
		}
	}
	return problems
}
