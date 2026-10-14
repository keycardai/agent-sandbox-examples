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
		{"image tag latest", sandbox.KeyImageTag, "latest", "KEYCARD_LOCAL_IMAGE_TAG must not be latest"},
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
// images the charts pin and Run does not import the three images. With it,
// Run imports the images at that tag, and a missing image stops Run before
// the import. A scenario with a Dockerfile builds its image FROM the pinned
// sandbox-base and imports it, and the sandbox runs it with the scenario's
// environment. Run deletes a pod that no longer matches the Sandbox, or
// whose agent exited, so the controller creates it again.
func TestRun(t *testing.T) {
	t.Parallel()
	const (
		images = "ghcr.io/keycardai/egress-netsetup:local ghcr.io/keycardai/egress-proxy:local" +
			" ghcr.io/keycardai/sandbox-base:local"
		dockerCall = "docker KUBERC= version --format {{.Server.Version}}\n"
		inspects   = "docker KUBERC= image inspect ghcr.io/keycardai/egress-netsetup:local\n" +
			"docker KUBERC= image inspect ghcr.io/keycardai/egress-proxy:local\n" +
			"docker KUBERC= image inspect ghcr.io/keycardai/sandbox-base:local\n"
		importCall   = "k3d KUBERC= image import --cluster asp " + images + "\n"
		kube         = "kubectl KUBERC=off --context k3d-asp --namespace agents "
		bootstrapped = dockerCall + listCall + discoveryCall + mintCalls
		imported     = bootstrapped + inspects + importCall
	)
	tests := []struct {
		name    string
		tag     string
		sandbox fakeSandbox
		// noDocker makes docker a path that does not exist.
		noDocker bool
		output   string
		// scenario adds agentScenario with a Dockerfile and runs it.
		scenario bool
		// tool runs before the fake sandbox.
		tool     string
		want     []error
		wantText string
		// wantCalls, wantOut and wantManifest are substrings of the calls,
		// the output and local/out/sandbox.yaml.
		wantCalls    []string
		wantOut      []string
		wantManifest []string
		notCalls     []string
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
				"/local/out/install.values.yaml --values ", "/local/out/scenario.values.yaml --wait\n",
				"kubectl KUBERC=off --context k3d-asp apply --namespace agents -f -\n" +
					kube + "get pod " + userID + " --ignore-not-found -o jsonpath={.status.phase}\n" +
					kube + "wait --for=create pod/" + userID + " --timeout=120s\n" +
					kube + "wait --for=condition=Ready pod/" + userID + " --timeout=5s\n" +
					kube + "exec " + userID + " -c agent -- curl",
			},
			notCalls:     []string{"docker KUBERC= image", "image import", "docker KUBERC= build"},
			wantManifest: append(imageLines(pinnedImages()), "user: x-access-token\n"),
		},
		{
			name:      "images at a tag",
			tag:       devTag,
			sandbox:   healthy(),
			output:    zoneSetupOutput,
			wantCalls: []string{imported},
			wantOut:   []string{"Imported " + strings.ReplaceAll(images, " ", ", ") + " into the k3d cluster asp.\n"},
			wantManifest: imageLines([]string{
				"ghcr.io/keycardai/egress-netsetup:local", "ghcr.io/keycardai/egress-proxy:local",
				"ghcr.io/keycardai/sandbox-base:local",
			}),
		},
		{
			name:     "scenario with an image",
			sandbox:  healthy(),
			output:   zoneSetupOutput,
			scenario: true,
			wantCalls: []string{
				bootstrapped + "docker KUBERC= build --tag ghcr.io/keycardai/sandbox-agent:local --build-arg BASE_IMAGE=" +
					pinnedImages()[2] + " ",
				"/scenarios/agent\nk3d KUBERC= image import --cluster asp ghcr.io/keycardai/sandbox-agent:local\n",
			},
			wantOut: []string{
				"Built the image ghcr.io/keycardai/sandbox-agent:local.\n",
				"Imported ghcr.io/keycardai/sandbox-agent:local into the k3d cluster asp.\n",
			},
			wantManifest: []string{
				"image: ghcr.io/keycardai/sandbox-agent:local\n",
				"- name: \"REPO\"\n", "value: \"https://github.com/octo/demo.git\"\n",
				"url: https://api.example.com\n",
			},
		},
		{
			name:    "sandbox template changed",
			sandbox: healthy(),
			output:  zoneSetupOutput,
			tool: `case "$*" in
*"get sandbox ` + userID + ` --ignore-not-found"*)
	[[ -f $CALLS.applied ]] && echo 2 || echo 1; exit 0 ;;
*" apply --namespace "*) touch "$CALLS.applied" ;;
*"get pod ` + userID + ` --ignore-not-found"*) echo Running; exit 0 ;;
esac
`,
			wantCalls: []string{"apply --namespace agents -f -\n" + kube + "get pod " + userID +
				" --ignore-not-found -o jsonpath={.status.phase}\n" + kube + "get sandbox " + userID +
				" --ignore-not-found -o jsonpath={.metadata.generation}\n" + kube + "delete pod " + userID +
				" --wait=true\n" + kube + "wait --for=create pod/" + userID},
			wantOut: []string{
				"Deleted the pod " + userID + ", so the controller creates it again from the sandbox's template.\n",
			},
		},
		{
			name:    "agent exited",
			sandbox: healthy(),
			output:  zoneSetupOutput,
			tool: `case "$*" in
*"get sandbox ` + userID + ` --ignore-not-found"*) echo 1; exit 0 ;;
*"get pod ` + userID + ` --ignore-not-found"*) echo Failed; exit 0 ;;
esac
`,
			wantCalls: []string{kube + "delete pod " + userID + " --wait=true\n"},
		},
		{
			name:    "pod up to date",
			sandbox: healthy(),
			output:  zoneSetupOutput,
			tool: `case "$*" in
*"get sandbox ` + userID + ` --ignore-not-found"*) echo 1; exit 0 ;;
*"get pod ` + userID + ` --ignore-not-found"*) echo Running; exit 0 ;;
esac
`,
			notCalls: []string{"delete pod"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{
				sandbox.KeyCluster: cluster, sandbox.KeyIssuer: testIssuer,
				sandbox.KeyZoneURL: zoneURL, sandbox.KeyUserEmail: email, sandbox.KeyImageTag: tc.tag,
			}, tc.tool+tc.sandbox.script())
			if tc.noDocker {
				removeDocker(t, local)
			}
			out := &bytes.Buffer{}
			local.Out = out
			linkCharts(t, local.Root)
			if tc.scenario {
				addScenario(t, local.Root, agentName, agentScenario, true)
				writeFile(t, filepath.Join(local.Root, "local", "answers.yaml"), "repo: octo/demo\n")
				local.Settings[sandbox.KeyScenario] = agentName
			}
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
			checkCalls(t, got, tc.wantCalls, tc.notCalls)
			checkRunWrote(t, local.Root, out.String())
			checkContains(t, "output", out.String(), tc.wantOut)
			manifest, _ := os.ReadFile(filepath.Join(local.Root, "local", "out", "sandbox.yaml"))
			checkContains(t, "sandbox.yaml", string(manifest), tc.wantManifest)
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

// checkCalls requires got to hold each of want and none of absent.
func checkCalls(t *testing.T, got string, want, absent []string) {
	t.Helper()
	for _, call := range want {
		if !strings.Contains(got, call) {
			t.Fatalf("calls lack %q\n%s", call, got)
		}
	}
	for _, call := range absent {
		if strings.Contains(got, call) {
			t.Errorf("calls have %q\n%s", call, got)
		}
	}
}

// imageLines returns the manifest lines that run images.
func imageLines(images []string) []string {
	lines := make([]string, 0, len(images))
	for _, image := range images {
		lines = append(lines, "image: "+image+"\n")
	}
	return lines
}

// checkRunWrote checks that the output reports what a whole run imported and
// wrote, and that those files exist under root.
func checkRunWrote(t *testing.T, root, out string) {
	t.Helper()
	for _, line := range []string{
		"Wrote local/out/install.values.yaml, local/out/sandbox.values.yaml and local/out/scenario.values.yaml.\n",
		"Wrote the applied manifests to local/out/sandbox.yaml.\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q", line)
		}
	}
	for _, name := range []string{"install.values.yaml", "sandbox.values.yaml", "scenario.values.yaml", "sandbox.yaml"} {
		path := filepath.Join(root, "local", "out", name)
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the test's own file.
		if err != nil || len(raw) == 0 {
			t.Errorf("local/out/%s: %v, want it written", name, err)
		}
	}
}

