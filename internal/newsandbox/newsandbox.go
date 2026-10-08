// Package newsandbox renders the sandbox chart for one user and applies the
// result with kubectl.
package newsandbox

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"sigs.k8s.io/yaml"

	"github.com/keycardai/agent-sandbox-poc/internal/chart"
	"github.com/keycardai/agent-sandbox-poc/internal/zonesetup"
)

// The error classes a run can end in.
var (
	// ErrInputs means an input is missing or invalid.
	ErrInputs = errors.New("inputs")
	// ErrKubectl means a kubectl command failed.
	ErrKubectl = errors.New("kubectl")
)

// dnsLabelRE matches an RFC 1123 DNS label. The namespace and the user's
// public id go unquoted into object names and metadata. Validate refuses
// either one unless it matches.
var dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	// maxLabelLength is the length limit of a DNS label.
	maxLabelLength = 63
	// valuesMode lets only the owner read the generated values file.
	valuesMode = 0o600
)

// Inputs holds the zone-setup outputs for one sandbox. The names match the
// names zone-setup prints and the keys the sandbox chart reads. Each field
// has a new-sandbox flag.
type Inputs struct {
	// Namespace is the sandbox namespace. It goes to Helm as the release
	// namespace and is not written to the values file.
	Namespace             string `json:"-"`
	ProviderID            string `json:"providerID,omitempty"`
	ApplicationID         string `json:"applicationID,omitempty"`
	ApplicationIdentifier string `json:"applicationIdentifier,omitempty"`
	UserEmail             string `json:"userEmail,omitempty"`
	UserID                string `json:"userID,omitempty"`
	ServiceAccount        string `json:"serviceAccount,omitempty"`
	ClientID              string `json:"clientID,omitempty"`
	// ZoneURL is the zone that decides every token exchange. When it is
	// empty, the value in the admin's values file applies.
	ZoneURL string `json:"zoneURL,omitempty"`
}

// Validate returns an error for the first invalid input. ServiceAccount must
// be system:serviceaccount:<Namespace>:<UserID>, the subject the credential
// is bound to. A sandbox with any other subject would fail every exchange
// with access_denied.
func (in Inputs) Validate() error {
	for _, required := range []struct{ flag, value string }{
		{"namespace", in.Namespace},
		{"user-id", in.UserID},
		{"client-id", in.ClientID},
		{"service-account", in.ServiceAccount},
	} {
		if required.value == "" {
			return fmt.Errorf("%w: --%s is required", ErrInputs, required.flag)
		}
	}
	for _, label := range []struct{ flag, value string }{
		{"namespace", in.Namespace},
		{"user-id", in.UserID},
	} {
		if !dnsLabelRE.MatchString(label.value) || len(label.value) > maxLabelLength {
			return fmt.Errorf("%w: --%s %q is not a DNS label", ErrInputs, label.flag, label.value)
		}
	}
	if subject := zonesetup.ServiceAccountSubject(in.Namespace, in.UserID); in.ServiceAccount != subject {
		return fmt.Errorf("%w: --service-account is %q, but this sandbox binds %q",
			ErrInputs, in.ServiceAccount, subject)
	}
	return nil
}

// Runner runs a command with the standard input and arguments given and
// returns its standard output.
type Runner func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)

// Command is one new-sandbox run.
type Command struct {
	Inputs Inputs
	// ChartDir is the sandbox chart.
	ChartDir string
	// Name is the name of the Sandbox and the Helm release, and the
	// sandbox.id on every audit record. It defaults to the user's public id.
	Name string
	// ValuesFiles are the admin's values files: the endpoints, the cluster
	// ranges and the zone URL.
	ValuesFiles []string
	// Apply sends the rendered objects to the cluster with kubectl apply.
	// When Apply is false, Run only renders.
	Apply bool
	// Kube runs kubectl.
	Kube Runner
	// Log gets progress messages. Run returns the manifests.
	Log io.Writer
}

// Run renders the sandbox and, when Apply is set, applies it. It returns
// the manifest stream either way.
func (c *Command) Run(ctx context.Context) ([]byte, error) {
	if err := c.Inputs.Validate(); err != nil {
		return nil, err
	}
	valuesFile, cleanup, err := c.writeValues()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	name := c.release()
	files := append(append([]string{}, c.ValuesFiles...), valuesFile)
	docs, err := chart.Render(ctx, c.ChartDir, name, c.Inputs.Namespace, files...)
	if err != nil {
		return nil, fmt.Errorf("rendering the sandbox chart: %w", err)
	}
	manifests := bytes.Join(docs, []byte("---\n"))
	if !c.Apply {
		c.sayf("rendered sandbox %s for %s and applied nothing", name, c.Inputs.UserID)
		return manifests, nil
	}
	out, err := c.Kube(ctx, manifests, "apply", "--namespace", c.Inputs.Namespace, "-f", "-")
	if err != nil {
		return nil, fmt.Errorf("%w: applying the sandbox: %w", ErrKubectl, err)
	}
	c.sayf("applied sandbox %s for %s\n%s", name, c.Inputs.UserID, out)
	return manifests, nil
}

// release is the Sandbox name and the Helm release name.
func (c *Command) release() string {
	return cmp.Or(c.Name, c.Inputs.UserID)
}

func (c *Command) sayf(format string, args ...any) {
	if c.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(c.Log, "new-sandbox: "+format+"\n", args...)
}

// writeValues writes the identity values to a temporary file and returns
// its path and the function that removes it. Run passes it to Helm after the
// admin's files. A value from a flag therefore overrides the same key in an
// admin's file.
func (c *Command) writeValues() (string, func(), error) {
	doc, err := yaml.Marshal(c.Inputs)
	if err != nil {
		return "", nil, fmt.Errorf("encoding the chart values: %w", err)
	}
	dir, err := os.MkdirTemp("", "new-sandbox-")
	if err != nil {
		return "", nil, fmt.Errorf("creating a temporary directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "values.yaml")
	if err = os.WriteFile(path, doc, valuesMode); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return path, cleanup, nil
}
