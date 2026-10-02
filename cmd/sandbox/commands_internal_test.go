package main

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

const githubKey = "ghp_1"

// catalog is what the catalog lists as the API servers that take a key.
func catalog(t *testing.T) []sandbox.Package {
	t.Helper()
	var packages []sandbox.Package
	err := json.Unmarshal([]byte(`[
		{"slug":"github","name":"GitHub API","form":{"fields":[{"name":"api_key","label":"Personal Access Token"}]}},
		{"slug":"linear","name":"Linear API","form":{"fields":[{"name":"api_key","label":"Personal API Key"}]}}
	]`), &packages)
	if err != nil {
		t.Fatal(err)
	}
	return packages
}

func TestParseResourcesReadsEachKeyFromItsVariable(t *testing.T) {
	t.Parallel()
	environment := map[string]string{"GH_TOKEN": githubKey, "LINEAR_KEY": "lin_1"}
	getenv := func(name string) string { return environment[name] }

	keys, err := parseResources(
		[]string{"github=GH_TOKEN", "linear=LINEAR_KEY"}, catalog(t), false, getenv,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []sandbox.APIKey{
		{Slug: sandbox.SlugGitHub, Name: githubAPI, Label: "Personal Access Token", Key: githubKey},
		{Slug: sandbox.SlugLinear, Name: linearAPI, Label: "Personal API Key", Key: "lin_1"},
	}
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %+v, want %+v", keys, want)
	}
}

func TestParseResourcesRefusesWhatSetupCannotInstall(t *testing.T) {
	t.Parallel()
	getenv := func(name string) string {
		if name == "GH_TOKEN" {
			return githubKey
		}
		return ""
	}

	for _, tc := range []struct {
		name        string
		spec        string
		interactive bool
		want        error
	}{
		{name: "an API server that takes no key", spec: "gitlab=GH_TOKEN", want: errUnknownResource},
		{name: "an empty variable", spec: "github=UNSET", want: errEmptyKey},
		{name: "no variable, and no one to ask", spec: "github", want: errNoKeyVariable},
		{name: "no variable, with someone to ask", spec: "github", interactive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			keys, err := parseResources([]string{tc.spec}, catalog(t), tc.interactive, getenv)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == nil && (len(keys) != 1 || keys[0].Key != "") {
				t.Errorf("keys = %+v, want github with its key left to ask for", keys)
			}
		})
	}
}

// TestUserRoundAsksOnlyWhenItShould checks when the wizard asks for another
// user. A session with no terminal refuses to ask, which marks the cases that
// do.
func TestUserRoundAsksOnlyWhenItShould(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		round      userRound
		candidates int
		more       bool
		asks       bool
		explains   bool
	}{
		{name: "the first user with access", round: userRound{access: true}, candidates: 2, more: true},
		{name: "the first user with access, typed in", round: userRound{access: true}, more: true},
		{name: "another user with access", round: userRound{access: true, added: 1}, candidates: 2, asks: true},
		{name: "no one left to add with access", round: userRound{access: true, added: 1}},
		{name: "after a user that failed", round: userRound{access: true, failed: true}, asks: true},
		{name: "a user without access", round: userRound{}, candidates: 1, asks: true},
		{name: "no one left to add without access", round: userRound{}, explains: true},
		{name: "no one left after one without access", round: userRound{added: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var infos int
			report := &sandbox.UI{Emit: func(event sandbox.Event) {
				if event.Kind == sandbox.EventInfo {
					infos++
				}
			}}
			sess := &session{app: &sandbox.App{UI: report, State: &sandbox.State{}}}

			more, err := tc.round.another(sess, tc.candidates)

			if asked := errors.Is(err, errNeedsTerminal); asked != tc.asks {
				t.Fatalf("asked = %v (err %v), want %v", asked, err, tc.asks)
			}
			if !tc.asks && more != tc.more {
				t.Errorf("more = %v, want %v", more, tc.more)
			}
			if explained := infos > 0; explained != tc.explains {
				t.Errorf("explained = %v, want %v", explained, tc.explains)
			}
		})
	}
}

func TestFormsValidateWhatTheyAskFor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{name: "a blank answer", err: required("  "), want: errRequired},
		{name: "an answer", err: required("sandbox-demo")},
		{name: "an email", err: validEmail(" ada@acme.example ")},
		{name: "not an email", err: validEmail("ada"), want: errNotAnEmail},
		{name: "nothing picked", err: pickedOne(nil), want: errPickOne},
		{name: "one picked", err: pickedOne([]string{sandbox.SlugGitHub})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !errors.Is(tc.err, tc.want) {
				t.Errorf("err = %v, want %v", tc.err, tc.want)
			}
		})
	}
}
