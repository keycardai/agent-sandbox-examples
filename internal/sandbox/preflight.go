package sandbox

import (
	"cmp"
	"context"
	"strings"

	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
)

// CheckState is how one preflight check came out.
type CheckState int

// The preflight check states.
const (
	// CheckOK is a check that passed.
	CheckOK CheckState = iota
	// CheckNote is something setup asks for, or does without.
	CheckNote
	// CheckFailed stops the bring-up until it is fixed.
	CheckFailed
)

// Check is one thing the machine needs before the bring-up, and how it is.
type Check struct {
	Name  string
	State CheckState
	// Detail says what was found, and Fix what to do when the check failed.
	Detail, Fix string
}

// PreflightItem is one preflight check, for a caller that runs them one at
// a time and shows each as it runs.
type PreflightItem struct {
	Name string
	Run  func(ctx context.Context) Check
}

// PreflightItems returns what the bring-up needs on this machine, in the
// order to check it: Docker, the three images, the tools mise.toml pins,
// and the Keycard service account. The checks only read.
func (s *Session) PreflightItems() []PreflightItem {
	local := s.Local()
	return []PreflightItem{
		{Name: "Docker", Run: local.preflightDocker},
		{Name: "Sandbox images", Run: local.checkImages},
		{Name: "Tools", Run: local.checkTools},
		{Name: "Keycard account", Run: func(context.Context) Check { return local.checkAccount() }},
	}
}

// preflightDocker checks that Docker runs. An existing cluster runs the
// sandbox and pulls its images, so it does not need Docker.
func (l *Local) preflightDocker(ctx context.Context) Check {
	check := Check{Name: "Docker"}
	if l.Existing() {
		check.State, check.Detail = CheckNote, "not needed for an existing cluster"
		return check
	}
	out, err := l.runTool(ctx, "docker", "info", "--format", "{{.ServerVersion}}|{{.OperatingSystem}}")
	if err != nil {
		check.State, check.Detail = CheckFailed, firstLine(err)
		check.Fix = "Install Docker, and start OrbStack or Docker Desktop."
		return check
	}
	version, system, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
	check.Detail = system + ", engine " + version
	return check
}

// checkTools runs each tool the steps need, through mise. An existing
// cluster needs only kubectl and helm: there is no k3d cluster to create
// and no tunnel to start.
func (l *Local) checkTools(ctx context.Context) Check {
	check := Check{Name: "Tools"}
	const version = "version"
	tools := [][]string{{"kubectl", version, "--client"}, {"helm", version}}
	if !l.Existing() {
		tools = [][]string{
			{"k3d", version}, {"kubectl", version, "--client"}, {"helm", version}, {"cloudflared", "--" + version},
		}
	}
	names := make([]string, 0, len(tools))
	var broken []string
	var first error
	for _, tool := range tools {
		names = append(names, tool[0])
		if _, err := l.runTool(ctx, tool[0], tool[1:]...); err != nil {
			broken = append(broken, tool[0])
			first = cmp.Or(first, err)
		}
	}
	if len(broken) == 0 {
		check.Detail = strings.Join(names, ", ") + " (from mise.toml)"
		return check
	}
	check.State, check.Detail = CheckFailed, "cannot run "+strings.Join(broken, ", ")+": "+firstLine(first)
	check.Fix = "Run mise install in this repository."
	return check
}

// runTool runs the tool name with args and returns its standard output.
// The error names the tool and holds what it wrote to standard error.
func (l *Local) runTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd, err := l.Tool(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	return pinned.Run(cmd, nil) //nolint:wrapcheck // pinned.Run names the tool and its arguments.
}

// firstLine returns the first line of err's text, for a check's one-line
// detail.
func firstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

// checkAccount says where the Keycard service account comes from: the
// environment, the saved settings, or setup's questions.
func (l *Local) checkAccount() Check {
	check := Check{Name: "Keycard account"}
	secret, key := l.envSecret()
	saved := l.previousValues().values.ManagementServiceAccount
	switch {
	case l.getenv(EnvClientID) != "" && secret != "":
		check.Detail = "from " + EnvClientID + " and " + key
	case saved.ClientID != "" && cmp.Or(secret, l.keptSecret(saved.ClientSecretEnv)) != "":
		check.Detail = "saved in the settings"
	default:
		check.State = CheckNote
		check.Detail = "setup asks for the service account's client ID and secret"
		check.Fix = "To skip the questions, set " + EnvClientID + " and " + EnvClientSecret + "."
	}
	return check
}
