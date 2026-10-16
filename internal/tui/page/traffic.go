package page

import (
	"cmp"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/keycardai/agent-sandbox-examples/internal/tui/component"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
)

// nameWidth and targetWidth are the columns of what Keycard did.
const nameWidth, targetWidth = 12, 24

// keycardLines says what the proxy did for each of the demo's endpoints,
// which hosts it blocked, and how many names the agent looked up. There is
// one line per endpoint and two more, however much traffic there is. When
// the proxy's log cannot be read, it says why instead.
func (s *Shared) keycardLines() []string {
	if err := s.Traffic.Err(); err != nil {
		return []string{
			s.Styles.Failed.Render("✕ Cannot read the proxy's log: " + component.FirstLine(err.Error())),
			s.Styles.Dim.Render("  The TUI tries again every few seconds."),
		}
	}
	records, _ := s.Traffic.Snapshot()
	uses, blocked := proxylog.Summarize(records, s.endpoints())
	lines := make([]string, 0, len(uses)+half)
	for _, use := range uses {
		lines = append(lines, s.useLine(use))
	}
	return append(lines, s.blockedLine(blocked), s.lookupLine(proxylog.Count(records)))
}

// useLine is one endpoint: its resource, where it is, and what came of the
// requests to it.
func (s *Shared) useLine(use proxylog.EndpointUse) string {
	theme := s.Styles
	endpoint := use.Endpoint
	// An endpoint with a credential names its resource, then where it is.
	// One without names only its host, across both columns.
	where := theme.Text.Render(component.Pad(endpoint.Host(), nameWidth+1+targetWidth))
	if endpoint.Credential {
		where = theme.Text.Render(component.Pad(cmp.Or(endpoint.Name, endpoint.Host()), nameWidth)) + " " +
			theme.Muted.Render(component.Pad(strings.TrimPrefix(endpoint.URL, "https://"), targetWidth))
	}
	icon, detail := s.useDetail(use)
	return icon + " " + where + " " + detail
}

// useDetail marks what came of the requests to an endpoint, and says it:
// how many went through, whether Keycard refused the last one's
// credential, what the server answered when that was an error, and when
// the last one was. A server's error comes after Keycard's part: the
// credential was added, and the server refused the request.
func (s *Shared) useDetail(use proxylog.EndpointUse) (string, string) {
	theme := s.Styles
	refusedLast := use.Last != nil && use.Last.RefusedCredential()
	icon, detail := theme.Faint.Render("·"), theme.Dim.Render("not used yet")
	switch {
	case refusedLast:
		icon = theme.Failed.Render("✕")
		detail = theme.Failed.Render("No credential: " + proxylog.ReasonText(use.Last.Decision.Reason))
	case use.Allowed > 0 && use.Endpoint.Credential:
		icon, detail = theme.OK.Render("✓"), theme.Text.Render(component.Plural(use.Allowed, "request")+
			", credential added")
	case use.Allowed > 0:
		icon, detail = theme.OK.Render("✓"), theme.Text.Render(component.Plural(use.Allowed, "request")+
			", no credential needed")
	}
	if use.Refused > 0 && !refusedLast {
		detail += theme.Warn.Render(fmt.Sprintf(", %d refused", use.Refused))
	}
	if use.Last != nil {
		detail += theme.Warn.Render(serverError(use.Last)) + theme.Dim.Render(" · "+proxylog.Clock(use.Last))
	}
	return icon, detail
}

// serverError says what the server answered an allowed request, when that
// was an error, or returns "".
func serverError(record *proxylog.Record) string {
	if record.Denied() || record.Outcome == nil || record.Outcome.Status < http.StatusBadRequest {
		return ""
	}
	return ", but the server answered " + strconv.Itoa(record.Outcome.Status) + " " +
		http.StatusText(record.Outcome.Status)
}

// blockedLine names the hosts the proxy refused, newest first.
func (s *Shared) blockedLine(blocked []proxylog.BlockedHost) string {
	theme := s.Styles
	if len(blocked) == 0 {
		return theme.Faint.Render("·") + " " + theme.Text.Render(component.Pad("Blocked", nameWidth)) + " " +
			theme.Dim.Render("nothing yet. A host off the allow list shows here.")
	}
	hosts := make([]string, 0, len(blocked))
	for _, host := range blocked {
		hosts = append(hosts, host.Host)
	}
	return theme.Failed.Render("✕") + " " + theme.Text.Render(component.Pad("Blocked", nameWidth)) + " " +
		theme.Failed.Render(strings.Join(hosts, ", ")) + theme.Dim.Render(" · "+proxylog.ReasonText(blocked[0].Reason))
}

// lookupLine counts the agent's DNS lookups.
func (s *Shared) lookupLine(counts proxylog.Counts) string {
	theme := s.Styles
	return theme.Faint.Render("·") + " " + theme.Text.Render(component.Pad("DNS", nameWidth)) + " " +
		theme.Dim.Render(fmt.Sprintf(
			"%s, %d refused. The resolver tries cluster names first, so refusals are normal.",
			component.Plural(counts.Lookups, "lookup"), counts.LookupsDenied,
		))
}
