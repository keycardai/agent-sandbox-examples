package sandbox_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

const (
	userID    = "user1abc"
	namespace = "agents"
	checkOK   = "every automated check passed"
	oneFailed = "1 of 5"
	// coreDNS is the address and liveness probe port of the CoreDNS pod the
	// NetworkPolicy probe connects to.
	coreDNS = "10.42.0.5:8080"
	// The fake proxy log replaces @NOW@ with the current time.
	deniedLine = `{"ts":"@NOW@","destination":{"host":"example.com","port":53},` +
		`"decision":{"verdict":"deny","reason":"no_matching_endpoint"}}`
	refusedLine = `{"ts":"@NOW@","destination":{"host":"api.github.com","port":443},` +
		`"decision":{"verdict":"deny","reason":"exchange_denied","error":"access_denied"}}`
	openRouterRefusedLine = `{"ts":"@NOW@","destination":{"host":"openrouter.ai","port":443},` +
		`"decision":{"verdict":"deny","reason":"exchange_denied","error":"access_denied"}}`
	upstreamLine = `{"ts":"@NOW@","destination":{"host":"example.com","port":53},` +
		`"decision":{"verdict":"deny","reason":"upstream_failed"}}`
	noDenyRecord = "  FAIL  denied: https://example.com/ was blocked, " +
		"but the proxy did not log no_matching_endpoint for it\n"
)

// stale returns line stamped before any check runs.
func stale(line string) string {
	return strings.ReplaceAll(line, "@NOW@", "2020-01-01T00:00:00Z")
}

// devTag is the tag of images built in the egress-proxy repository.
const devTag = "local"

// pinnedImages returns the images the charts pin.
func pinnedImages() []string {
	return []string{
		"ghcr.io/keycardai/egress-netsetup:v0.2.1@sha256:cff8c7a98ebfadfb514ccc0840cfb7a67994711f7e7c405d309613e4611b5b33",
		"ghcr.io/keycardai/egress-proxy:v0.2.1@sha256:79684cf9b059d8e2f1719b108a05251bb47ef3bebb39759fd1811ec36b9ff7d2",
		"ghcr.io/keycardai/sandbox-base:v0.2.1@sha256:555ef77f07de9e186e594aa3b0aa4805cfe7149ace065ca97d40fbc0e02c1cce",
	}
}

// fakeSandbox is what the fake tools answer for a running sandbox.
type fakeSandbox struct {
	allowed      string // the status pypi returns
	githubStatus string
	githubBody   string
	// openRouterStatus and openRouterBody are what OpenRouter's key
	// endpoint returns.
	openRouterStatus string
	openRouterBody   string
	deniedExit       int    // curl's exit code for example.com
	logs             string // the proxy's log
	serviceAccount   string
	// tlsFailOnce makes the first pypi fetch exit 60, as curl does before
	// the agent trusts the proxy.
	tlsFailOnce bool
	// missingImage is an image docker image inspect does not find.
	missingImage string
	// coreDNS is what kubectl prints for the CoreDNS pod's IP and liveness
	// probe port.
	coreDNS string
	// probeExit is curl's exit code in the NetworkPolicy probe pod.
	probeExit int
}

// healthy returns a sandbox that passes every check.
func healthy() fakeSandbox {
	return fakeSandbox{
		allowed: "200", githubStatus: "200", githubBody: `{"login":"octocat"}`, deniedExit: 6,
		openRouterStatus: "200", openRouterBody: `{"data":{"label":"sk-or-v1-abc...xyz"}}`,
		logs: deniedLine, serviceAccount: userID, coreDNS: "10.42.0.5 8080", probeExit: 7,
	}
}

