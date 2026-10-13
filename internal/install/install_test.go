package install_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-examples/internal/install"
)

// fakeTool is kubectl and helm. Each call appends the tool and its arguments
// to $CALLS, and fails unless KUBERC is off. helm also appends every HELM_
// variable it sees. Before the install, kubectl prints $EXISTING_<mode> as
// the namespace's Pod Security label for that mode, or fails when $EXISTING
// is FAIL. After the install, it prints $ENFORCE as the enforce label, or
// fails when that is FAIL. Each call fails when $FAIL names it.
const fakeTool = `#!/bin/bash
printf '%s %s\n' "$FAKE_TOOL" "$*" >>"$CALLS"
[[ ${KUBERC-} == off ]] || { echo "$FAKE_TOOL read KUBERC=${KUBERC-unset}" >&2; exit 1; }
if [[ $FAKE_TOOL == helm ]]; then
	env | grep '^HELM_' >>"$CALLS"
	[[ $FAIL == helm ]] && exit 1
	exit 0
fi
[[ $FAIL == apply && $* == *" apply "* ]] && exit 1
[[ $FAIL == rollout && $* == *" rollout "* ]] && exit 1
if [[ $* == *" --ignore-not-found "* ]]; then
	[[ ${EXISTING-} == FAIL ]] && exit 1
	args=$*
	mode=${args##*io/}
	existing=EXISTING_${mode%\}}
	printf '%s' "${!existing-}"
elif [[ $* == *" get namespace "* ]]; then
	[[ $ENFORCE == FAIL ]] && exit 1
	printf '%s' "$ENFORCE"
fi
`

