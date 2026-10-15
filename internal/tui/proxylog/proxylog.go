// Package proxylog reads the egress proxy's log, which holds one JSON audit
// record for each decision the proxy makes. It sums the records up by the
// demo's endpoints, finds the record of a request, and writes a record or a
// structured log line as a row a person can read. It draws nothing.
package proxylog

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

const (
	// ScopeDNS is the scope of a record of one name lookup.
	ScopeDNS = "dns"
	// LevelWidth is the width of a log line's level, such as ERROR, in the
	// row ReadableLine writes.
	LevelWidth = 5
	// verdictDeny is the proxy's verdict on a request it refused.
	verdictDeny = "deny"
	// reasonUpstreamFailed is the reason on an allowed request or lookup
	// that the server or the cluster's resolver did not answer.
	reasonUpstreamFailed = "upstream_failed"
	// maxRecords is how many records a Store keeps.
	maxRecords = 5000
	// hostWidth is the width of the destination column.
	hostWidth = 36
)

// Record is the part of an egress-proxy audit record the TUI shows.
type Record struct {
	TS          time.Time `json:"ts"`
	Scope       string    `json:"scope"`
	Destination struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"destination"`
	Request  *Request `json:"request"`
	Query    *Query   `json:"query"`
	Decision struct {
		Verdict  string `json:"verdict"`
		Reason   string `json:"reason"`
		Endpoint string `json:"endpoint"`
		Error    string `json:"error"`
	} `json:"decision"`
	Outcome *Outcome `json:"outcome"`
}

// Request is a request's line.
type Request struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Query is a DNS question, and the answer's code.
type Query struct {
	Type  string `json:"type"`
	Rcode string `json:"rcode"`
}

// Outcome is what came of an allowed request.
type Outcome struct {
	Status int    `json:"status"`
	Error  string `json:"error"`
}

// Parse reads one line of the proxy's log, and reports whether it is an
// audit record.
func Parse(line []byte) (Record, bool) {
	var record Record
	if json.Unmarshal(line, &record) != nil || record.Decision.Verdict == "" {
		return Record{}, false
	}
	return record, true
}

// Denied reports whether the proxy refused the record's request.
func (r Record) Denied() bool { return r.Decision.Verdict == verdictDeny }

// RefusedCredential reports whether Keycard did not give a credential for
// the request, as opposed to a request no endpoint allows.
func (r Record) RefusedCredential() bool {
	switch r.Decision.Reason {
	case "exchange_denied", "exchange_failed", "credential_unavailable":
		return true
	}
	return false
}

// RoutineLookup reports whether a record is a DNS lookup that says nothing
// about the agent: one the proxy allowed, or one of the cluster's own names
// it refused.
func (r Record) RoutineLookup() bool {
	return r.Scope == ScopeDNS && (!r.Denied() || clusterName(r.Destination.Host))
}

// Row turns a record into one row: time, verdict, what was asked for, and
// what came of it.
func (r Record) Row() string {
	verdict := "✓ allow"
	if r.Denied() {
		verdict = "✕ deny "
	}
	what, result := r.Scope, ""
	// A looked-up name ends in a dot, which says nothing to a reader.
	target := strings.TrimSuffix(r.Destination.Host, ".")
	switch {
	case r.Request != nil:
		what, target = r.Request.Method, r.Destination.Host+r.Request.Path
	case r.Query != nil:
		what, result = "DNS "+r.Query.Type, r.Query.Rcode
	case r.Scope == "connection":
		what, target = "CONNECT", r.Destination.Host+":"+strconv.Itoa(r.Destination.Port)
	}
	if r.Outcome != nil {
		result = strconv.Itoa(r.Outcome.Status)
		if r.Outcome.Error != "" {
			result += " " + r.Outcome.Error
		}
	}
	// An allowed request names the endpoint that let it through, and a
	// denied one why, in words.
	why := ReasonText(r.Decision.Reason)
	if !r.Denied() {
		why = r.Decision.Endpoint
	}
	if r.Decision.Error != "" {
		why += ": " + r.Decision.Error
	}
	if len(target) > hostWidth {
		target = target[:hostWidth-1] + "…"
	}
	return strings.TrimRight(fmt.Sprintf("%s  %s  %-8s %-*s %-9s %s",
		Clock(&r), verdict, what, hostWidth, target, result, why), " ")
}

// Clock is the time of a record, in the operator's zone.
func Clock(r *Record) string {
	return r.TS.Local().Format(time.TimeOnly) //nolint:gosmopolitan // The operator reads times in their own zone.
}

// ReasonText says why the proxy refused, in words.
func ReasonText(reason string) string {
	texts := map[string]string{
		"no_matching_endpoint":    "not on the sandbox's allow list",
		"deny_always":             "on the platform's deny list",
		"host_mismatch":           "the request names two different hosts",
		"cleartext_not_forwarded": "plain HTTP is never forwarded",
		"exchange_denied":         "Keycard's policy does not allow it",
		"exchange_failed":         "the exchange with Keycard did not complete",
		"credential_unavailable":  "the proxy has no source for the credential",
		"upstream_failed":         "the server did not answer",
	}
	return cmp.Or(texts[reason], reason)
}

// ReadableLine turns a JSON log line into one a person can read. An audit
// record becomes its Row, and a structured log line, such as slog writes,
// becomes its time, level, message and fields. Any other line stays as it
// is.
func ReadableLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return line
	}
	if record, ok := Parse([]byte(trimmed)); ok {
		return record.Row()
	}
	var fields map[string]any
	if json.Unmarshal([]byte(trimmed), &fields) != nil {
		return line
	}
	message, ok := fields["msg"].(string)
	if !ok {
		return line
	}
	when := "--:--:--"
	if stamp, isText := fields["time"].(string); isText {
		if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			when = parsed.Local().Format(time.TimeOnly) //nolint:gosmopolitan // The operator reads times in their own zone.
		}
	}
	level, _ := fields["level"].(string)
	for _, known := range []string{"msg", "time", "level"} {
		delete(fields, known)
	}
	parts := make([]string, 0, len(fields))
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		parts = append(parts, name+"="+fieldText(fields[name]))
	}
	return strings.TrimRight(fmt.Sprintf("%s  %-*s %s  %s", when, LevelWidth, level, message,
		strings.Join(parts, " ")), " ")
}

// Store keeps the proxy's records for one pod. A follower adds them and the
// TUI reads them, so it locks.
type Store struct {
	mu      sync.Mutex
	records []Record
	pod     string
}

// Add keeps a record, and drops the oldest past maxRecords.
func (s *Store) Add(record Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	if over := len(s.records) - maxRecords; over > 0 {
		s.records = append([]Record(nil), s.records[over:]...)
	}
}

// Reset drops every record, and keeps the records of pod that come after.
func (s *Store) Reset(pod string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records, s.pod = nil, pod
}

// Snapshot returns the records and the pod they are for.
func (s *Store) Snapshot() ([]Record, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...), s.pod
}

// Counts are the DNS lookups the proxy answered, and how many of them it
// refused.
type Counts struct {
	Lookups, LookupsDenied int
}

// Count counts the DNS lookups in records.
func Count(records []Record) Counts {
	var counts Counts
	for _, record := range records {
		if record.Scope != ScopeDNS {
			continue
		}
		counts.Lookups++
		if record.Denied() {
			counts.LookupsDenied++
		}
	}
	return counts
}

// EndpointUse is what the proxy did for one endpoint: how many requests it
// let through, how many Keycard refused a credential for, and the latest.
type EndpointUse struct {
	Endpoint         sandbox.ScenarioEndpoint
	Allowed, Refused int
	Last             *Record
}

// BlockedHost is a host the agent asked for that no endpoint allows.
type BlockedHost struct {
	Host, Reason string
}

// Summarize sorts the requests by the endpoint that decided them. Hosts the
// proxy refused go in blocked, newest first: a request it refused, or a
// name it would not look up, which is how a host off the allow list is
// refused. Lookups of the cluster's own names are left out: the resolver
// tries them first for every name. A request or lookup the server did not
// answer is not blocked, and is left out too.
func Summarize(records []Record, endpoints []sandbox.ScenarioEndpoint) ([]EndpointUse, []BlockedHost) {
	uses := make([]EndpointUse, len(endpoints))
	index := map[string]int{}
	for i, endpoint := range endpoints {
		uses[i].Endpoint = endpoint
		index[endpoint.ID()] = i
	}
	var blocked blockedHosts
	for i := range records {
		record := &records[i]
		slot, matched := index[record.Decision.Endpoint]
		switch {
		case record.RoutineLookup():
		case matched && record.RefusedCredential():
			uses[slot].Refused++
			uses[slot].Last = record
		case matched && !record.Denied():
			uses[slot].Allowed++
			uses[slot].Last = record
		case record.Denied() && record.Decision.Reason != reasonUpstreamFailed:
			blocked.add(record)
		}
	}
	return uses, blocked.newestFirst()
}

// RecordFor returns the newest record for host since since: a request or a
// connection, or else a refused lookup of the name.
func RecordFor(records []Record, host string, since time.Time) (Record, bool) {
	var lookup *Record
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		if record.TS.Before(since) {
			break
		}
		if strings.TrimSuffix(record.Destination.Host, ".") != host {
			continue
		}
		if record.Scope != ScopeDNS {
			return record, true
		}
		if lookup == nil && record.Denied() {
			lookup = &records[i]
		}
	}
	if lookup != nil {
		return *lookup, true
	}
	return Record{}, false
}

// blockedHosts collects the hosts the proxy refused, each once, in the
// order of their latest refusal.
type blockedHosts struct {
	byHost map[string]*BlockedHost
	order  []string
}

func (b *blockedHosts) add(record *Record) {
	host := strings.TrimSuffix(record.Destination.Host, ".")
	if b.byHost == nil {
		b.byHost = map[string]*BlockedHost{}
	}
	if b.byHost[host] == nil {
		b.byHost[host] = &BlockedHost{Host: host}
	} else {
		b.order = slices.DeleteFunc(b.order, func(seen string) bool { return seen == host })
	}
	b.order = append(b.order, host)
	b.byHost[host].Reason = record.Decision.Reason
}

func (b *blockedHosts) newestFirst() []BlockedHost {
	hosts := make([]BlockedHost, 0, len(b.order))
	for _, host := range slices.Backward(b.order) {
		hosts = append(hosts, *b.byHost[host])
	}
	return hosts
}

// clusterName reports whether a looked-up name is the cluster's own, such
// as a search domain the resolver tried, rather than a host on the
// internet. The .local and .internal domains are reserved for private
// names, and a cloud's nodes add one of them, such as ec2.internal on EKS.
func clusterName(host string) bool {
	host = strings.TrimSuffix(host, ".")
	return !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal")
}

// fieldText prints a log field's value: text as it is, quoted when it has
// spaces, and anything else as JSON.
func fieldText(value any) string {
	if text, ok := value.(string); ok {
		if strings.ContainsAny(text, " \t") {
			return strconv.Quote(text)
		}
		return text
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(raw)
}
