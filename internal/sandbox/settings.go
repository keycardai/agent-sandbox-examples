// Package sandbox sets up the sandbox demo against a Keycard zone, on a local
// k3d cluster or an existing one. Each step is one subcommand of cmd/sandbox.
package sandbox

import (
	"bufio"
	"bytes"
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

// The settings local.env can hold. local/local.env.example documents each one.
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

// Settings are the values of local.env, with the environment applied.
type Settings map[string]string

// LoadSettings reads the settings file at path, or root/local/local.env when
// path is empty. It parses the file as data in the format local.env.example
// describes, and does not run it. A missing default file gives empty
// settings. A missing named file is an error. A non-empty value from getenv
// overrides the file.
func LoadSettings(root, path string, getenv func(string) string) (Settings, error) {
	settings := Settings{}
	file := cmp.Or(path, filepath.Join(root, "local", "local.env"))
	raw, err := os.ReadFile(file) //nolint:gosec // G304: the path is the settings file the operator named.
	switch {
	case errors.Is(err, fs.ErrNotExist) && path == "":
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrSettings, err)
	default:
		if err = settings.parse(file, raw); err != nil {
			return nil, err
		}
	}
	for _, key := range settingKeys() {
		if value := getenv(key); value != "" {
			settings[key] = value
		}
	}
	return settings, nil
}

func (s Settings) parse(file string, raw []byte) error {
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for lineNo := 1; lines.Scan(); lineNo++ {
		line := lines.Text()
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !slices.Contains(settingKeys(), key) {
			return fmt.Errorf("%w: %s line %d is not KEY=value for a key local.env.example lists", ErrSettings, file, lineNo)
		}
		s[key] = cmp.Or(s[key], value)
	}
	if err := lines.Err(); err != nil {
		return fmt.Errorf("%w: reading %s: %w", ErrSettings, file, err)
	}
	return nil
}

// Local is a checkout of this repository and the settings its steps read.
type Local struct {
	// Root is the repository root. The steps write under Root/local/out.
	Root string
	// Settings are the values of local.env, with the environment applied.
	Settings Settings
	// Tool returns a command that runs a tool mise.toml pins.
	Tool func(ctx context.Context, name string, args ...string) (*exec.Cmd, error)
	// Out receives progress lines and the output of long-running tools.
	Out io.Writer
	// Getenv reads the environment for Init. If it is nil, Init does not
	// read the environment.
	Getenv func(string) string
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
// QuickIssuerFile.
func (l *Local) issuer(flag string) (string, error) {
	if url := cmp.Or(flag, l.Settings[KeyIssuer]); url != "" {
		return url, nil
	}
	raw, err := os.ReadFile(l.QuickIssuerFile())
	if url := strings.TrimSpace(string(raw)); err == nil && url != "" {
		return url, nil
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
