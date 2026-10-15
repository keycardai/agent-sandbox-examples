package sandbox_test

import (
	"strings"
	"testing"
	"time"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// TestPreflightImages names the release the charts pin, and fails when
// Docker cannot reach ghcr.io. With KEYCARD_LOCAL_IMAGE_TAG set, it fails
// when Docker lacks one of the images at that tag.
func TestPreflightImages(t *testing.T) {
	t.Parallel()
	const (
		noAccess = `case "$*" in "manifest inspect "*) exit 1 ;; esac` + "\n"
		noProxy  = `case "$*" in "image inspect ghcr.io/keycardai/egress-proxy:local") exit 1 ;; esac` + "\n"
	)
	for _, tc := range []struct {
		name, tag, tool string
		state           sandbox.CheckState
		detail, fix     string
	}{
		{name: "pinned release", detail: "the release v0.2.1 from ghcr.io/keycardai"},
		{
			name: "ghcr.io out of reach", tool: noAccess,
			state: sandbox.CheckFailed, detail: "cannot reach ghcr.io/keycardai", fix: "Docker cannot reach ghcr.io",
		},
		{name: "images at a tag", tag: devTag, detail: "the images Docker has at local"},
		{
			name: "image missing at a tag", tag: devTag, tool: noProxy,
			state: sandbox.CheckFailed, detail: "Docker does not have ghcr.io/keycardai/egress-proxy:local",
			fix: "Build them in the egress-proxy repository",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, _ := newLocal(t, sandbox.Settings{sandbox.KeyImageTag: tc.tag}, tc.tool)
			linkCharts(t, local.Root)
			var check sandbox.Check
			for _, item := range local.NewSession("127.0.0.1:8480", time.Second).PreflightItems() {
				if item.Name == "Sandbox images" {
					check = item.Run(t.Context())
				}
			}
			if check.State != tc.state || !strings.HasPrefix(check.Detail, tc.detail) {
				t.Errorf("check = %+v, want state %v and %q", check, tc.state, tc.detail)
			}
			if !strings.Contains(check.Fix, tc.fix) {
				t.Errorf("fix = %q, want it to contain %q", check.Fix, tc.fix)
			}
		})
	}
}
