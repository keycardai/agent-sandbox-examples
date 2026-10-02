package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The slugs of the API servers the agent has a call for.
const (
	SlugGitHub = "github"
	SlugLinear = "linear"
)

var (
	errReadPath    = errors.New("--read must be OWNER/REPO/PATH")
	errLinearQuery = errors.New("linear refused the query")
	errCurlOutput  = errors.New("curl wrote no status")
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
		case SlugGitHub:
			p, err := githubProbe(r.APIURL, read)
			if err != nil {
				return nil, err
			}
			probes = append(probes, p)
		case SlugLinear:
			probes = append(probes, linearProbe(r.APIURL))
		}
	}
	return probes, nil
}

// githubProbe reads a file, given as OWNER/REPO/PATH, through the contents
// API.
func githubProbe(apiURL, path string) (probe, error) {
	owner, rest, _ := strings.Cut(path, "/")
	repo, file, _ := strings.Cut(rest, "/")
	if owner == "" || repo == "" || file == "" {
		return probe{}, fmt.Errorf("%w (got %q)", errReadPath, path)
	}
	target := strings.TrimSuffix(apiURL, "/") + "/repos/" + owner + "/" + repo + "/contents/" + file
	return probe{
		Resource: SlugGitHub,
		API:      "GitHub",
		Title:    "GitHub: read " + path,
		Args:     []string{"-H", "Accept: application/vnd.github.raw+json", target},
		Show:     func(body string) (string, error) { return body, nil },
	}, nil
}

// linearQuery asks Linear who the credential belongs to, which every key can
// read.
const linearQuery = `{"query":"{ viewer { name email } organization { name } }"}`

// linearAnswer is the part of Linear's GraphQL response the probe reads.
type linearAnswer struct {
	Data struct {
		Viewer struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"viewer"`
		Organization struct {
			Name string `json:"name"`
		} `json:"organization"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// linearProbe asks Linear's GraphQL API who the agent is acting as.
func linearProbe(apiURL string) probe {
	return probe{
		Resource: SlugLinear,
		API:      "Linear",
		Title:    "Linear: who am I",
		Args: []string{
			"-X", "POST", "-H", "Content-Type: application/json", "--data", linearQuery,
			strings.TrimSuffix(apiURL, "/") + "/graphql",
		},
		Show: func(body string) (string, error) {
			var answer linearAnswer
			if err := json.Unmarshal([]byte(body), &answer); err != nil {
				return "", fmt.Errorf("the answer is not JSON: %w", err)
			}
			if len(answer.Errors) > 0 {
				return "", fmt.Errorf("%w: %s", errLinearQuery, answer.Errors[0].Message)
			}
			viewer, org := answer.Data.Viewer, answer.Data.Organization
			return fmt.Sprintf("%s <%s> in the %s workspace", viewer.Name, viewer.Email, org.Name), nil
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
	const (
		marker     = "\n__sandbox_status__:"
		tlsRetries = 10
		retryDelay = 2 * time.Second
	)
	command := append([]string{"curl", "-sS", "-w", marker + "%{http_code}"}, args...)
	var out string
	var err error
	for range tlsRetries {
		out, err = a.sandboxKube().Exec(ctx, a.State.Namespace, pod, "agent", command...)
		if !curlTLSFailure(err) {
			break
		}
		select {
		case <-ctx.Done():
			return Fetch{}, fmt.Errorf("wait to retry curl: %w", ctx.Err())
		case <-time.After(retryDelay):
		}
	}
	if err != nil {
		return Fetch{}, err
	}
	i := strings.LastIndex(out, marker)
	if i < 0 {
		return Fetch{}, fmt.Errorf("%w: %q", errCurlOutput, out)
	}
	status, err := strconv.Atoi(strings.TrimSpace(out[i+len(marker):]))
	if err != nil {
		return Fetch{}, fmt.Errorf("%w: %q", errCurlOutput, out[i:])
	}
	return Fetch{Status: status, Body: out[:i]}, nil
}

// curlTLSFailure reports whether curl exited because the TLS handshake failed
// (35) or the server's certificate was not trusted (60).
func curlTLSFailure(err error) bool {
	const (
		curlSSLConnect   = 35
		curlPeerFailedCA = 60
	)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	code := exitErr.ExitCode()
	return code == curlSSLConnect || code == curlPeerFailedCA
}