// script returns a fake docker, k3d, kubectl and helm for f. k3d lists the
// cluster, the cluster's issuer and tokens name testIssuer, and the sandbox
// namespace enforces privileged.
func (f fakeSandbox) script() string {
	tlsFail := ""
	if f.tlsFailOnce {
		tlsFail = `[[ -f "$CALLS.tls" ]] || { touch "$CALLS.tls"; exit 60; }`
	}
	return clusterTool(present, testIssuer, testIssuer) + fmt.Sprintf(`
case "$FAKE_TOOL $*" in
"docker image inspect %[1]s") exit 1 ;;
*"exec %[2]s -c agent -- curl"*"https://pypi.org/simple/") %[3]s
	printf '\n__sandbox_status__:%[4]s' ;;
*"exec %[2]s -c agent -- sh -c"*"openrouter.ai"*) printf '%%s\n__sandbox_status__:%%s' '%[10]s' '%[11]s' ;;
*"exec %[2]s -c agent -- sh -c"*) printf '%%s\n__sandbox_status__:%%s' '%[5]s' '%[6]s' ;;
*"exec %[2]s -c agent -- curl"*"https://example.com/")
	[[ %[7]d == 0 ]] && printf '\n__sandbox_status__:200'
	exit %[7]d ;;
*"logs %[2]s -c egress-proxy"*) printf '%%s\n' '%[8]s' | sed "s/@NOW@/$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)/g" ;;
*"get pod %[2]s -o jsonpath={.spec.serviceAccountName}") printf '%%s' '%[9]s' ;;
*"get namespace %[12]s -o jsonpath="*) printf privileged ;;
*"--namespace kube-system get pods --selector k8s-app=kube-dns "*) printf '%[13]s' ;;
*"get pod %[2]s -o jsonpath={.spec.containers[?(@.name==\"agent\")].image}") printf sandbox-base ;;
*"--namespace %[12]s run keycard-netpol-probe --image sandbox-base "*" probe %[15]s")
	printf '__probe_exit__:%[14]d\n' ;;
*" apply "*) echo "applied" ;;
esac
`, f.missingImage, userID, tlsFail, f.allowed, f.githubBody, f.githubStatus, f.deniedExit, f.logs, f.serviceAccount,
		f.openRouterBody, f.openRouterStatus, namespace, f.coreDNS, f.probeExit, coreDNS)
}

