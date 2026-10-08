package sandbox_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

const (
	userID     = "user1abc"
	namespace  = "agents"
	checkOK    = "every automated check passed"
	oneFailed  = "1 of 4"
	deniedLine = `{"destination":{"host":"example.com","port":53},` +
		`"decision":{"verdict":"deny","reason":"no_matching_endpoint"}}`
	refusedLine = `{"destination":{"host":"api.github.com","port":443},` +
		`"decision":{"verdict":"deny","reason":"exchange_denied","error":"access_denied"}}`
)

// fakeSandbox sets what the fake tools return for a running sandbox.
type fakeSandbox struct {
	allowed        string // the status pypi returns
	githubStatus   string
	githubBody     string
	deniedExit     int    // curl's exit code for example.com
	logs           string // the proxy's log
	serviceAccount string
	// tlsFailOnce makes the first pypi fetch exit 60, as curl does before
	// the agent trusts the proxy.
	tlsFailOnce bool
	// missingImage is an image docker image inspect does not find.
	missingImage string
}

// healthy returns a sandbox that passes every check.
func healthy() fakeSandbox {
	return fakeSandbox{
		allowed: "200", githubStatus: "200", githubBody: `{"login":"octocat"}`, deniedExit: 6,
		logs: deniedLine, serviceAccount: userID,
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
*"exec %[2]s -c agent -- sh -c"*) printf '%%s\n__sandbox_status__:%%s' '%[5]s' '%[6]s' ;;
*"exec %[2]s -c agent -- curl"*"https://example.com/")
	[[ %[7]d == 0 ]] && printf '\n__sandbox_status__:200'
	exit %[7]d ;;
*"logs %[2]s -c egress-proxy"*) printf '%%s\n' '%[8]s' ;;
*"get pod %[2]s -o jsonpath={.spec.serviceAccountName}") printf '%%s' '%[9]s' ;;
*"get namespace %[10]s -o jsonpath="*) printf privileged ;;
*" apply "*) echo "applied" ;;
esac
`, f.missingImage, userID, tlsFail, f.allowed, f.githubBody, f.githubStatus, f.deniedExit, f.logs,
		f.serviceAccount, namespace)
}

// TestCheck runs the four checks against a fake sandbox. A failing check
// prints the value it got and counts toward ErrCheck.
func TestCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		sandbox  fakeSandbox
		login    string
		want     []error
		wantText string
		wantOut  []string
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
				"MANUAL  Open the zone's event log.",
				checkOK,
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
			name:     "no audit record",
			sandbox:  with(func(f *fakeSandbox) { f.logs = refusedLine }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut: []string{
				"  FAIL  denied: https://example.com/ was blocked, but the proxy did not log a deny record for it\n",
			},
		},
		{
			name:     "another ServiceAccount",
			sandbox:  with(func(f *fakeSandbox) { f.serviceAccount = "default" }),
			want:     []error{sandbox.ErrCheck},
			wantText: oneFailed,
			wantOut:  []string{`  FAIL  the pod runs as the ServiceAccount "default", want "` + userID + `"` + "\n"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, _ := newLocal(t, sandbox.Settings{sandbox.KeyGitHubLogin: tc.login}, tc.sandbox.script())
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
			sandbox.KeyZoneURL: "https://zone.example.com", sandbox.KeyUserEmail: "ada@example.com",
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

// TestRun runs the whole flow against fake tools and the zone-setup output
// of an earlier run. The pinned helm renders the repository's sandbox chart.
// A missing image stops Run before the import. A missing zone-setup output
// stops it before the install.
func TestRun(t *testing.T) {
	t.Parallel()
	const (
		images = "ghcr.io/keycardai/egress-netsetup:local ghcr.io/keycardai/egress-proxy:local" +
			" ghcr.io/keycardai/sandbox-base:local"
		inspects = "docker KUBERC= image inspect ghcr.io/keycardai/egress-netsetup:local\n" +
			"docker KUBERC= image inspect ghcr.io/keycardai/egress-proxy:local\n" +
			"docker KUBERC= image inspect ghcr.io/keycardai/sandbox-base:local\n"
		importCall = "k3d KUBERC= image import --cluster asp " + images + "\n"
		kube       = "kubectl KUBERC=off --context k3d-asp --namespace agents "
	)
	output := `k8s_sandbox:
  application_id: app-1
  application_identifier: urn:keycard:sandbox:agents
  namespace: agents
  provider_id: provider-1
sandbox_user_access:
- user_email: ada@example.com
  user_id: ` + userID + `
  service_account: system:serviceaccount:agents:` + userID + `
  client_id: client-1
  namespace: agents
`
	tests := []struct {
		name      string
		sandbox   fakeSandbox
		output    string
		want      []error
		wantText  string
		wantCalls []string
	}{
		{
			name:      "image missing",
			sandbox:   with(func(f *fakeSandbox) { f.missingImage = "ghcr.io/keycardai/egress-proxy:local" }),
			want:      []error{sandbox.ErrImages},
			wantText:  "ghcr.io/keycardai/egress-proxy:local. Build them in the egress-proxy repository",
			wantCalls: []string{listCall + discoveryCall + mintCalls + inspects},
		},
		{
			name:      "no zone-setup output",
			sandbox:   healthy(),
			want:      []error{sandbox.ErrZoneSetup},
			wantText:  "zone-setup.yaml is missing or empty. Run without --skip-zone-setup",
			wantCalls: []string{listCall + discoveryCall + mintCalls + inspects + importCall},
		},
		{
			name:    "whole flow",
			sandbox: healthy(),
			output:  output,
			wantCalls: []string{
				listCall + discoveryCall + mintCalls + inspects + importCall,
				"helm KUBERC=off upgrade --install keycard-sandbox ",
				"kubectl KUBERC=off --context k3d-asp apply --namespace agents -f -\n" +
					kube + "wait --for=create pod/" + userID + " --timeout=120s\n" +
					kube + "wait --for=condition=Ready pod/" + userID + " --timeout=300s\n" +
					kube + "exec " + userID + " -c agent -- curl",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local, calls := newLocal(t, sandbox.Settings{
				sandbox.KeyCluster: cluster, sandbox.KeyIssuer: testIssuer,
				sandbox.KeyZoneURL: "https://zone.example.com", sandbox.KeyUserEmail: "ada@example.com",
			}, tc.sandbox.script())
			out := &bytes.Buffer{}
			local.Out = out
			linkCharts(t, local.Root)
			if tc.output != "" {
				writeFile(t, filepath.Join(local.Root, "local", "out", "zone-setup.yaml"), tc.output)
			}
			err := local.Run(t.Context(), sandbox.RunOptions{SkipZoneSetup: true})
			checkErr(t, err, tc.want, tc.wantText)
			got := readCalls(t, calls)
			for _, want := range tc.wantCalls {
				if !strings.Contains(got, want) {
					t.Fatalf("calls lack %q\n%s", want, got)
				}
			}
			if tc.want == nil {
				checkRunWrote(t, local.Root, out.String())
			}
		})
	}
}

// checkRunWrote checks that the output reports what a whole run imported and
// wrote, and that those files exist under root.
func checkRunWrote(t *testing.T, root, out string) {
	t.Helper()
	for _, line := range []string{
		"Imported 3 images into the k3d cluster asp.\n",
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
