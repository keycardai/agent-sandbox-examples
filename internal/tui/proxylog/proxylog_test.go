package proxylog_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
	"github.com/keycardai/agent-sandbox-examples/internal/tui/proxylog"
)

const (
	githubHost     = "api.github.com"
	githubEndpoint = "api.github.com:443/"
	sampleDenied   = "example.com"
	sampleMethod   = "GET"
	sampleAllowed  = "endpoint_allowed"
	sampleReason   = "no_matching_endpoint"
	verdictAllow   = "allow"
	verdictDeny    = "deny"
	scopeRequest   = "request"
)

// newRecord makes a proxy record. A request record gets a GET of path and,
// when allowed, a 200.
func newRecord(stamp time.Time, scope, host, path, verdict, reason, endpoint string) proxylog.Record {
	record := proxylog.Record{TS: stamp, Scope: scope}
	record.Destination.Host = host
	record.Decision.Verdict, record.Decision.Reason, record.Decision.Endpoint = verdict, reason, endpoint
	switch {
	case scope == proxylog.ScopeDNS:
		record.Query = &proxylog.Query{Type: "A", Rcode: "NOERROR"}
	case verdict == verdictAllow:
		record.Request = &proxylog.Request{Method: sampleMethod, Path: path}
		record.Outcome = &proxylog.Outcome{Status: http.StatusOK}
	default:
		record.Request = &proxylog.Request{Method: sampleMethod, Path: path}
	}
	return record
}

// sampleRecords are what the proxy writes for a lookup, a search domain
// the resolver tried, a GitHub request with the credential, a pypi.org
// request, an OpenRouter request Keycard refused, and a host off the allow
// list.
func sampleRecords(stamp time.Time) []proxylog.Record {
	return []proxylog.Record{
		newRecord(stamp, proxylog.ScopeDNS, githubHost, "", verdictAllow, sampleAllowed, githubEndpoint),
		newRecord(stamp, proxylog.ScopeDNS, "api.github.com.default.svc.cluster.local", "", verdictDeny, sampleReason,
			""),
		newRecord(stamp.Add(time.Second), scopeRequest, githubHost, "/user", verdictAllow, sampleAllowed, githubEndpoint),
		newRecord(stamp.Add(2*time.Second), scopeRequest, "pypi.org", "/simple/", verdictAllow, sampleAllowed,
			"pypi.org:443/"),
		newRecord(stamp.Add(3*time.Second), scopeRequest, "openrouter.ai", "/api/v1/key", verdictDeny, "exchange_denied",
			"openrouter.ai:443/api/v1"),
		newRecord(stamp.Add(4*time.Second), proxylog.ScopeDNS, sampleDenied, "", verdictDeny, sampleReason, ""),
	}
}

// piEndpoints are the pi demo's endpoints: three with a credential, and
// two without.
func piEndpoints() []sandbox.ScenarioEndpoint {
	return []sandbox.ScenarioEndpoint{
		{URL: "https://api.github.com", Credential: true, Name: "GitHub"},
		{URL: "https://github.com", Credential: true, Name: "GitHub git"},
		{URL: "https://openrouter.ai/api/v1", Credential: true, Name: "OpenRouter"},
		{URL: "https://pypi.org"},
		{URL: "https://files.pythonhosted.org"},
	}
}

// TestSummarize sorts the requests by endpoint, counts what Keycard
// refused, and names the blocked hosts without the cluster's own names.
func TestSummarize(t *testing.T) {
	t.Parallel()
	records := sampleRecords(time.Now())
	records = append(records,
		newRecord(time.Now(), scopeRequest, "evil.test", "/", verdictDeny, sampleReason, ""),
		newRecord(time.Now(), proxylog.ScopeDNS, "api.github.com.ec2.internal", "", verdictDeny, sampleReason, ""),
		newRecord(time.Now(), scopeRequest, githubHost, "/user", verdictDeny, "upstream_failed", githubEndpoint))
	uses, blocked := proxylog.Summarize(records, piEndpoints())
	got := make([]string, 0, len(uses))
	for _, use := range uses {
		got = append(got, fmt.Sprintf("%s %d allowed %d refused", use.Endpoint.Host(), use.Allowed, use.Refused))
	}
	want := []string{
		"api.github.com 1 allowed 0 refused", "github.com 0 allowed 0 refused", "openrouter.ai 0 allowed 1 refused",
		"pypi.org 1 allowed 0 refused", "files.pythonhosted.org 0 allowed 0 refused",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("uses =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if last := uses[2].Last; last == nil || !last.RefusedCredential() {
		t.Errorf("OpenRouter's last record = %+v, want the refused credential", last)
	}
	if len(blocked) != 2 || blocked[0].Host != "evil.test" || blocked[1].Host != sampleDenied {
		t.Errorf("blocked = %+v, want evil.test then example.com", blocked)
	}
}

// TestRecordFor finds a request's record, falls back to a refused lookup,
// and leaves out records from before the request.
func TestRecordFor(t *testing.T) {
	t.Parallel()
	stamp := time.Now()
	records := sampleRecords(stamp)
	if record, ok := proxylog.RecordFor(records, githubHost, stamp); !ok || record.Scope != scopeRequest {
		t.Errorf("GitHub = %+v, %v; want the request, not the lookup", record, ok)
	}
	if record, ok := proxylog.RecordFor(records, sampleDenied, stamp); !ok || record.Scope != proxylog.ScopeDNS ||
		!record.Denied() {
		t.Errorf("example.com = %+v, %v; want the refused lookup", record, ok)
	}
	if _, ok := proxylog.RecordFor(records, githubHost, stamp.Add(time.Hour)); ok {
		t.Error("a record from before since was found")
	}
}

// TestReadableLine turns the proxy's audit records and structured log
// lines into rows, and leaves other lines alone.
func TestReadableLine(t *testing.T) {
	t.Parallel()
	audit := `{"ts":"2026-10-09T17:31:44.259271137Z","sandbox_id":"rc58iso9q4qhg3xiur5l81l5qg",` +
		`"document":{"digest":"sha256:ab"},"config":{"digest":"sha256:cd"},"scope":"request",` +
		`"destination":{"host":"api.github.com","port":443,"sni":"api.github.com"},` +
		`"request":{"method":"GET","path":"/user","host":"api.github.com"},` +
		`"decision":{"verdict":"allow","reason":"endpoint_allowed","endpoint":"api.github.com:443/"},` +
		`"outcome":{"status":200}}`
	row := proxylog.ReadableLine(audit)
	for _, want := range []string{"✓ allow", sampleMethod, "api.github.com/user", "200", githubEndpoint} {
		if !strings.Contains(row, want) || strings.Contains(row, "sandbox_id") {
			t.Errorf("audit row %q lacks %q, or keeps the JSON", row, want)
		}
	}
	structured := `{"time":"2026-10-09T17:31:44.25Z","level":"WARN","msg":"exchange failed",` +
		`"resource":"https://api.github.com","attempt":2}`
	//nolint:gosmopolitan // ReadableLine prints times in the operator's zone.
	when := time.Date(2026, 10, 9, 17, 31, 44, 0, time.UTC).Local().Format(time.TimeOnly)
	want := when + "  WARN  exchange failed  attempt=2 resource=https://api.github.com"
	if got := proxylog.ReadableLine(structured); got != want {
		t.Errorf("structured line = %q, want %q", got, want)
	}
	for _, line := range []string{"plain text from the agent", `{"no":"message"}`, "{not json"} {
		if got := proxylog.ReadableLine(line); got != line {
			t.Errorf("ReadableLine(%q) = %q, want it unchanged", line, got)
		}
	}
}