// TestInstall runs Install against fake kubectl and helm with two values
// files, which helm must get in order. The fakes run with only the row's
// environment, without the host's HELM_ variables.
func TestInstall(t *testing.T) {
	t.Parallel()
	const cluster, root = "asp", "/repo"
	dir := t.TempDir()
	script := filepath.Join(dir, "tool")
	//nolint:gosec // G306: the fake tool must be executable.
	if err := os.WriteFile(script, []byte(fakeTool), 0o700); err != nil {
		t.Fatal(err)
	}
	values, overlay := filepath.Join(dir, "values.yaml"), filepath.Join(dir, "overlay.yaml")
	for _, file := range []string{values, overlay} {
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	kube := "kubectl --context " + cluster
	label := func(mode string) string {
		return kube + " get namespace agents --ignore-not-found" +
			` -o jsonpath={.metadata.labels.pod-security\.kubernetes\.io/` + mode + "}"
	}
	calls := []string{
		label("enforce"),
		label("audit"),
		label("warn"),
		kube + " apply --server-side -f " + root + "/install/agent-sandbox/sandbox-with-extensions.yaml",
		kube + " -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s",
		"helm upgrade --install keycard-sandbox " + root + "/install --kube-context " + cluster + " --namespace agents" +
			" --create-namespace --take-ownership --values " + values + " --values " + overlay + " --wait",
		kube + ` get namespace agents -o jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`,
	}
	upTo := func(n int) string { return strings.Join(calls[:n], "\n") + "\n" }
	all := upTo(len(calls))
	const (
		installed         = "Installed keycard-sandbox into namespace agents on " + cluster
		enforcePrivileged = "ENFORCE=privileged"
	)

	tests := []struct {
		name, context, namespace string
		values                   []string
		env                      []string
		wantErr                  string // the error, or "" for success
		wantOut, wantCalls       string
	}{
		{
			name:      "installs the controller, then the chart",
			env:       []string{enforcePrivileged},
			wantOut:   installed,
			wantCalls: all,
		},
		{
			name:      "stops when the namespace is not privileged",
			env:       []string{"ENFORCE=baseline"},
			wantErr:   `install: namespace agents enforces the Pod Security level "baseline", not privileged`,
			wantCalls: all,
		},
		{
			name:      "stops when the labels cannot be read",
			env:       []string{"ENFORCE=FAIL"},
			wantErr:   "install: cannot read the labels of namespace agents",
			wantCalls: all,
		},
		{
			name:      "stops when the controller apply fails",
			env:       []string{"FAIL=apply"},
			wantErr:   "install: kubectl --context asp apply --server-side",
			wantCalls: upTo(4),
		},
		{
			name:      "stops when the controller rollout fails",
			env:       []string{"FAIL=rollout"},
			wantErr:   "install: kubectl --context asp -n agent-sandbox-system rollout status",
			wantCalls: upTo(5),
		},
		{
			name:      "stops when helm fails",
			env:       []string{"FAIL=helm"},
			wantErr:   "install: helm upgrade --install keycard-sandbox",
			wantCalls: upTo(6),
		},
		{
			name: "stops before any change when the namespace enforces another level",
			env:  []string{"EXISTING_enforce=baseline"},
			wantErr: "If that is acceptable, run: kubectl --context " + cluster +
				" label namespace agents pod-security.kubernetes.io/enforce=privileged --overwrite",
			wantCalls: upTo(1),
		},
		{
			name:    "quotes the context in the printed command",
			context: "staging west",
			env:     []string{"EXISTING_enforce=baseline"},
			wantErr: `run: kubectl --context 'staging west' label namespace agents`,
			wantCalls: "kubectl --context staging west get namespace agents --ignore-not-found" +
				` -o jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}` + "\n",
		},
		{
			name:      "stops before any change when the namespace audits another level",
			env:       []string{"EXISTING_audit=restricted"},
			wantErr:   "install: namespace agents sets pod-security.kubernetes.io/audit=restricted",
			wantCalls: upTo(2),
		},
		{
			name:      "stops before any change when the existing labels cannot be read",
			env:       []string{"EXISTING=FAIL"},
			wantErr:   "install: cannot read the labels of namespace agents",
			wantCalls: upTo(1),
		},
		{
			name: "adopts a namespace that is already privileged",
			env: []string{
				enforcePrivileged,
				"EXISTING_enforce=privileged", "EXISTING_audit=privileged", "EXISTING_warn=privileged",
			},
			wantOut:   installed,
			wantCalls: all,
		},
		{
			name: "keeps kubectl and helm on the requested cluster",
			env: []string{
				enforcePrivileged,
				"HELM_KUBEAPISERVER=https://elsewhere", "HELM_KUBETOKEN=secret", "KUBERC=/elsewhere/kuberc",
			},
			wantOut:   installed,
			wantCalls: all,
		},
		{
			name:    "refuses a context that kubectl would read as a flag",
			context: "--kubeconfig=/elsewhere",
			wantErr: `install: the context must not start with a dash (got "--kubeconfig=/elsewhere")`,
		},
		{
			name:      "refuses a namespace that is not a DNS label",
			namespace: "Agents",
			wantErr:   `install: the namespace must be a DNS label of lowercase letters, digits and dashes (got "Agents")`,
		},
		{
			name:    "refuses a values file that is not there",
			values:  []string{filepath.Join(dir, "missing.yaml")},
			wantErr: "install: " + filepath.Join(dir, "missing.yaml") + " is not a readable file",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			callsFile := filepath.Join(t.TempDir(), "calls")
			tool := func(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
				cmd := exec.CommandContext(ctx, script, args...) //nolint:gosec // G204: the script is the test's fake tool.
				cmd.Env = append([]string{"PATH=/usr/bin:/bin", "FAKE_TOOL=" + name, "CALLS=" + callsFile}, tc.env...)
				return cmd, nil
			}
			out := &bytes.Buffer{}
			installer := &install.Install{
				Root:      root,
				Context:   cmp.Or(tc.context, cluster),
				Namespace: cmp.Or(tc.namespace, "agents"),
				Values:    tc.values,
				Tool:      tool,
				Out:       out,
			}
			if installer.Values == nil {
				installer.Values = []string{values, overlay}
			}

			checkRun(t, installer.Run(t.Context()), out.String(), tc.wantErr, tc.wantOut)
			// The calls file exists only if a tool ran.
			got, _ := os.ReadFile(callsFile) //nolint:gosec // G304: the path is the test's temp file.
			if string(got) != tc.wantCalls {
				t.Errorf("calls =\n%s\nwant\n%s", got, tc.wantCalls)
			}
		})
	}
}

// checkRun checks that Run succeeded when wantErr is empty, and otherwise
// failed with ErrInstall and wantErr, without reporting success.
func checkRun(t *testing.T, err error, out, wantErr, wantOut string) {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("Run() = %v, want success\n%s", err, out)
		}
	} else if !errors.Is(err, install.ErrInstall) || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("Run() = %v, want ErrInstall containing %q", err, wantErr)
	}
	if !strings.Contains(out, wantOut) {
		t.Errorf("output lacks %q\n%s", wantOut, out)
	}
	if wantErr != "" && strings.Contains(out, "Installed") {
		t.Errorf("output reports success\n%s", out)
	}
}
