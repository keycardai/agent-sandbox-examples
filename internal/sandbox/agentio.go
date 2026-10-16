package sandbox

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/install"
	"github.com/keycardai/agent-sandbox-examples/internal/zonesetup"
)

const (
	// agentRequestTimeout bounds one request the agent sends for the TUI.
	agentRequestTimeout = "20"
	// placeholderAuth is the Authorization header the agent sends. The proxy
	// replaces it with the user's credential on an endpoint that has one.
	placeholderAuth = "Authorization: Bearer placeholder"
	// httpsPort is the port of an endpoint whose URL names none.
	httpsPort = "443"
)

// ErrConsole is a console link that cannot be made, such as for a
// management API whose host does not start with api.
var ErrConsole = errors.New("console link")

// ScenarioEndpoint is one endpoint the agent may call: its URL, and the
// resource whose credential the proxy adds, if any.
type ScenarioEndpoint struct {
	URL string
	// Credential is set when the proxy adds a credential to requests here,
	// and Name names its resource, such as GitHub.
	Credential bool
	Name       string
}

// ID is the endpoint as the proxy names it on an audit record: host, port
// and path prefix.
func (e ScenarioEndpoint) ID() string {
	parsed, err := url.Parse(e.URL)
	if err != nil {
		return e.URL
	}
	port := cmp.Or(parsed.Port(), httpsPort)
	return parsed.Hostname() + ":" + port + cmp.Or(parsed.EscapedPath(), "/")
}

// Host is the endpoint's host.
func (e ScenarioEndpoint) Host() string {
	parsed, err := url.Parse(e.URL)
	if err != nil {
		return e.URL
	}
	return parsed.Hostname()
}

// EndpointList returns the scenario's endpoints, each with the name of the
// resource whose credential it gets.
func (s *Scenario) EndpointList() []ScenarioEndpoint {
	names := map[string]string{}
	for _, resource := range s.Resources {
		names[strings.TrimRight(resource.Identifier, "/")] = resource.Name
	}
	endpoints := make([]ScenarioEndpoint, 0, len(s.Endpoints))
	for _, item := range s.Endpoints {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		address, _ := fields["url"].(string)
		if address == "" {
			continue
		}
		_, credential := fields["credential"]
		endpoints = append(endpoints, ScenarioEndpoint{
			URL: address, Credential: credential, Name: names[strings.TrimRight(address, "/")],
		})
	}
	return endpoints
}

// EndpointFor returns the endpoint a request to address goes to: the one
// with the longest URL that address starts with.
func EndpointFor(endpoints []ScenarioEndpoint, address string) (ScenarioEndpoint, bool) {
	var found ScenarioEndpoint
	for _, endpoint := range endpoints {
		prefix := strings.TrimRight(endpoint.URL, "/")
		if (address == prefix || strings.HasPrefix(address, prefix+"/")) && len(prefix) > len(found.URL) {
			found = ScenarioEndpoint{URL: prefix, Credential: endpoint.Credential, Name: endpoint.Name}
		}
	}
	return found, found.URL != ""
}

// AgentResponse is what a request the agent sent got back.
type AgentResponse struct {
	Status int
	Body   string
	Took   time.Duration
	// Failure is curl's message when curl could not complete the request,
	// as when the proxy refused the host's name.
	Failure string
}

// curlMessage starts each message curl prints when it cannot complete a
// request, such as "curl: (6) Could not resolve host".
const curlMessage = "curl: ("

// AgentCurl returns the command AgentRequest runs in the agent's container:
// curl, with the placeholder credential, and a body as JSON. curl writes
// the status after statusMarker. The format spells the marker's newline as
// \n, which curl turns back into one, so that the command prints on one
// line.
func AgentCurl(method, address, body string) []string {
	curl := []string{
		"curl", "-sS", "-m", agentRequestTimeout, "-X", method, "-H", placeholderAuth,
		"-w", strings.ReplaceAll(statusMarker, "\n", `\n`) + "%{http_code}",
	}
	if body != "" {
		curl = append(curl, "-H", "Content-Type: application/json", "--data", body)
	}
	return append(curl, address)
}

