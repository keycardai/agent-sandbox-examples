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

// keycardLines says what the proxy did for each endpoint, then the blocked
// hosts and DNS lookups. Its length does not depend on the traffic.
func (v view) keycardLines() []string {
	if err := v.state.Agents.Traffic.Err(); err != nil {
		return []string{
			v.ui.Styles.Failed.Render("✕ Cannot read the proxy's log: " + component.FirstLine(err.Error())),
			v.ui.Styles.Dim.Render("  The TUI tries again every few seconds."),
		}
	}
	records, _ := v.state.Agents.Traffic.Snapshot()
	uses, blocked := proxylog.Summarize(records, v.endpoints())
	lines := make([]string, 0, len(uses)+half)
	for _, use := range uses {
		lines = append(lines, v.useLine(use))
	}
	return append(lines, v.blockedLine(blocked), v.lookupLine(v.state.Agents.Traffic.Lookups()))
}

// useLine says what came of the requests to one endpoint.
func (v view) useLine(use proxylog.EndpointUse) string {
	theme := v.ui.Styles
	endpoint := use.Endpoint
	where := theme.Text.Render(component.Pad(endpoint.Host(), nameWidth+1+targetWidth))
	if endpoint.Credential {
		where = theme.Text.Render(component.Pad(cmp.Or(endpoint.Name, endpoint.Host()), nameWidth)) + " " +
			theme.Muted.Render(component.Pad(strings.TrimPrefix(endpoint.URL, "https://"), targetWidth))
	}
	icon, detail := v.useDetail(use)
	return icon + " " + where + " " + detail
}

// useDetail marks and says what came of the requests to an endpoint, with
// useRest after it.
func (v view) useDetail(use proxylog.EndpointUse) (string, string) {
	theme := v.ui.Styles
	icon, detail := theme.Faint.Render("·"), theme.Dim.Render("not used yet")
	switch {
	case refusedLast(use):
		icon = theme.Failed.Render("✕")
		detail = theme.Failed.Render("No credential: " + proxylog.ReasonText(use.Last.Decision.Reason))
	case use.Allowed > 0 && use.Endpoint.Credential:
		icon, detail = theme.OK.Render("✓"), theme.Text.Render(component.Plural(use.Allowed, "request")+
			", credential added")
	case use.Allowed > 0:
		icon, detail = theme.OK.Render("✓"), theme.Text.Render(component.Plural(use.Allowed, "request")+
			", no credential needed")
	case use.Failed > 0:
		icon = theme.Failed.Render("✕")
		detail = theme.Failed.Render(component.Plural(use.Failed, "request") + ", the server did not answer")
	}
	return icon, detail + v.useRest(use)
}

// useRest adds the counts useDetail did not say, any server error on the
// last request, and when the last request was.
func (v view) useRest(use proxylog.EndpointUse) string {
	theme := v.ui.Styles
	rest := ""
	if use.Refused > 0 && !refusedLast(use) {
		rest += theme.Warn.Render(fmt.Sprintf(", %d refused", use.Refused))
	}
	if use.Failed > 0 && (use.Allowed > 0 || refusedLast(use)) {
		rest += theme.Warn.Render(fmt.Sprintf(", %d not answered", use.Failed))
	}
	if use.Last != nil {
		rest += theme.Warn.Render(serverError(use.Last)) + theme.Dim.Render(" · "+proxylog.Clock(use.Last))
	}
	return rest
}

// refusedLast reports whether the last request had its credential refused.
func refusedLast(use proxylog.EndpointUse) bool {
	return use.Last != nil && use.Last.RefusedCredential()
}

// serverError names the error status the server gave an allowed request.
func serverError(record *proxylog.Record) string {
	if record.Denied() || record.Outcome == nil || record.Outcome.Status < http.StatusBadRequest {
		return ""
	}
	return ", but the server answered " + strconv.Itoa(record.Outcome.Status) + " " +
		http.StatusText(record.Outcome.Status)
}

// blockedLine names the hosts the proxy refused, newest first.
func (v view) blockedLine(blocked []proxylog.BlockedHost) string {
	theme := v.ui.Styles
	if len(blocked) == 0 {
		return theme.Faint.Render("·") + " " + theme.Text.Render(component.Pad("Blocked", nameWidth)) + " " +
			theme.Dim.Render("nothing yet. A host off the allow list shows here.")
	}
	// Group the hosts by reason, the newest reason first.
	var reasons []string
	hosts := map[string][]string{}
	for _, host := range blocked {
		if _, ok := hosts[host.Reason]; !ok {
			reasons = append(reasons, host.Reason)
		}
		hosts[host.Reason] = append(hosts[host.Reason], host.Host)
	}
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, theme.Failed.Render(strings.Join(hosts[reason], ", "))+
			theme.Dim.Render(" · "+proxylog.ReasonText(reason)))
	}
	return theme.Failed.Render("✕") + " " + theme.Text.Render(component.Pad("Blocked", nameWidth)) + " " +
		strings.Join(parts, theme.Dim.Render("; "))
}

// lookupLine counts the agent's DNS lookups.
func (v view) lookupLine(counts proxylog.Counts) string {
	theme := v.ui.Styles
	return theme.Faint.Render("·") + " " + theme.Text.Render(component.Pad("DNS", nameWidth)) + " " +
		theme.Dim.Render(fmt.Sprintf(
			"%s, %d refused. The resolver tries cluster names first, so refusals are normal.",
			component.Plural(counts.Lookups, "lookup"), counts.LookupsDenied,
		))
}
