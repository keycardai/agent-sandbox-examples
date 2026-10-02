package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// A probe is one call the agent makes to an API through the proxy. The agent
// holds no credential; the proxy writes the one the token exchange returns.
type probe struct {
	// Resource is the slug of the API server the call goes to, and API its
	// name.
	Resource string
	API      string
	Title    string
	// Args are curl's arguments after the ones every call shares.
	Args []string
	// Show turns a 200 response into what the run prints, and fails when the
	// body says the call did not work.
	Show func(body string) (string, error)
}

// probesFor lists the calls the agent makes, one for each installed API
// server it has a call for.
func probesFor(resources []Resource, read string) ([]probe, error) {
	var probes []probe
	for _, r := range resources {
		switch r.Slug {
		case "github":
			p, err := githubProbe(r.APIURL, read)
			if err != nil {
				return nil, err
			}
			probes = append(probes, p)
		case "linear":
			probes = append(probes, linearProbe(r.APIURL))
		}
	}
	return probes, nil
}

// githubProbe reads a file, given as OWNER/REPO/PATH, through the contents
// API.
func githubProbe(apiURL, path string) (probe, error) {
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return probe{}, fmt.Errorf("--read must be OWNER/REPO/PATH (got %q)", path)
	}
	target := strings.TrimSuffix(apiURL, "/") + "/repos/" + parts[0] + "/" + parts[1] + "/contents/" + parts[2]
	return probe{
		Resource: "github",
		API:      "GitHub",
		Title:    "GitHub: read " + path,
		Args:     []string{"-H", "Accept: application/vnd.github.raw+json", target},
		Show:     func(body string) (string, error) { return body, nil },
	}, nil
}

// linearQuery asks Linear who the credential belongs to, which every key can
// read.
const linearQuery = `{"query":"{ viewer { name email } organization { name } }"}`

// linearProbe asks Linear's GraphQL API who the agent is acting as.
func linearProbe(apiURL string) probe {
	return probe{
		Resource: "linear",
		API:      "Linear",
		Title:    "Linear: who am I",
		Args: []string{
			"-X", "POST", "-H", "Content-Type: application/json", "--data", linearQuery,
			strings.TrimSuffix(apiURL, "/") + "/graphql",
		},
		Show: func(body string) (string, error) {
			var resp struct {
				Data struct {
					Viewer       struct{ Name, Email string }
					Organization struct{ Name string }
				}
				Errors []struct{ Message string }
			}
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				return "", fmt.Errorf("the answer is not JSON: %w", err)
			}
			if len(resp.Errors) > 0 {
				return "", errors.New("the query failed: " + resp.Errors[0].Message)
			}
			d := resp.Data
			return fmt.Sprintf("%s <%s> in the %s workspace", d.Viewer.Name, d.Viewer.Email, d.Organization.Name), nil
		},
	}
}

// Fetch is the agent's request and what came back.
type Fetch struct {
	Status int
	Body   string
}

// fetch runs curl with args in the pod's agent container. A pod is ready once
// its agent container starts, but sandbox-base's entrypoint is still
// installing the proxy's CA then, so curl refuses the proxy's certificate for
// the first few seconds. fetch retries TLS failures until the CA is in place.
func (a *App) fetch(ctx context.Context, pod string, args []string) (Fetch, error) {
	const marker = "\n__sandbox_status__:"
	command := append([]string{"curl", "-sS", "-w", marker + "%{http_code}"}, args...)
	var out string
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		out, err = a.sandboxKube().Exec(ctx, a.State.Namespace, pod, "agent", command...)
		if err == nil ||
			!strings.Contains(err.Error(), "exit status 35") && !strings.Contains(err.Error(), "exit status 60") {
			break
		}
		select {
		case <-ctx.Done():
			return Fetch{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if err != nil {
		return Fetch{}, err
	}
	i := strings.LastIndex(out, marker)
	if i < 0 {
		return Fetch{}, fmt.Errorf("unexpected curl output: %q", out)
	}
	status, _ := strconv.Atoi(strings.TrimSpace(out[i+len(marker):]))
	return Fetch{Status: status, Body: out[:i]}, nil
}
