// Package sandbox sets up the sandbox demo against a Keycard zone, on a local
// k3d cluster. Each step is one subcommand of cmd/sandbox.
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
	maxClusterNameBytes = 32
	pemMarker           = "-----BEGIN CERTIFICATE-----"
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
)

// clusterName is k3d's RFC 1123 label, narrowed to lowercase.
var clusterName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func settingKeys() []string {
	return []string{
		KeyCluster, KeyIssuer, KeyTunnel, KeyZoneURL, KeyZoneOnHost,
		KeyZoneSetupValues, KeyUserEmail, KeyGitHubLogin, KeyCAFile,
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
}

// QuickIssuerFile is where the tunnel step writes a quick tunnel's URL while
// it runs. The other steps read the issuer from it when KEYCARD_LOCAL_ISSUER
// is empty.
func (l *Local) QuickIssuerFile() string {
	return filepath.Join(l.Root, "local", "out", "issuer")
}

// cluster returns flag, or KEYCARD_LOCAL_CLUSTER when flag is empty. The
// name must be lowercase letters, digits and dashes, at most 32 bytes, as
// k3d requires.
func (l *Local) cluster(flag string) (string, error) {
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

// kubectl runs kubectl in the cluster's k3d context with stdin and returns
// its standard output. It sets KUBERC=off, so a kuberc default for --server,
// --cluster, --user or --token cannot override the context.
func (l *Local) kubectl(ctx context.Context, cluster string, stdin []byte, args ...string) ([]byte, error) {
	cmd, err := l.Tool(ctx, "kubectl", slices.Concat([]string{"--context", "k3d-" + cluster}, args)...)
	if err != nil {
		return nil, err
	}
	cmd.Env = append(cmd.Environ(), "KUBERC=off")
	out, err := pinned.Run(cmd, stdin)
	if err != nil {
		return nil, fmt.Errorf("kubectl: %w", err)
	}
	return out, nil
}