// TestCheck runs the checks against a fake sandbox. A failing check prints
// the value it got and counts toward ErrCheck. The OpenRouter check runs only
// when the zone-setup values file has an OpenRouter resource.
func TestCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sandbox    fakeSandbox
		login      string
		openRouter bool
		want       []error
		wantText   string
		wantOut    []string
		notOut     []string
	}{
		{
			name:    "every check passes",
			sandbox: healthy(),
			login:   "octocat",
			wantOut: []string{
				"  PASS  allowed, no credential: https://pypi.org/simple/ returned 200\n",
				`  PASS  credentialed: https://api.github.com/user named the GitHub account "octocat"` + "\n",
				"  PASS  denied: https://example.com/ was blocked: no_matching_endpoint\n",
				"  PASS  the pod runs as the ServiceAccount " + userID + "\n",
				"  PASS  NetworkPolicy: a pod in " + namespace + " could not connect to CoreDNS at " + coreDNS +
					" (curl exit 7)\n",
				"MANUAL  Open the zone's event log.",
				checkOK,
			},
			notOut: []string{"openrouter"},
		},
		{
			name:       "OpenRouter key",
			sandbox:    healthy(),
			openRouter: true,
			wantOut: []string{
				`  PASS  credentialed: https://openrouter.ai/api/v1/key returned the API key "sk-or-v1-abc...xyz"` + "\n",
				checkOK,
			},
		},
		{
			name: "OpenRouter key refused",
			sandbox: with(func(f *fakeSandbox) {
				// GitHub's refusal comes last, so the check must match the host.
				f.openRouterStatus, f.openRouterBody, f.logs = "502", "",
					deniedLine+"\n"+openRouterRefusedLine+"\n"+refusedLine
			}),
			openRouter: true,
			want:       []error{sandbox.ErrCheck},
			wantText:   "1 of 6",
			wantOut: []string{
				"  FAIL  credentialed: https://openrouter.ai/api/v1/key returned 502 (proxy: exchange_denied: access_denied)\n",
			},
		},
		{
			name:    "agent does not trust the proxy yet",
			sandbox: with(func(f *fakeSandbox) { f.tlsFailOnce = true }),
			wantOut: []string{"  PASS  allowed, no credential: https://pypi.org/simple/ returned 200\n", checkOK},
		},
		{
			name:     "allowed destination refused",
			sandbox:  with(func(f *fakeSandbox) { f.allowed = "403" }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{"  FAIL  allowed, no credential: https://pypi.org/simple/ returned 403, want 200\n"},
		},
		{
			name: "exchange refused",
			sandbox: with(func(f *fakeSandbox) {
				f.githubStatus, f.githubBody, f.logs = "502", "", deniedLine+"\n"+refusedLine
			}),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut: []string{
				"  FAIL  credentialed: https://api.github.com/user returned 502 (proxy: exchange_denied: access_denied)\n",
			},
		},
		{
			name: "exchange refused, with only another host's record and a stale one",
			sandbox: with(func(f *fakeSandbox) {
				f.githubStatus, f.githubBody, f.logs = "502", "", stale(refusedLine)+"\n"+deniedLine
			}),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{"  FAIL  credentialed: https://api.github.com/user returned 502\n"},
		},
		{
			name:     "another GitHub account",
			sandbox:  healthy(),
			login:    "someone",
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{`  FAIL  credentialed: https://api.github.com/user named "octocat", want "someone"` + "\n"},
		},
		{
			name:     "denied destination reachable",
			sandbox:  with(func(f *fakeSandbox) { f.deniedExit = 0 }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{"  FAIL  denied: https://example.com/ was reachable\n"},
		},
		{
			name:     "denied destination fails after name resolution",
			sandbox:  with(func(f *fakeSandbox) { f.deniedExit = 7 }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{"  FAIL  denied: https://example.com/ did not fail at name resolution: "},
		},
		{
			name:     "no audit record",
			sandbox:  with(func(f *fakeSandbox) { f.logs = refusedLine }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{noDenyRecord},
		},
		{
			name:     "denied because upstream DNS failed",
			sandbox:  with(func(f *fakeSandbox) { f.logs = upstreamLine }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{noDenyRecord},
		},
		{
			name:     "stale audit record",
			sandbox:  with(func(f *fakeSandbox) { f.logs = stale(deniedLine) }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{noDenyRecord},
		},
		{
			name:     "another ServiceAccount",
			sandbox:  with(func(f *fakeSandbox) { f.serviceAccount = "default" }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{`  FAIL  the pod runs as the ServiceAccount "default", want "` + userID + `"` + "\n"},
		},
		{
			name:    "policy engine drops the probe",
			sandbox: with(func(f *fakeSandbox) { f.probeExit = 28 }),
			wantOut: []string{"  PASS  NetworkPolicy: a pod in " + namespace + " could not connect to CoreDNS at " +
				coreDNS + " (curl exit 28)\n", checkOK},
		},
		{
			name:     "NetworkPolicy not enforced",
			sandbox:  with(func(f *fakeSandbox) { f.probeExit = 0 }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut: []string{"  FAIL  NetworkPolicy: a pod in " + namespace + " connected to CoreDNS at " + coreDNS +
				". The cluster does not enforce NetworkPolicy, so only netsetup and the proxy restrict " +
				"the sandbox's egress\n"},
		},
		{
			name:     "probe has no curl",
			sandbox:  with(func(f *fakeSandbox) { f.probeExit = 127 }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{"  FAIL  NetworkPolicy: the probe's curl to " + coreDNS + " exited 127, want 7 or 28\n"},
		},
		{
			name:     "CoreDNS liveness probe port is named",
			sandbox:  with(func(f *fakeSandbox) { f.coreDNS = "10.42.0.5 health" }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut: []string{"  FAIL  NetworkPolicy: want the IP and the numeric liveness probe " +
				`port of a running CoreDNS pod in kube-system, got "10.42.0.5 health"` + "\n"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, _ := newLocal(t, sandbox.Settings{sandbox.KeyGitHubLogin: tc.login}, tc.sandbox.script())
			if tc.openRouter {
				writeFile(t, filepath.Join(local.Root, "zone-setup.yaml"),
					"resources:\n  - identifier: https://openrouter.ai/api/v1\n    secret_file: /k\n")
			}
			out := &bytes.Buffer{}
			local.Out = out
			err := local.Check(t.Context(), sandbox.CheckTarget{
				Cluster: cluster, Namespace: namespace, Pod: userID, UserID: userID,
			})
			checkErr(t, err, tc.want, tc.wantText)
			for _, want := range tc.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q\n%s", want, out)
				}
			}
			for _, absent := range tc.notOut {
				if strings.Contains(out.String(), absent) {
					t.Errorf("output has %q\n%s", absent, out)
				}
			}
		})
	}
}

// TestRunSettings checks that Run refuses each bad setting before it runs
// any tool.
func TestRunSettings(t *testing.T) {
	t.Parallel()
	valid := func() sandbox.Settings {
		return sandbox.Settings{
			sandbox.KeyCluster: cluster, sandbox.KeyIssuer: testIssuer,
			sandbox.KeyZoneURL: zoneURL, sandbox.KeyUserEmail: email,
		}
	}
	tests := []struct {
		name, key, value, wantText string
	}{
		{
			"zone URL over http",
			sandbox.KeyZoneURL,
			"http://zone.example.com",
			"KEYCARD_LOCAL_ZONE_URL must be https://<zone host>",
		},
		{
			"zone URL with a path",
			sandbox.KeyZoneURL,
			"https://zone.example.com/z",
			"KEYCARD_LOCAL_ZONE_URL must be https://<zone host>",
		},
		{"no user", sandbox.KeyUserEmail, "", "KEYCARD_LOCAL_USER_EMAIL is empty. Set it"},
		{
			"zone on host is yes",
			sandbox.KeyZoneOnHost,
			"yes",
			`KEYCARD_LOCAL_ZONE_ON_HOST must be true or empty (got "yes")`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings := valid()
			settings[tc.key] = tc.value
			local, calls := newLocal(t, settings, "")
			err := local.Run(t.Context(), sandbox.RunOptions{})
			checkErr(t, err, []error{sandbox.ErrSettings}, tc.wantText)
			if got := readCalls(t, calls); got != "" {
				t.Fatalf("calls = %q, want none", got)
			}
		})
	}
}

// zoneSetupOutput is the zone-setup output of an earlier run, for
// ada@example.com.
const zoneSetupOutput = `k8s_sandbox:
  application_id: app-1
  application_identifier: urn:keycard:sandbox:agents
  namespace: agents
  provider_id: provider-1
sandbox_user_access:
- user_email: ` + email + `
  user_id: ` + userID + `
  service_account: system:serviceaccount:agents:` + userID + `
  client_id: client-1
  namespace: agents
`

// TestCheckTargetFor checks that each name the checks pass to kubectl, from
// a flag or from the zone-setup output, is a Kubernetes name.
func TestCheckTargetFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                   string
		output                 string
		namespace, pod, userID string
		want                   sandbox.CheckTarget
		wantText               string
	}{
		{
			name:   "from the zone-setup output",
			output: zoneSetupOutput,
			want:   sandbox.CheckTarget{Cluster: cluster, Namespace: namespace, Pod: userID, UserID: userID},
		},
		{
			name:      "flags beat the zone-setup output",
			output:    zoneSetupOutput,
			namespace: "agents-2", pod: "pod-1.a", userID: "user2",
			want: sandbox.CheckTarget{Cluster: cluster, Namespace: "agents-2", Pod: "pod-1.a", UserID: "user2"},
		},
		{
			name:     "pod is a flag",
			output:   zoneSetupOutput,
			pod:      "--all-containers",
			wantText: `the pod "--all-containers" is not valid`,
		},
		{
			name:      "namespace has a capital",
			namespace: "Agents", userID: userID,
			wantText: `the namespace "Agents" is not valid`,
		},
		{
			name:     "user id in the zone-setup output is a flag",
			output:   strings.ReplaceAll(zoneSetupOutput, "user_id: "+userID, "user_id: -x"),
			wantText: `the user id "-x" is not valid`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{sandbox.KeyUserEmail: email}, "")
			if tc.output != "" {
				writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), tc.output)
			}
			got, err := local.CheckTargetFor(cluster, tc.namespace, tc.pod, tc.userID)
			if tc.wantText != "" {
				checkErr(t, err, []error{sandbox.ErrTarget}, tc.wantText)
			} else if err != nil || got != tc.want {
				t.Fatalf("CheckTargetFor() = %+v, %v, want %+v", got, err, tc.want)
			}
			if got := readCalls(t, calls); got != "" {
				t.Fatalf("calls = %q, want none", got)
			}
		})
	}
}