// ShellWords quotes each of args for a POSIX shell, so that the words
// joined with spaces can be pasted as the command.
func ShellWords(args []string) []string {
	words := make([]string, len(args))
	for i, arg := range args {
		words[i] = install.ShellQuote(arg)
	}
	return words
}

// AgentRequest has the agent send one HTTPS request, with a placeholder
// credential, through the proxy as every request of the agent goes. A body
// goes as JSON. A request curl could not complete is not an error: its
// response says why in Failure. A request that did not reach curl, as when
// the pod is gone, is an ErrAgent error.
func (s *Session) AgentRequest(ctx context.Context, method, address, body string) (AgentResponse, error) {
	target, err := s.Target()
	if err != nil {
		return AgentResponse{}, err
	}
	args := []string{namespaceFlag, target.Namespace, execVerb, target.Pod, "-c", agentContainer, "--"}
	cmd, err := s.Local().kubectlCommand(ctx, target.Cluster, append(args, AgentCurl(method, address, body)...)...)
	if err != nil {
		return AgentResponse{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	started := time.Now()
	out, err := cmd.Output()
	response := AgentResponse{Took: time.Since(started)}
	if err != nil {
		if ctx.Err() != nil {
			return response, fmt.Errorf("%w: %w", ErrAgent, ctx.Err())
		}
		i := strings.Index(stderr.String(), curlMessage)
		if i < 0 {
			return response, fmt.Errorf("%w: sending the request from the agent's container: %w: %s",
				ErrAgent, err, strings.TrimSpace(stderr.String()))
		}
		line, _, _ := strings.Cut(stderr.String()[i:], "\n")
		response.Failure = strings.TrimSpace(line)
		return response, nil
	}
	text, status, _ := strings.Cut(string(out), statusMarker)
	response.Body = text
	response.Status, _ = strconv.Atoi(strings.TrimSpace(status))
	return response, nil
}

// ZoneEventsURL returns the console page of the zone's event log, where the
// zone records each credential it issues. It signs in to find the zone's
// organization.
func (s *Session) ZoneEventsURL(ctx context.Context) (string, error) {
	local := s.Local()
	values := local.previousValues().values
	consoleURL, err := consoleBase(values.ManagementAPIURL)
	if err != nil {
		return "", err
	}
	clientID, secret, err := local.accountSecret(values.ManagementServiceAccount)
	if err != nil {
		return "", fmt.Errorf("signing in to find the zone's organization: %w", err)
	}
	roots, err := rootsFrom(local.Settings[KeyCAFile])
	if err != nil {
		return "", err
	}
	org, err := zonesetup.Login(ctx, values.ManagementAPIURL, clientID, secret, roots)
	if err != nil {
		return "", fmt.Errorf("signing in to find the zone's organization: %w", err)
	}
	zones, err := org.Zones(ctx)
	if err != nil {
		return "", fmt.Errorf("listing the zones: %w", err)
	}
	for _, zone := range zones {
		if zone.ID == values.ZoneID {
			return consoleURL + "/orgs/" + zone.OrganizationID + "/zones/" + zone.ID + "/audit", nil
		}
	}
	return "", fmt.Errorf("%w: the service account does not see the zone %s", ErrConsole, values.ZoneID)
}

// consoleBase turns the management API's URL into the console's: the
// console's host is the API's with console in place of api.
func consoleBase(apiURL string) (string, error) {
	parsed, err := url.Parse(apiURL)
	if err != nil || !strings.HasPrefix(parsed.Host, "api.") {
		return "", fmt.Errorf("%w: cannot tell the console from the management API %q", ErrConsole, apiURL)
	}
	return "https://console." + strings.TrimPrefix(parsed.Host, "api."), nil
}