// checkContains requires got to contain each of wants. what names got in
// the error.
func checkContains(t *testing.T, what, got string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("%s lacks %q\n%s", what, want, got)
		}
	}
}

// with returns healthy with change applied.
func with(change func(*fakeSandbox)) fakeSandbox {
	f := healthy()
	change(&f)
	return f
}

// linkCharts links the repository's sandbox chart, local values and
// scenarios into root, where Run reads them. Each scenario is its own link,
// so a test can add a scenario of its own.
func linkCharts(t *testing.T, root string) {
	t.Helper()
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := filepath.Glob(filepath.Join(repo, "scenarios", "*"))
	if err != nil {
		t.Fatal(err)
	}
	links := make([]string, 0, len(scenarios)+3)
	links = append(links, "install", "sandbox", filepath.Join("local", "values"))
	for _, dir := range scenarios {
		links = append(links, filepath.Join("scenarios", filepath.Base(dir)))
	}
	for _, dir := range links {
		if err = os.MkdirAll(filepath.Dir(filepath.Join(root, dir)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err = os.Symlink(filepath.Join(repo, dir), filepath.Join(root, dir)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRunPodStops checks that Run stops waiting for the agent's pod once the
// pod has stopped, as when the agent exits at startup, and names the agent's
// last log line. A pod that stopped never becomes ready.
func TestRunPodStops(t *testing.T) {
	t.Parallel()
	const lastLine = "Slack conversations.open failed: user_not_found"
	local, _ := newLocal(t, sandbox.Settings{
		sandbox.KeyCluster: cluster, sandbox.KeyIssuer: testIssuer,
		sandbox.KeyZoneURL: zoneURL, sandbox.KeyUserEmail: email,
	}, `case "$*" in
*"wait --for=condition=Ready pod/`+userID+` --timeout=5s")
	echo 'error: timed out waiting for the condition' >&2; exit 1 ;;
*"get pod `+userID+` -o jsonpath={.status.phase}") echo Failed; exit 0 ;;
*"logs `+userID+` -c agent --tail=1") echo '`+lastLine+`'; exit 0 ;;
esac
`+healthy().script())
	local.Out = &bytes.Buffer{}
	linkCharts(t, local.Root)
	writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), zoneSetupOutput)
	err := local.Run(t.Context(), sandbox.RunOptions{SkipZoneSetup: true})
	checkErr(t, err, []error{sandbox.ErrPod},
		userID+" stopped (Failed) before it was ready. The agent's last log line: "+lastLine)
}
