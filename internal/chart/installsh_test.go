package chart_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// controllerSHA256 is the sha256 of the agent-sandbox v1.0.4
// sandbox-with-extensions.yaml, as GitHub publishes it for the release asset.
// install/agent-sandbox/refresh.sh names the release.
const controllerSHA256 = "8cbe7f4c252463667e2286993bd735cb33bb2e47d7f304b92aa2ca97f57052c0"

// TestControllerManifest pins the controller manifest that install.sh
// applies, so a manifest that changes without this digest fails.
func TestControllerManifest(t *testing.T) {
	t.Parallel()
	manifest, err := os.ReadFile(
		filepath.Join(installRoot(t), "install", "agent-sandbox", "sandbox-with-extensions.yaml"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(manifest)
	if got := hex.EncodeToString(sum[:]); got != controllerSHA256 {
		t.Errorf("controller manifest sha256 = %s, want %s", got, controllerSHA256)
	}
}

// TestInstallScript runs install.sh under /bin/bash, which is bash 3.2 on
// macOS. Decoy kubectl and helm come first on PATH and record any call. A fake
// mise answers only against the repository root, as scripts/pinned asks it
// to, and resolves kubectl and helm to fakes that append their arguments to
// $CALLS. The fake kubectl prints $ENFORCE as the namespace's enforce label,
// or fails when it is FAIL.
func TestInstallScript(t *testing.T) {
	t.Parallel()
	const cluster = "asp"
	root := installRoot(t)
	decoys, miseDir, pinned := t.TempDir(), t.TempDir(), t.TempDir()
	mise := `#!/bin/bash
tool=${!#}
[[ "$1 $2" == "-C ` + root + `" ]] || exit 1
case $tool in
kubectl | helm) ;;
*) exit 1 ;;
esac
case "${*:3:$#-3}" in
which) echo "` + pinned + `/$tool" ;;
"ls --current --json")
	printf '[\n  {\n    "version": "1.0.0",\n    "requested_version": "1.0.0",\n'
	printf '    "install_path": "%s",\n' "` + pinned + `"
	printf '    "source": {\n      "type": "mise.toml",\n      "path": "%s"\n    },\n' "` + root + `/mise.toml"
	printf '    "installed": true,\n    "active": true\n  }\n]\n'
	;;
*) exit 1 ;;
esac
`
	kubectl := `#!/bin/bash
printf 'kubectl %s\n' "$*" >>"$CALLS"
if [[ $* == *" get namespace "* ]]; then
	[[ $ENFORCE == FAIL ]] && exit 1
	printf '%s' "$ENFORCE"
fi
`
	for path, body := range map[string]string{
		filepath.Join(decoys, "helm"):    "#!/bin/bash\nprintf 'decoy helm %s\\n' \"$*\" >>\"$CALLS\"\n",
		filepath.Join(decoys, "kubectl"): "#!/bin/bash\nprintf 'decoy kubectl %s\\n' \"$*\" >>\"$CALLS\"\n",
		filepath.Join(miseDir, "mise"):   mise,
		filepath.Join(pinned, "helm"):    "#!/bin/bash\nprintf 'helm %s\\n' \"$*\" >>\"$CALLS\"\n",
		filepath.Join(pinned, "kubectl"): kubectl,
	} {
		//nolint:gosec // The fakes must be executable.
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	install := filepath.Join(root, "install")
	kube := "kubectl --context " + cluster
	all := strings.Join([]string{
		kube + " apply --server-side -f " + install + "/agent-sandbox/sandbox-with-extensions.yaml",
		kube + " -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=180s",
		"helm upgrade --install keycard-sandbox " + install + " --kube-context " + cluster + " --namespace agents" +
			" --create-namespace --take-ownership --values " + values + " --set-string namespace=agents --wait",
		kube + ` get namespace agents -o jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}`,
	}, "\n") + "\n"

	tests := []struct {
		name, context, enforce string
		wantErr                bool
		wantOut, wantCalls     string
	}{
		{
			name:      "installs the controller, then the chart",
			context:   cluster,
			enforce:   "privileged",
			wantOut:   "install.sh: installed keycard-sandbox into namespace agents on " + cluster,
			wantCalls: all,
		},
		{
			name:    "refuses localdev",
			context: "localdev",
			wantErr: true,
			wantOut: "install.sh: 'localdev' is the Keycard localdev stack. install.sh never writes to it.",
		},
		{
			name:    "refuses k3d-localdev",
			context: "k3d-localdev",
			wantErr: true,
			wantOut: "install.sh: 'k3d-localdev' is the Keycard localdev stack. install.sh never writes to it.",
		},
		{
			name:      "stops when the namespace is not privileged",
			context:   cluster,
			enforce:   "baseline",
			wantErr:   true,
			wantOut:   "install.sh: namespace agents enforces the Pod Security level 'baseline', not privileged",
			wantCalls: all,
		},
		{
			name:      "stops when the labels cannot be read",
			context:   cluster,
			enforce:   "FAIL",
			wantErr:   true,
			wantOut:   "install.sh: cannot read the labels of namespace agents",
			wantCalls: all,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := filepath.Join(t.TempDir(), "calls")
			//nolint:gosec // The arguments are fixed inputs to a script in this repository.
			cmd := exec.CommandContext(t.Context(), "/bin/bash", filepath.Join(install, "install.sh"),
				"--context", tc.context, "--values", values)
			cmd.Env = []string{
				"PATH=" + decoys + ":" + miseDir + ":/usr/bin:/bin",
				"CALLS=" + calls, "ENFORCE=" + tc.enforce,
			}

			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %t\n%s", err, tc.wantErr, out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("output lacks %q\n%s", tc.wantOut, out)
			}
			// A missing file means no tool ran.
			got, _ := os.ReadFile(calls) //nolint:gosec // The path is the test's temp file.
			if string(got) != tc.wantCalls {
				t.Errorf("calls =\n%s\nwant\n%s", got, tc.wantCalls)
			}
		})
	}
}

// installRoot returns the repository root with symlinks resolved, as pwd -P
// prints it and scripts/pinned passes it to mise.
func installRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err == nil {
		root, err = filepath.EvalSymlinks(root)
	}
	if err != nil {
		t.Fatal(err)
	}
	return root
}