// TestRun runs the whole flow against fake tools and the zone-setup output
// of an earlier run. The pinned helm renders the repository's sandbox chart.
// A run that fails must make exactly the calls in wantCalls, so each row
// shows the step Run stops at. A whole run must make each call in wantCalls
// and none in notCalls. Without KEYCARD_LOCAL_IMAGE_TAG, the pod runs the
// images the charts pin and Run does not import images. With it, Run imports
// the images at that tag.
func TestRun(t *testing.T) {
	t.Parallel()
	const (
		dockerCall = "docker KUBERC= version --format {{.Server.Version}}\n"
		inspects   = "docker KUBERC= image inspect ghcr.io/keycardai/egress-netsetup:local\n" +
			"docker KUBERC= image inspect ghcr.io/keycardai/egress-proxy:local\n" +
			"docker KUBERC= image inspect ghcr.io/keycardai/sandbox-base:local\n"
		importCall = "k3d KUBERC= image import --cluster asp ghcr.io/keycardai/egress-netsetup:local" +
			" ghcr.io/keycardai/egress-proxy:local ghcr.io/keycardai/sandbox-base:local\n"
		kube         = "kubectl KUBERC=off --context k3d-asp --namespace agents "
		bootstrapped = dockerCall + listCall + discoveryCall + mintCalls
		imported     = bootstrapped + inspects + importCall
	)
	tests := []struct {
		name    string
		tag     string
		sandbox fakeSandbox
		// noDocker makes docker a path that does not exist.
		noDocker  bool
		output    string
		want      []error
		wantText  string
		wantCalls []string
		notCalls  []string
		// wantImages are the images the applied pod runs.
		wantImages []string
	}{
		{
			name:     "docker missing",
			sandbox:  healthy(),
			noDocker: true,
			want:     []error{sandbox.ErrDocker},
			wantText: "no such file or directory. Install Docker, and start OrbStack or Docker Desktop",
		},
		{
			name:      "image missing",
			tag:       devTag,
			sandbox:   with(func(f *fakeSandbox) { f.missingImage = "ghcr.io/keycardai/egress-proxy:local" }),
			want:      []error{sandbox.ErrImages},
			wantText:  "ghcr.io/keycardai/egress-proxy:local. Build them in the egress-proxy repository",
			wantCalls: []string{dockerCall + listCall + discoveryCall + mintCalls + inspects},
		},
		{
			name:      "no zone-setup output",
			tag:       devTag,
			sandbox:   healthy(),
			want:      []error{sandbox.ErrZoneSetup},
			wantText:  "zone-setup.yaml is missing or empty. Run without --skip-zone-setup",
			wantCalls: []string{imported},
		},
		{
			name:      "zone-setup output without a provider",
			sandbox:   healthy(),
			output:    strings.ReplaceAll(zoneSetupOutput, "  provider_id: provider-1\n", ""),
			want:      []error{sandbox.ErrZoneSetup},
			wantText:  "zone-setup.yaml has no k8s_sandbox.provider_id. Run without --skip-zone-setup",
			wantCalls: []string{bootstrapped},
		},
		{
			name:      "zone-setup output without a client id",
			sandbox:   healthy(),
			output:    strings.ReplaceAll(zoneSetupOutput, "  client_id: client-1\n", ""),
			want:      []error{sandbox.ErrZoneSetup},
			wantText:  "zone-setup.yaml has no sandbox_user_access[0].client_id. Run without --skip-zone-setup",
			wantCalls: []string{bootstrapped},
		},
		{
			name:    "whole flow",
			sandbox: healthy(),
			output:  zoneSetupOutput,
			wantCalls: []string{
				bootstrapped,
				"helm KUBERC=off upgrade --install keycard-sandbox ",
				"kubectl KUBERC=off --context k3d-asp apply --namespace agents -f -\n" +
					kube + "wait --for=create pod/" + userID + " --timeout=120s\n" +
					kube + "wait --for=condition=Ready pod/" + userID + " --timeout=300s\n" +
					kube + "exec " + userID + " -c agent -- curl",
			},
			notCalls:   []string{"docker KUBERC= image", "image import"},
			wantImages: pinnedImages(),
		},
		{
			name:      "images at a tag",
			tag:       devTag,
			sandbox:   healthy(),
			output:    zoneSetupOutput,
			wantCalls: []string{imported},
			wantImages: []string{
				"ghcr.io/keycardai/egress-netsetup:local", "ghcr.io/keycardai/egress-proxy:local",
				"ghcr.io/keycardai/sandbox-base:local",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{
				sandbox.KeyCluster: cluster, sandbox.KeyIssuer: testIssuer,
				sandbox.KeyZoneURL: zoneURL, sandbox.KeyUserEmail: email, sandbox.KeyImageTag: tc.tag,
			}, tc.sandbox.script())
			if tc.noDocker {
				removeDocker(t, local)
			}
			out := &bytes.Buffer{}
			local.Out = out
			linkCharts(t, local.Root)
			if tc.output != "" {
				writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), tc.output)
			}
			err := local.Run(t.Context(), sandbox.RunOptions{SkipZoneSetup: true})
			checkErr(t, err, tc.want, tc.wantText)
			got := readCalls(t, calls)
			if tc.want != nil {
				if want := strings.Join(tc.wantCalls, ""); got != want {
					t.Fatalf("calls = %q, want %q", got, want)
				}
				return
			}
			for _, want := range tc.wantCalls {
				if !strings.Contains(got, want) {
					t.Fatalf("calls lack %q\n%s", want, got)
				}
			}
			for _, absent := range tc.notCalls {
				if strings.Contains(got, absent) {
					t.Errorf("calls have %q\n%s", absent, got)
				}
			}
			checkRunWrote(t, local.Root, out.String())
			checkImages(t, local.Root, tc.wantImages)
		})
	}
}

