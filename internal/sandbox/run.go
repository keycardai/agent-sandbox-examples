package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

var (
	errNoSandbox      = errors.New("the user has no sandbox yet; run `sandbox add-user`")
	errNoUsers        = errors.New("no users yet; run `sandbox add-user`")
	errNoProbes       = errors.New("the agent has no call for the installed API servers; set up GitHub or Linear")
	errRunMismatch    = errors.New("a user's agent did not get what their access allows")
	errNotRefused     = errors.New("expected the zone to refuse the exchange, but the call went through")
	errUnexpectedCall = errors.New("the call did not get what the user's access allows")
)

// RunInput is what run needs.
type RunInput struct {
	// Read is the GitHub file the agent reads, as OWNER/REPO/PATH. Only a
	// setup with GitHub needs it.
	Read string
	// Emails limits the run to these users. Empty runs every user.
	Emails []string
}

// Result is what one call by one user's agent got.
type Result struct {
	User     *User
	Resource string
	API      string
	Fetch    Fetch
	// Answer is what the API said, shown the way the call presents it.
	Answer string
	// Reason and Detail are why the proxy refused the request, from its
	// audit record. Both are empty when the proxy let it through.
	Reason, Detail string
	// Matches says the result is what the user's access allows: an answer
	// with access, the zone's refusal without.
	Matches bool
}

// Run has the agent in each user's sandbox call every installed API it has a
// call for, and prints what each call got: the answer for a user with
// access, the zone's refusal for a user without. It fails when any call got
// something other than that.
func (a *App) Run(ctx context.Context, in RunInput) ([]Result, error) {
	users, err := a.runUsers(in.Emails)
	if err != nil {
		return nil, err
	}
	probes, err := probesFor(a.State.Resources, in.Read)
	if err != nil {
		return nil, err
	}
	if len(probes) == 0 {
		return nil, errNoProbes
	}
	var results []Result
	failed := false
	for _, user := range users {
		a.UI.Section("Agent acting for " + user.Email + " (" + accessText(user.Access) + ")")
		for _, call := range probes {
			result, err := a.runProbe(ctx, user, call)
			failed = failed || err != nil
			results = append(results, result)
		}
	}
	if failed {
		return results, errRunMismatch
	}
	return results, nil
}

// runUsers responds with the users the run is for: these emails, or every
// user with a sandbox.
func (a *App) runUsers(emails []string) ([]*User, error) {
	if len(emails) == 0 {
		users := a.State.SortedUsers()
		if len(users) == 0 {
			return nil, errNoUsers
		}
		return users, nil
	}
	users := make([]*User, 0, len(emails))
	for _, email := range emails {
		user := a.State.Users[strings.ToLower(email)]
		if user == nil {
			return nil, fmt.Errorf("%w: %s", errNoSandbox, email)
		}
		users = append(users, user)
	}
	return users, nil
}

// runProbe makes one call from the user's sandbox, checks it against the
// user's access, and prints the answer or the refusal under the step.
func (a *App) runProbe(ctx context.Context, user *User, call probe) (Result, error) {
	result := Result{User: user, Resource: call.Resource, API: call.API}
	err := a.UI.Step(ctx, call.Title, func(ctx context.Context) (string, error) {
		fetched, err := a.fetch(ctx, user.Pod, call.Args)
		if err != nil {
			return "", err
		}
		result.Fetch = fetched
		var answerErr error
		if fetched.Status == http.StatusOK {
			result.Answer, answerErr = call.Show(fetched.Body)
		} else {
			result.Reason, result.Detail = a.refusal(ctx, user.Pod)
		}
		return judge(&result, answerErr)
	})
	switch {
	case result.Answer != "":
		a.UI.Quote(result.Answer)
	case result.Detail != "":
		a.UI.Quote(result.Detail)
	case result.Fetch.Status != http.StatusOK && result.Reason == "" && result.Fetch.Body != "":
		a.UI.Quote(result.Fetch.Body)
	}
	return result, err
}

// judge sets whether the call got what the user's access allows, and responds
// with the step's detail, or why it did not. A user with access must get an
// answer. A user without access must be refused by the zone: the proxy's
// exchange_* reasons are the zone's refusal, and any other failure is a
// broken sandbox, not a refusal.
func judge(result *Result, answerErr error) (string, error) {
	access := result.User.Access
	status := result.Fetch.Status
	answered := status == http.StatusOK && answerErr == nil
	refused := strings.HasPrefix(result.Reason, "exchange_")
	result.Matches = (access && answered) || (!access && refused)
	switch {
	case !result.Matches:
		return "", mismatch(result, answerErr)
	case access:
		return "HTTP " + strconv.Itoa(status), nil
	default:
		return "refused · HTTP " + strconv.Itoa(status) + " " + result.Reason, nil
	}
}

// mismatch says how a call missed what the user's access allows.
func mismatch(result *Result, answerErr error) error {
	status := result.Fetch.Status
	switch {
	case !result.User.Access && status == http.StatusOK:
		return errNotRefused
	case answerErr != nil:
		return answerErr
	case result.Reason != "":
		return fmt.Errorf("%w: HTTP %d %s", errUnexpectedCall, status, result.Reason)
	default:
		return fmt.Errorf("%w: HTTP %d from the API", errUnexpectedCall, status)
	}
}

// refusal reads why the proxy refused the last request from its audit
// record: the reason, such as exchange_denied, and the zone's error when the
// record has one.
func (a *App) refusal(ctx context.Context, pod string) (string, string) {
	logs, err := a.sandboxKube().Logs(ctx, a.State.Namespace, pod, "proxy")
	if err != nil {
		return "", ""
	}
	return lastRefusal(logs)
}

// lastRefusal finds the last deny record in the proxy's JSON log lines.
func lastRefusal(logs string) (string, string) {
	const zoneError = "oauth error "
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	for _, line := range slices.Backward(lines) {
		var record struct {
			Decision struct {
				Verdict string `json:"verdict"`
				Reason  string `json:"reason"`
				Error   string `json:"error"`
			} `json:"decision"`
		}
		if json.Unmarshal([]byte(line), &record) != nil || record.Decision.Verdict != "deny" {
			continue
		}
		detail := record.Decision.Error
		if i := strings.Index(detail, zoneError); i >= 0 {
			detail = detail[i+len(zoneError):]
		}
		return record.Decision.Reason, detail
	}
	return "", ""
}
