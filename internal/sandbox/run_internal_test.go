package sandbox

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

var errQuery = errors.New("the query failed")

const exchangeDenied = "exchange_denied"

// TestJudge checks what counts as getting what a user's access allows: an
// answer with access, and the zone's refusal of the exchange without. Any
// other failure is a broken sandbox.
func TestJudge(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		access    bool
		status    int
		reason    string
		answerErr error
		matches   bool
		failure   error
	}{
		{name: "an answer with access", access: true, status: http.StatusOK, matches: true},
		{name: "a refusal without access", status: http.StatusBadGateway, reason: "exchange_failed", matches: true},
		{name: "a denial without access", status: http.StatusForbidden, reason: exchangeDenied, matches: true},
		{name: "an answer without access", status: http.StatusOK, failure: errNotRefused},
		{
			name: "a refusal with access", access: true, status: http.StatusForbidden,
			reason: exchangeDenied, failure: errUnexpectedCall,
		},
		{
			name: "a broken sandbox without access is not a refusal", status: http.StatusBadGateway,
			reason: "upstream_failed", failure: errUnexpectedCall,
		},
		{name: "an API error with access", access: true, status: http.StatusNotFound, failure: errUnexpectedCall},
		{
			name: "an answer that says the call failed", access: true, status: http.StatusOK,
			answerErr: errQuery, failure: errQuery,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := Result{User: &User{Access: tc.access}, Fetch: Fetch{Status: tc.status}, Reason: tc.reason}
			_, err := judge(&result, tc.answerErr)
			if result.Matches != tc.matches {
				t.Errorf("Matches = %v, want %v", result.Matches, tc.matches)
			}
			if !errors.Is(err, tc.failure) {
				t.Errorf("err = %v, want %v", err, tc.failure)
			}
		})
	}
}

func TestLastRefusalReadsTheZonesError(t *testing.T) {
	t.Parallel()

	logs := strings.Join([]string{
		`{"msg":"refused a request","decision":{"verdict":"deny","reason":"exchange_failed","error":"old"}}`,
		`not json`,
		`{"msg":"refused a request","decision":{"verdict":"deny","reason":"exchange_denied",` +
			`"error":"resource https://api.github.com: the token exchange failed: oauth error access_denied: no policy"}}`,
		`{"msg":"allowed a request","decision":{"verdict":"allow","reason":"endpoint_allowed"}}`,
	}, "\n")

	reason, detail := lastRefusal(logs)
	if reason != exchangeDenied || detail != "access_denied: no policy" {
		t.Errorf("lastRefusal = %q, %q, want the last deny and the zone's error", reason, detail)
	}
	if reason, _ := lastRefusal(`{"decision":{"verdict":"allow"}}`); reason != "" {
		t.Errorf("an allow-only log has refusal %q", reason)
	}
}

func TestProbesForTheInstalledAPIs(t *testing.T) {
	t.Parallel()

	resources := []Resource{
		{Slug: SlugGitHub, APIURL: "https://api.github.com"},
		{Slug: "sentry", APIURL: "https://sentry.io/api/0"},
		{Slug: SlugLinear, APIURL: "https://api.linear.app"},
	}
	probes, err := probesFor(resources, "owner/repo/docs/demo.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 2 || probes[0].Resource != SlugGitHub || probes[1].Resource != SlugLinear {
		t.Fatalf("probes = %+v, want GitHub and Linear, and nothing for an API without a call", probes)
	}
	const wantTarget = "https://api.github.com/repos/owner/repo/contents/docs/demo.txt"
	if target := probes[0].Args[len(probes[0].Args)-1]; target != wantTarget {
		t.Errorf("GitHub reads %s, want %s", target, wantTarget)
	}
	if _, err := probesFor(resources, "owner/repo"); !errors.Is(err, errReadPath) {
		t.Errorf("a read without a path = %v, want errReadPath", err)
	}
}

func TestLinearProbeShowsWhoTheAgentActsAs(t *testing.T) {
	t.Parallel()

	show := linearProbe("https://api.linear.app").Show
	got, err := show(`{"data":{"viewer":{"name":"Ada","email":"ada@acme.example"},"organization":{"name":"Acme"}}}`)
	if err != nil || got != "Ada <ada@acme.example> in the Acme workspace" {
		t.Errorf("Show = %q, %v", got, err)
	}
	if _, err := show(`{"errors":[{"message":"Authentication required"}]}`); !errors.Is(err, errLinearQuery) {
		t.Errorf("a GraphQL error = %v, want errLinearQuery", err)
	}
}
