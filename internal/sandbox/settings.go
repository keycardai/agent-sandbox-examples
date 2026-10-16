// Package sandbox sets up the sandbox demo against a Keycard zone, on a local
// k3d cluster or an existing one. Each step is one subcommand of cmd/sandbox.
package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
)

// The settings. The config file holds them, and an environment variable
// of the same name overrides each. docs/local.md documents each one.
const (
	KeyCluster          = "KEYCARD_LOCAL_CLUSTER"
	KeyIssuer           = "KEYCARD_LOCAL_ISSUER"
	KeyTunnel           = "KEYCARD_LOCAL_TUNNEL"
	KeyZoneURL          = "KEYCARD_LOCAL_ZONE_URL"
	KeyZoneOnHost       = "KEYCARD_LOCAL_ZONE_ON_HOST"
	KeyZoneSetupValues  = "KEYCARD_LOCAL_ZONE_SETUP_VALUES"
	KeyUserEmail        = "KEYCARD_LOCAL_USER_EMAIL"
	KeyGitHubLogin      = "KEYCARD_LOCAL_GITHUB_LOGIN"
	KeyCAFile           = "KEYCARD_LOCAL_CA_FILE"
	KeyKubeContext      = "KEYCARD_LOCAL_KUBE_CONTEXT"
	KeyImageTag         = "KEYCARD_LOCAL_IMAGE_TAG"
	KeyClusterCIDRs     = "KEYCARD_LOCAL_CLUSTER_CIDRS"
	KeyScenario         = "KEYCARD_LOCAL_SCENARIO"
	maxClusterNameBytes = 32
	// settingTrue is the value of a setting that is on.
	settingTrue = "true"
	pemMarker   = "-----BEGIN CERTIFICATE-----"
	// dirMode and fileMode are the modes of what the steps write under
	// local/out.
	dirMode  = 0o750
	fileMode = 0o600
)

// Errors the steps wrap.
var (
	// ErrSettings is a settings file that is missing or has a line that is
	// not KEY=value for a known key.
	ErrSettings = errors.New("settings refused")
	// ErrCluster is a missing cluster name, or one k3d would refuse or read
	// as an option.
	ErrCluster = errors.New("cluster name refused")
	// ErrIssuer is a missing issuer, or one a step cannot use.
	ErrIssuer = errors.New("issuer refused")
	// ErrCAFile is a KEYCARD_LOCAL_CA_FILE that is not an absolute path to a
	// readable PEM file.
	ErrCAFile = errors.New("ca file refused")
	// ErrTunnel is a tunnel setting that cloudflared cannot run, a quick
	// tunnel's issuer file that exists or cannot be written, or a cloudflared
	// that stopped.
	ErrTunnel = errors.New("tunnel refused")
	// ErrExisting is a k3d-only step run with settings that name an existing
	// cluster.
	ErrExisting = errors.New("not for an existing cluster")
)

// clusterName is k3d's RFC 1123 label, narrowed to lowercase.
var clusterName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func settingKeys() []string {
	return []string{
		KeyCluster, KeyIssuer, KeyTunnel, KeyZoneURL, KeyZoneOnHost,
		KeyZoneSetupValues, KeyUserEmail, KeyGitHubLogin, KeyCAFile,
		KeyKubeContext, KeyImageTag, KeyClusterCIDRs, KeyScenario,
	}
}

// Settings are the config's settings, with the environment applied.
type Settings map[string]string

// Local is a checkout of this repository and the settings its steps read.
type Local struct {
	// Root is the repository root. The steps write under Root/local/out.
	Root string
	// Settings are the config's settings, with the environment applied.
	Settings Settings
	// Store keeps what setup asked, for this session and, when the operator
	// saves it, in the config file and the keychain. Copies share it.
	Store *Store
	// Tool returns a command that runs a tool mise.toml pins.
	Tool func(ctx context.Context, name string, args ...string) (*exec.Cmd, error)
	// Out receives progress lines and the output of long-running tools.
	Out io.Writer
	// Stdin is the input of the command Shell runs in the agent container.
	// If it is nil, the command reads no input.
	Stdin io.Reader
	// Ask asks the operator questions. When stdin is not a terminal, Ask is
	// nil and every question returns ErrNoTerminal.
	Ask Asker
	// Getenv reads the environment for Init. If it is nil, Init does not
	// read the environment.
	Getenv func(string) string
}

// UseConfig sets the settings to the store's config, with the environment
// over them.
func (l *Local) UseConfig() {
	getenv := l.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	l.Settings = SettingsFrom(l.config(), getenv)
}

// QuickIssuerFile is where the tunnel step writes a quick tunnel's URL while
// it runs. The other steps read the issuer from it when KEYCARD_LOCAL_ISSUER
// is empty.
func (l *Local) QuickIssuerFile() string {
	return filepath.Join(l.Root, "local", "out", "issuer")
}

// Existing reports whether KEYCARD_LOCAL_KUBE_CONTEXT is set. When it is,
// the steps use that existing cluster instead of a k3d cluster.
func (l *Local) Existing() bool {
	return l.Settings[KeyKubeContext] != ""
}

