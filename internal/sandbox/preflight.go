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

func (l *Local) preflightDocker(ctx context.Context) Check {
	check := Check{Name: "Docker"}
	cmd, err := l.Tool(ctx, "docker", "info", "--format", "{{.ServerVersion}}|{{.OperatingSystem}}")
	if err == nil {
		var out []byte
		if out, err = pinned.Run(cmd, nil); err == nil {
			version, system, _ := strings.Cut(strings.TrimSpace(string(out)), "|")
			check.Detail = system + ", engine " + version
			return check
		}
	}
	check.State, check.Detail = CheckFailed, "not running"
	check.Fix = "Install Docker, and start OrbStack or Docker Desktop."
	return check
}

// checkTools runs each tool the steps need, through mise.
func (l *Local) checkTools(ctx context.Context) Check {
	check := Check{Name: "Tools"}
	var broken []string
	const version = "version"
	for _, tool := range [][]string{
		{"k3d", version}, {"kubectl", version, "--client"}, {"helm", version}, {"cloudflared", "--" + version},
	} {
		cmd, err := l.Tool(ctx, tool[0], tool[1:]...)
		if err == nil {
			_, err = pinned.Run(cmd, nil)
		}
		if err != nil {
			broken = append(broken, tool[0])
		}
	}
	if len(broken) == 0 {
		check.Detail = "k3d, kubectl, helm, cloudflared (from mise.toml)"
		return check
	}
	check.State, check.Detail = CheckFailed, "cannot run "+strings.Join(broken, ", ")
	check.Fix = "Run mise install in this repository."
	return check
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
