// Package install installs the agent-sandbox controller, then the
// keycard-sandbox chart, into a cluster.
package install

// The agent-sandbox release whose controller manifest Run applies. Its
// sha256 is in install_test.go.
//go:generate go run download.go v1.0.4

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/keycardai/agent-sandbox-examples/internal/pinned"
)

const (
	release    = "keycard-sandbox"
	privileged = "privileged"
)

// ErrInstall is an install that was refused or that failed.
var ErrInstall = errors.New("install")

var (
	dnsLabel  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	shellSafe = regexp.MustCompile(`^[A-Za-z0-9@%_+=:,./-]+$`)
)

// overrides returns the variables that can override the cluster or
// identity that the kube context names. kubectl and helm run without them.
func overrides() []string {
	return []string{
		"KUBERC",
		"HELM_KUBEAPISERVER", "HELM_KUBEASGROUPS", "HELM_KUBEASUSER", "HELM_KUBECAFILE", "HELM_KUBECONTEXT",
		"HELM_KUBEINSECURE_SKIP_TLS_VERIFY", "HELM_KUBETLS_SERVER_NAME", "HELM_KUBETOKEN", "HELM_NAMESPACE",
	}
}

// Install installs the controller from the release manifest committed in
// install/agent-sandbox, then the chart. The chart has a SandboxTemplate,
// a kind the controller defines, so the controller goes first. The chart
// adopts an existing namespace.
//
// The egress proxy's init container needs the privileged Pod Security level.
// Run stops before any change when the namespace sets another level. It
// fails after the chart when the namespace does not enforce privileged.
type Install struct {
	// Root is the repository root. The chart is Root/install.
	Root string
	// Context is the kube context to install into.
	Context string
	// Namespace is the sandbox namespace.
	Namespace string
	// Values are the chart's values files. A later file wins, as in helm.
	Values []string
	// Tool returns a command that runs kubectl or helm.
	Tool func(ctx context.Context, name string, args ...string) (*exec.Cmd, error)
	// Out receives progress lines and the output of kubectl apply and helm.
	Out io.Writer
}

// Run installs the controller and the chart.
func (in *Install) Run(ctx context.Context) error {
	if err := in.validate(); err != nil {
		return err
	}
	if err := in.checkExistingLevels(ctx); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(in.Out, "Applying the agent-sandbox controller to %s\n", in.Context)
	manifest := filepath.Join(in.Root, "install", "agent-sandbox", "sandbox-with-extensions.yaml")
	if err := in.stream(ctx, "kubectl", "--context", in.Context, "apply", "--server-side", "-f", manifest); err != nil {
		return err
	}
	err := in.stream(ctx, "kubectl", "--context", in.Context, "-n", "agent-sandbox-system",
		"rollout", "status", "deployment/agent-sandbox-controller", "--timeout=180s")
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(in.Out, "Installing %s into namespace %s\n", release, in.Namespace)
	args := []string{
		"upgrade", "--install", release, filepath.Join(in.Root, "install"),
		"--kube-context", in.Context, "--namespace", in.Namespace, "--create-namespace", "--take-ownership",
	}
	for _, file := range in.Values {
		args = append(args, "--values", file)
	}
	if err = in.stream(ctx, "helm", append(args, "--wait")...); err != nil {
		return err
	}

	enforce, err := in.level(ctx, "enforce", false)
	if err != nil {
		return err
	}
	if enforce != privileged {
		return fmt.Errorf("%w: namespace %s enforces the Pod Security level %q, not privileged, "+
			"so the cluster will not admit a sandbox. Find what rewrites pod-security.kubernetes.io/enforce "+
			"on the namespace, then install again", ErrInstall, in.Namespace, enforce)
	}
	_, _ = fmt.Fprintf(in.Out, "Installed %s into namespace %s on %s\n", release, in.Namespace, in.Context)
	return nil
}

func (in *Install) validate() error {
	switch {
	case in.Context == "":
		return fmt.Errorf("%w: the kube context is empty", ErrInstall)
	case strings.HasPrefix(in.Context, "-"):
		return fmt.Errorf("%w: the context must not start with a dash (got %q)", ErrInstall, in.Context)
	case len(in.Namespace) > 63 || !dnsLabel.MatchString(in.Namespace):
		return fmt.Errorf("%w: the namespace must be a DNS label of lowercase letters, digits and dashes (got %q)",
			ErrInstall, in.Namespace)
	case len(in.Values) == 0:
		return fmt.Errorf("%w: no values file. Start from install/values.example.yaml", ErrInstall)
	}
	for _, file := range in.Values {
		if !readable(file) {
			return fmt.Errorf("%w: %s is not a readable file", ErrInstall, file)
		}
	}
	return nil
}

// readable reports whether file is a regular file this process can open.
func readable(file string) bool {
	f, err := os.Open(file) //nolint:gosec // G304: the path is a values file the operator named.
	if err != nil {
		return false
	}
	info, err := f.Stat()
	_ = f.Close()
	return err == nil && info.Mode().IsRegular()
}

// checkExistingLevels refuses a namespace that sets a Pod Security level
// other than privileged. helm cannot take over a label that another manager
// set to another level, so the error prints the kubectl command that fixes it.
func (in *Install) checkExistingLevels(ctx context.Context) error {
	for _, mode := range []string{"enforce", "audit", "warn"} {
		level, err := in.level(ctx, mode, true)
		if err != nil {
			return err
		}
		if level != "" && level != privileged {
			return fmt.Errorf("%w: namespace %s sets pod-security.kubernetes.io/%s=%s, and sandboxes need privileged. "+
				"If that is acceptable, run: kubectl --context %s label namespace %s "+
				"pod-security.kubernetes.io/%s=privileged --overwrite",
				ErrInstall, in.Namespace, mode, level, ShellQuote(in.Context), in.Namespace, mode)
		}
	}
	return nil
}

// level returns the namespace's Pod Security label for mode. With
// ignoreNotFound, it returns "" for a namespace that does not exist.
func (in *Install) level(ctx context.Context, mode string, ignoreNotFound bool) (string, error) {
	args := []string{"--context", in.Context, "get", "namespace", in.Namespace}
	if ignoreNotFound {
		args = append(args, "--ignore-not-found")
	}
	args = append(args, "-o", `jsonpath={.metadata.labels.pod-security\.kubernetes\.io/`+mode+"}")
	cmd, err := in.command(ctx, "kubectl", args...)
	if err != nil {
		return "", err
	}
	out, err := pinned.Run(cmd, nil)
	if err != nil {
		return "", fmt.Errorf("%w: cannot read the labels of namespace %s: %w", ErrInstall, in.Namespace, err)
	}
	return string(out), nil
}

// stream runs a tool with its output on Out.
func (in *Install) stream(ctx context.Context, tool string, args ...string) error {
	cmd, err := in.command(ctx, tool, args...)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = in.Out, in.Out
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrInstall, tool, strings.Join(args, " "), err)
	}
	return nil
}

// command returns a command for tool that runs without the overrides, and
// with kubectl's kuberc turned off.
func (in *Install) command(ctx context.Context, tool string, args ...string) (*exec.Cmd, error) {
	cmd, err := in.Tool(ctx, tool, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInstall, err)
	}
	env := slices.DeleteFunc(cmd.Environ(), func(kv string) bool {
		key, _, _ := strings.Cut(kv, "=")
		return slices.Contains(overrides(), key)
	})
	env = append(env, "KUBERC=off")
	cmd.Env = env
	return cmd, nil
}

// ShellQuote quotes s for a POSIX shell, so a printed command can be pasted.
func ShellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