// StartsTunnel reports whether the bring-up starts a tunnel for the
// cluster's issuer: on a local cluster, unless KEYCARD_LOCAL_ISSUER names an
// issuer the operator publishes without one. KEYCARD_LOCAL_TUNNEL always
// starts one.
func (l *Local) StartsTunnel() bool {
	return !l.Existing() && (l.Settings[KeyTunnel] != "" || l.Settings[KeyIssuer] == "")
}

// cluster returns flag, or the setting when flag is empty. For an existing
// cluster, the name is a kubeconfig context from KEYCARD_LOCAL_KUBE_CONTEXT.
// Otherwise it is a k3d cluster from KEYCARD_LOCAL_CLUSTER: at most 32
// lowercase letters, digits and dashes, as k3d requires.
func (l *Local) cluster(flag string) (string, error) {
	if l.Existing() {
		return cmp.Or(flag, l.Settings[KeyKubeContext]), nil
	}
	name := cmp.Or(flag, l.Settings[KeyCluster])
	if name == "" {
		return "", fmt.Errorf("%w: no cluster name. Set %s or pass --cluster", ErrCluster, KeyCluster)
	}
	if len(name) > maxClusterNameBytes || !clusterName.MatchString(name) {
		return "", fmt.Errorf("%w: the name must be at most 32 lowercase letters, digits and dashes (got %q)",
			ErrCluster, name)
	}
	return name, nil
}

// issuer returns flag, else KEYCARD_LOCAL_ISSUER, else the URL in
// QuickIssuerFile. It refuses the file when no running tunnel has it locked,
// because the tunnel behind that URL is not running.
func (l *Local) issuer(flag string) (string, error) {
	if url := cmp.Or(flag, l.Settings[KeyIssuer]); url != "" {
		return url, nil
	}
	url, running, err := readQuickIssuer(l.QuickIssuerFile())
	switch {
	case err == nil && !running:
		return "", fmt.Errorf("%w: the quick tunnel that wrote %s is not running. "+
			"Run `go run ./cmd/sandbox tunnel` in another terminal", ErrIssuer, l.QuickIssuerFile())
	case err == nil && url != "":
		return url, nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%w: %w", ErrIssuer, err)
	}
	return "", fmt.Errorf("%w: no issuer. Set %s, pass --issuer, "+
		"or run `go run ./cmd/sandbox tunnel` in another terminal", ErrIssuer, KeyIssuer)
}

// caFile returns KEYCARD_LOCAL_CA_FILE, or "" when it is empty. The path
// must be absolute, because tools that run in another directory read it.
func (l *Local) caFile() (string, error) {
	file := l.Settings[KeyCAFile]
	if file == "" {
		return "", nil
	}
	if !filepath.IsAbs(file) {
		return "", fmt.Errorf("%w: %s must be an absolute path (got %q)", ErrCAFile, KeyCAFile, file)
	}
	raw, err := os.ReadFile(file) //nolint:gosec // G304: the path is the CA file the operator named.
	if err != nil {
		return "", fmt.Errorf("%w: %s %s is not a readable file", ErrCAFile, KeyCAFile, file)
	}
	if text := string(raw); !strings.HasPrefix(text, pemMarker) && !strings.Contains(text, "\n"+pemMarker) {
		return "", fmt.Errorf("%w: %s %s does not hold a PEM certificate", ErrCAFile, KeyCAFile, file)
	}
	return file, nil
}

// kubeContext returns the kubeconfig context of cluster. For an existing
// cluster it is cluster. For a k3d cluster it is k3d-<cluster>.
func (l *Local) kubeContext(cluster string) string {
	if l.Existing() {
		return cluster
	}
	return "k3d-" + cluster
}

// k3dName returns Local.cluster(flag), or ErrExisting when the settings name
// an existing cluster.
func (l *Local) k3dName(step, flag string) (string, error) {
	if err := l.requireK3d(step); err != nil {
		return "", err
	}
	return l.cluster(flag)
}

// requireK3d returns ErrExisting, naming step, when the settings name an
// existing cluster.
func (l *Local) requireK3d(step string) error {
	if l.Existing() {
		return fmt.Errorf("%w: %s is for a k3d cluster that sandbox creates, and %s names an existing cluster",
			ErrExisting, step, KeyKubeContext)
	}
	return nil
}

// kubectl runs kubectl in the cluster's context with stdin and returns its
// standard output. It sets KUBERC=off, so a kuberc default for --server,
// --cluster, --user or --token cannot override the context.
func (l *Local) kubectl(ctx context.Context, cluster string, stdin []byte, args ...string) ([]byte, error) {
	cmd, err := l.kubectlCommand(ctx, cluster, args...)
	if err != nil {
		return nil, err
	}
	out, err := pinned.Run(cmd, stdin)
	if err != nil {
		return nil, fmt.Errorf("kubectl: %w", err)
	}
	return out, nil
}

// kubectlCommand returns a kubectl command in the cluster's context, with
// KUBERC=off.
func (l *Local) kubectlCommand(ctx context.Context, cluster string, args ...string) (*exec.Cmd, error) {
	cmd, err := l.Tool(ctx, "kubectl", slices.Concat([]string{"--context", l.kubeContext(cluster)}, args)...)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Environ(), "KUBERC=off")
	return cmd, nil
}
