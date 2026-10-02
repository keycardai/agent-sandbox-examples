package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Images are the egress-proxy images every sandbox pod runs, built from an
// egress-proxy checkout under the tag local.
var Images = []string{"egress-proxy:local", "egress-netsetup:local", "sandbox-base:local"}

// k3sImage is the k3s release agent-sandbox-poc's local lane uses.
const k3sImage = "rancher/k3s:v1.36.4-k3s1"

// run runs a command and responds with its standard output. A failure carries
// the command's standard error.
func run(ctx context.Context, stdin []byte, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLines(msg, 4))
	}
	return out.String(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// Kube runs kubectl against one context.
type Kube struct{ Context string }

func (k Kube) kubectl(ctx context.Context, stdin []byte, args ...string) (string, error) {
	return run(ctx, stdin, "kubectl", append([]string{"--context", k.Context}, args...)...)
}

// Apply applies the objects as a List.
func (k Kube) Apply(ctx context.Context, objects []map[string]any) error {
	b, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objects})
	if err != nil {
		return fmt.Errorf("encode manifests: %w", err)
	}
	_, err = k.kubectl(ctx, b, "apply", "-f", "-")
	return err
}

// GetRaw reads a raw API path, such as the issuer discovery document.
func (k Kube) GetRaw(ctx context.Context, path string) ([]byte, error) {
	out, err := k.kubectl(ctx, nil, "get", "--raw", path)
	return []byte(out), err
}

// CreateToken mints a ServiceAccount token.
func (k Kube) CreateToken(ctx context.Context, namespace, serviceAccount, audience string) (string, error) {
	out, err := k.kubectl(
		ctx,
		nil,
		"-n",
		namespace,
		"create",
		"token",
		serviceAccount,
		"--audience",
		audience,
		"--duration",
		"10m",
	)
	return strings.TrimSpace(out), err
}

// WaitReady waits for a pod to be ready.
func (k Kube) WaitReady(ctx context.Context, namespace, pod string, timeout time.Duration) error {
	_, err := k.kubectl(
		ctx,
		nil,
		"-n",
		namespace,
		"wait",
		"--for=condition=Ready",
		"pod/"+pod,
		"--timeout",
		timeout.String(),
	)
	return err
}

// Exec runs a command in a container and responds with its standard output.
func (k Kube) Exec(ctx context.Context, namespace, pod, container string, command ...string) (string, error) {
	args := append([]string{"-n", namespace, "exec", pod, "-c", container, "--"}, command...)
	return k.kubectl(ctx, nil, args...)
}

// Delete deletes objects, ignoring ones already gone.
func (k Kube) Delete(ctx context.Context, namespace string, refs ...string) error {
	args := []string{"delete", "--ignore-not-found", "--wait=false"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	_, err := k.kubectl(ctx, nil, append(args, refs...)...)
	return err
}

// Exists reports whether an object exists.
func (k Kube) Exists(ctx context.Context, namespace, ref string) bool {
	_, err := k.kubectl(ctx, nil, "-n", namespace, "get", ref)
	return err == nil
}

// ClusterContext is the kubectl context k3d gives a cluster.
func ClusterContext(cluster string) string { return "k3d-" + cluster }

// ClusterExists reports whether k3d has a cluster with this name.
func ClusterExists(ctx context.Context, cluster string) (bool, error) {
	out, err := run(ctx, nil, "k3d", "cluster", "list", "-o", "json")
	if err != nil {
		return false, err
	}
	var clusters []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(out), &clusters); err != nil {
		return false, fmt.Errorf("read k3d cluster list: %w", err)
	}
	for _, c := range clusters {
		if c.Name == cluster {
			return true, nil
		}
	}
	return false, nil
}

// CreateCluster creates a k3d cluster whose API server mints ServiceAccount
// tokens with the issuer, the flags agent-sandbox-poc's local/bootstrap.sh
// uses.
func CreateCluster(ctx context.Context, cluster, issuer string) error {
	_, err := run(ctx, nil, "k3d", "cluster", "create", cluster,
		"--image", k3sImage,
		"--kubeconfig-update-default=true",
		"--kubeconfig-switch-context=false",
		"--k3s-arg", "--kube-apiserver-arg=service-account-issuer="+issuer+"@server:*",
		"--k3s-arg", "--kube-apiserver-arg=service-account-jwks-uri="+issuer+"/openid/v1/jwks@server:*",
		"--k3s-arg", "--disable=traefik@server:*",
		"--k3s-arg", "--disable=servicelb@server:*",
	)
	return err
}

// DeleteCluster deletes the k3d cluster.
func DeleteCluster(ctx context.Context, cluster string) error {
	_, err := run(ctx, nil, "k3d", "cluster", "delete", cluster)
	return err
}

// ClusterIssuer reads the issuer the cluster's API server publishes.
func ClusterIssuer(ctx context.Context, kube Kube) (string, error) {
	raw, err := kube.GetRaw(ctx, "/.well-known/openid-configuration")
	if err != nil {
		return "", err
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("read the discovery document: %w", err)
	}
	return doc.Issuer, nil
}

// HostGateway is the address a pod in the cluster reaches the host on, where
// localdev's gateway listens on port 443.
func HostGateway(ctx context.Context, cluster string) (string, error) {
	out, err := run(
		ctx,
		nil,
		"docker",
		"network",
		"inspect",
		"k3d-"+cluster,
		"--format",
		"{{range .IPAM.Config}}{{.Gateway}} {{end}}",
	)
	if err != nil {
		return "", err
	}
	for _, ip := range strings.Fields(out) {
		if !strings.Contains(ip, ":") {
			return ip, nil
		}
	}
	return "", fmt.Errorf("network k3d-%s has no IPv4 gateway", cluster)
}

// MissingImages lists the images Docker does not have.
func MissingImages(ctx context.Context) []string {
	var missing []string
	for _, image := range Images {
		if _, err := run(ctx, nil, "docker", "image", "inspect", image); err != nil {
			missing = append(missing, image)
		}
	}
	return missing
}

// BuildImages builds the three images from an egress-proxy checkout.
func BuildImages(ctx context.Context, egressProxyDir string) error {
	if _, err := os.Stat(filepath.Join(egressProxyDir, "Makefile")); err != nil {
		return fmt.Errorf("no egress-proxy checkout at %s (set --egress-proxy-dir): %w", egressProxyDir, err)
	}
	for _, target := range []string{"image-proxy", "image-netsetup", "image-sandbox-base"} {
		if _, err := run(ctx, nil, "make", "-C", egressProxyDir, target, "IMAGE_TAG=local"); err != nil {
			return err
		}
	}
	return nil
}

// ImportedImages reports whether the cluster's node has every image.
func ImportedImages(ctx context.Context, cluster string) bool {
	out, err := run(ctx, nil, "docker", "exec", "k3d-"+cluster+"-server-0", "crictl", "images", "-o", "json")
	if err != nil {
		return false
	}
	for _, image := range Images {
		if !strings.Contains(out, "docker.io/library/"+image) {
			return false
		}
	}
	return true
}

// ImportImages imports the images into the cluster.
func ImportImages(ctx context.Context, cluster string) error {
	_, err := run(ctx, nil, "k3d", append(append([]string{"image", "import"}, Images...), "-c", cluster)...)
	return err
}

// RequireTools fails when a command the CLI shells out to is missing.
func RequireTools() error {
	var missing []string
	for _, tool := range []string{"docker", "k3d", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing tools: %s", strings.Join(missing, ", "))
	}
	return nil
}

// ErrNotFound marks a lookup that found nothing.
var ErrNotFound = errors.New("not found")
