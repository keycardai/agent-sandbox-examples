package sandbox_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

const (
	eksContext = "arn:aws:eks:us-west-2:111122223333:cluster/demo"
	eksIssuer  = "https://oidc.eks.us-west-2.amazonaws.com/id/EXAMPLE"
	eksRanges  = "10.0.0.0/16,172.20.0.0/16"
)

// existing returns the settings for a sandbox on the existing EKS cluster.
func existing() sandbox.Settings {
	return sandbox.Settings{
		sandbox.KeyKubeContext: eksContext, sandbox.KeyClusterCIDRs: eksRanges,
		sandbox.KeyZoneURL: zoneURL, sandbox.KeyUserEmail: email,
	}
}

// eksTool is a fake kubectl for the existing cluster: its kubeconfig has
// the k3d context and the EKS one, and its discovery document names
// eksIssuer. Other commands go to fake.
func eksTool(fake fakeSandbox) string {
	return `case "$FAKE_TOOL $*" in
"kubectl config get-contexts --output=name") printf 'k3d-keycard-sandbox-local\n` + eksContext + `\n'; exit 0 ;;
*"get --raw /.well-known/openid-configuration")
	echo '{"issuer":"` + eksIssuer + `","jwks_uri":"` + eksIssuer + `/keys"}'; exit 0 ;;
esac
` + fake.script()
}

// TestExistingRefuses checks that the steps that create a k3d cluster or
// publish its issuer fail for an existing cluster before any tool runs.
func TestExistingRefuses(t *testing.T) {
	t.Parallel()
	steps := map[string]func(*sandbox.Local) error{
		"bootstrap": func(l *sandbox.Local) error { return l.Bootstrap(t.Context(), "", "", false) },
		"down":      func(l *sandbox.Local) error { return l.Down(t.Context(), "", true, nil) },
		"tunnel":    func(l *sandbox.Local) error { return l.Tunnel(t.Context(), listen) },
		"mirror":    func(l *sandbox.Local) error { return l.Mirror(t.Context(), "", "", listen) },
	}
	for name, step := range steps {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, existing(), "")
			checkErr(t, step(local), []error{sandbox.ErrExisting}, name+" is for a k3d cluster")
			if got := readCalls(t, calls); got != "" {
				t.Fatalf("calls = %q, want none", got)
			}
		})
	}
}

// TestRunExistingSettings checks that Run refuses settings it cannot use
// for an existing cluster. Run reads the cluster's issuer before it checks
// them, so each case expects that one call.
func TestRunExistingSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, key, value string
		want             error
		wantText         string
	}{
		{
			"zone on this machine",
			sandbox.KeyZoneOnHost,
			"true",
			sandbox.ErrExisting,
			"KEYCARD_LOCAL_ZONE_ON_HOST is for a k3d cluster",
		},
		{"no ranges", sandbox.KeyClusterCIDRs, "", sandbox.ErrSettings, "KEYCARD_LOCAL_CLUSTER_CIDRS is empty"},
		{
			"a range without a prefix",
			sandbox.KeyClusterCIDRs,
			"10.0.0.0",
			sandbox.ErrSettings,
			`"10.0.0.0" is not an address range`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings := existing()
			settings[tc.key] = tc.value
			local, calls := newLocal(t, settings, eksTool(healthy()))
			checkErr(t, local.Run(t.Context(), sandbox.RunOptions{}), []error{tc.want}, tc.wantText)
			want := "kubectl KUBERC=off --context " + eksContext + " get --raw /.well-known/openid-configuration\n"
			if got := readCalls(t, calls); got != want {
				t.Fatalf("calls = %q, want %q", got, want)
			}
		})
	}
}

// TestRunExisting runs the whole flow on an existing cluster. Run does not
// create a cluster or import images. install.sh and kubectl use the
// cluster's context, and both values files point at the ghcr.io images and
// the cluster's ranges.
func TestRunExisting(t *testing.T) {
	t.Parallel()
	local, calls := newLocal(t, existing(), eksTool(healthy()))
	linkCharts(t, local.Root)
	writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), zoneSetupOutput)
	if err := local.Run(t.Context(), sandbox.RunOptions{SkipZoneSetup: true}); err != nil {
		t.Fatal(err)
	}
	got := readCalls(t, calls)
	if strings.Contains(got, "k3d ") || strings.Contains(got, "docker ") {
		t.Errorf("calls = %q, want no k3d or docker", got)
	}
	if want := "install/install.sh KUBERC= --context " + eksContext + " --namespace agents "; !strings.Contains(
		got,
		want,
	) {
		t.Errorf("calls lack %q\n%s", want, got)
	}
	for _, name := range []string{"install.values.yaml", "sandbox.values.yaml"} {
		path := filepath.Join(local.Root, "local", "out", name)
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the test's own file.
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"proxy: ghcr.io/keycardai/egress-proxy:0.1.0", "- 10.0.0.0/16", "- 172.20.0.0/16"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s lacks %q\n%s", name, want, raw)
			}
		}
	}
}

// TestUpExistingPrint prints only check-issuer and run for an existing
// cluster.
func TestUpExistingPrint(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t, existing(), "")
	out := &bytes.Buffer{}
	local.Out = out
	if err := local.Up(t.Context(), sandbox.UpOptions{Listen: listen, Print: true}); err != nil {
		t.Fatal(err)
	}
	if want := "  go run ./cmd/sandbox check-issuer\n  go run ./cmd/sandbox run\n"; out.String() != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

// TestInitExisting chooses the EKS context. Init reads its issuer, asks for
// the image tag and the ranges, and does not ask how to publish the issuer.
// Init refuses a zone on this machine, because the cluster cannot dial it.
func TestInitExisting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, zoneURL string
		want          []error
		wantText      string
	}{
		{name: "zone on the internet", zoneURL: "https://192.0.2.10"},
		{
			name: "zone on this machine", zoneURL: "https://localhost",
			want: []error{sandbox.ErrExisting}, wantText: "an existing cluster cannot connect to it",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server, caFile := fakeManagement(t, tc.zoneURL, gitHubProviders, "")
			local := initLocal(t, eksTool(healthy()))
			answers := signInAnswers(server.URL, caFile, goodSecret)
			answers["Which cluster runs the sandbox?"] = []string{eksContext}
			answers["The cluster's pod and service ranges"] = []string{eksRanges}
			ask := &scriptedAsker{t: t, answers: answers}

			_, err := local.Init(t.Context(), ask)
			checkErr(t, err, tc.want, tc.wantText)
			if tc.want != nil {
				return
			}
			for _, question := range ask.asked {
				if strings.HasPrefix(question, "How does the zone reach") {
					t.Errorf("asked %q, want no issuer question for an existing cluster", question)
				}
			}
			settings, err := sandbox.LoadSettings(local.Root, "", func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]string{
				sandbox.KeyKubeContext: eksContext, sandbox.KeyImageTag: "0.1.0",
				sandbox.KeyClusterCIDRs: eksRanges, sandbox.KeyZoneOnHost: "", sandbox.KeyIssuer: "",
			} {
				if settings[key] != want {
					t.Errorf("local.env %s = %q, want %q", key, settings[key], want)
				}
			}
		})
	}
}