// removeDocker makes local run docker from a path that does not exist.
func removeDocker(t *testing.T, local *sandbox.Local) {
	t.Helper()
	tool, missing := local.Tool, filepath.Join(t.TempDir(), "docker")
	local.Tool = func(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
		if name == "docker" {
			//nolint:gosec // G204: the path is the test's own missing file.
			return exec.CommandContext(ctx, missing, args...), nil
		}
		return tool(ctx, name, args...)
	}
}

// checkImages checks that the applied sandbox pod runs images.
func checkImages(t *testing.T, root string, images []string) {
	t.Helper()
	path := filepath.Join(root, "local", "out", "sandbox.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range images {
		if !strings.Contains(string(raw), "image: "+image+"\n") {
			t.Errorf("local/out/sandbox.yaml lacks the image %q", image)
		}
	}
}

// checkRunWrote checks that the output reports what a whole run wrote, and
// that those files exist under root.
func checkRunWrote(t *testing.T, root, out string) {
	t.Helper()
	for _, line := range []string{
		"Wrote local/out/install.values.yaml and local/out/sandbox.values.yaml.\n",
		"Wrote the applied manifests to local/out/sandbox.yaml.\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q", line)
		}
	}
	for _, name := range []string{"install.values.yaml", "sandbox.values.yaml", "sandbox.yaml"} {
		path := filepath.Join(root, "local", "out", name)
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the test's own file.
		if err != nil || len(raw) == 0 {
			t.Errorf("local/out/%s: %v, want it written", name, err)
		}
	}
}

// with returns healthy with change applied.
func with(change func(*fakeSandbox)) fakeSandbox {
	f := healthy()
	change(&f)
	return f
}

// linkCharts links the repository's sandbox chart and local values into
// root, where Run reads them.
func linkCharts(t *testing.T, root string) {
	t.Helper()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "local"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"install", "sandbox", filepath.Join("local", "values")} {
		if err = os.Symlink(filepath.Join(repo, dir), filepath.Join(root, dir)); err != nil {
			t.Fatal(err)
		}
	}
}
