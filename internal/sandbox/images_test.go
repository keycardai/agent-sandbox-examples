package sandbox_test

import (
	"strings"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// TestPreflightImages names the release the charts pin, and fails with
// Docker's error when Docker cannot read it from ghcr.io. With
// KEYCARD_LOCAL_IMAGE_TAG set, it fails when Docker lacks one of the images
// at that tag, or when Docker does not answer.
func TestPreflightImages(t *testing.T) {
	t.Parallel()
	const (
		noAccess = `case "$*" in "manifest inspect "*) echo denied >&2; exit 1 ;; esac` + "\n"
		noProxy  = `case "$*" in "image inspect ghcr.io/keycardai/egress-proxy:local") exit 1 ;; esac` + "\n"
		noDocker = `case "$*" in version*|"image inspect "*) echo "Cannot connect" >&2; exit 1 ;; esac` + "\n"
	)
	for _, tc := range []struct {
		name, tag, tool string
		state           sandbox.CheckState
		detail, fix     string
	}{
		{name: "pinned release", detail: "the release v0.2.1 from ghcr.io/keycardai"},
		{
			name: "ghcr.io refuses", tool: noAccess,
			state: sandbox.CheckFailed, detail: "exit status 1: denied", fix: "Fix what Docker says",
		},
		{name: "images at a tag", tag: devTag, detail: "the images Docker has at local"},
		{
			name: "image missing at a tag", tag: devTag, tool: noProxy,
			state: sandbox.CheckFailed, detail: "Docker does not have ghcr.io/keycardai/egress-proxy:local",
			fix: "Build them in the egress-proxy repository",
		},
		{
			name: "Docker down at a tag", tag: devTag, tool: noDocker,
			state: sandbox.CheckFailed, detail: "exit status 1: Cannot connect",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, _ := newLocal(t, sandbox.Settings{sandbox.KeyImageTag: tc.tag}, tc.tool)
			linkCharts(t, local.Root)
			check := preflight(t, local, "Sandbox images")
			if check.State != tc.state || !strings.Contains(check.Detail, tc.detail) {
				t.Errorf("check = %+v, want state %v and %q", check, tc.state, tc.detail)
			}
			if !strings.Contains(check.Fix, tc.fix) {
				t.Errorf("fix = %q, want it to contain %q", check.Fix, tc.fix)
			}
		})
	}
}

// The names of the preflight checks the tests run.
const checkDocker, checkTools = "Docker", "Tools"

// TestPreflightTools fails with the tool's error when Docker or a tool
// mise.toml pins does not run.
func TestPreflightTools(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{}, `case "$FAKE_TOOL $*" in "docker info"*|"helm version") `+
		`echo "permission denied" >&2; exit 1 ;; esac`+"\n")
	for name, want := range map[string]string{
		checkDocker: "exit status 1: permission denied",
		checkTools:  "cannot run helm: ",
	} {
		check := preflight(t, local, name)
		if check.State != sandbox.CheckFailed || !strings.Contains(check.Detail, want) {
			t.Errorf("%s check = %+v, want it failed with %q", name, check, want)
		}
	}
}

// TestPreflightExistingCluster needs neither Docker, nor k3d and
// cloudflared, for an existing cluster. The fake fails each of them, so
// running one would fail its check.
func TestPreflightExistingCluster(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, sandbox.Settings{sandbox.KeyKubeContext: eksContext},
		`case "$FAKE_TOOL" in docker|k3d|cloudflared) echo "not installed" >&2; exit 1 ;; esac`+"\n")
	for name, want := range map[string]sandbox.Check{
		checkDocker: {Name: checkDocker, State: sandbox.CheckNote, Detail: "not needed for an existing cluster"},
		checkTools:  {Name: checkTools, State: sandbox.CheckOK, Detail: "kubectl, helm (from mise.toml)"},
	} {
		if check := preflight(t, local, name); check != want {
			t.Errorf("%s check = %+v, want %+v", name, check, want)
		}
	}
}

// preflight runs the preflight check called name.
func preflight(t *testing.T, local *sandbox.Local, name string) sandbox.Check {
	t.Helper()
	for _, item := range local.NewSession("127.0.0.1:8480", time.Second).PreflightItems() {
		if item.Name == name {
			return item.Run(t.Context())
		}
	}
	t.Fatalf("no preflight check called %s", name)
	return sandbox.Check{}
}
