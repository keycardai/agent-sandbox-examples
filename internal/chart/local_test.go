package chart_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/keycardai/agent-sandbox-examples/internal/chart"
	"github.com/keycardai/agent-sandbox-examples/internal/sandbox"
)

// `sandbox run` renders both charts with the files under local/values and
// one more file per chart that sandbox.LocalValues returns. The tests render
// them for a zone on the Docker host, without and with a CA file.
const (
	localGateway  = "192.168.97.1"
	localZoneHost = "acme.example.com"
	localZoneURL  = "https://" + localZoneHost
	localRootCA   = "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----"
)

type caCase struct {
	name   string
	withCA bool
}

// caCases returns the two shapes of the local values, without and with
// KEYCARD_LOCAL_CA_FILE.
func caCases() []caCase {
	return []caCase{{"no CA file", false}, {"CA file", true}}
}

func localValues(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("../../local/values", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// runValues writes the install and sandbox values files that `sandbox run`
// writes, with the root CA when withCA is set.
func runValues(t *testing.T, withCA bool) (string, string) {
	t.Helper()
	var caPEM []byte
	if withCA {
		caPEM = []byte(localRootCA + "\n")
	}
	install, sandboxYAML, err := sandbox.LocalValues(sandbox.LocalValuesInput{
		ZoneURL: localZoneURL, Gateway: localGateway, CAPEM: caPEM,
	})
	if err != nil {
		t.Fatal(err)
	}
	return writeValues(t, string(install)), writeValues(t, string(sandboxYAML))
}

func renderLocalInstall(t *testing.T, withCA bool) map[string][]byte {
	t.Helper()
	dir, err := filepath.Abs("../../install")
	if err != nil {
		t.Fatal(err)
	}
	install, _ := runValues(t, withCA)
	return renderChart(t, dir, release, localValues(t, "install.yaml"), install)
}

func renderLocalSandbox(t *testing.T, withCA bool) map[string][]byte {
	t.Helper()
	_, local := runValues(t, withCA)
	values := sandboxValues(t, localValues(t, "sandbox.yaml"), local)
	return renderChart(t, sandboxDir(t), sandboxUserID, values...)
}

// TestLocalPodMatchesInstall checks that the two charts render the same pod
// with the local values, with and without a CA file.
func TestLocalPodMatchesInstall(t *testing.T) {
	t.Parallel()
	for _, tc := range caCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertPodsMatch(t, renderLocalInstall(t, tc.withCA), renderLocalSandbox(t, tc.withCA))
		})
	}
}

// TestLocalReachesZone checks what a pod needs to reach a zone on the Docker
// host: the zone's name mapped to the gateway, and a NetworkPolicy rule that
// allows the gateway on 443. The extra roots are rendered, and mounted only
// on the proxy, only with a CA file.
func TestLocalReachesZone(t *testing.T) {
	t.Parallel()
	for _, tc := range caCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			objects := renderLocalInstall(t, tc.withCA)

			var tmpl sandboxTemplate
			decode(t, objects, "SandboxTemplate/"+release, &tmpl)
			spec := tmpl.Spec.PodTemplate.Spec
			wantAliases := []corev1.HostAlias{{IP: localGateway, Hostnames: []string{localZoneHost}}}
			if !reflect.DeepEqual(spec.HostAliases, wantAliases) {
				t.Errorf("hostAliases = %+v, want %+v", spec.HostAliases, wantAliases)
			}
			assertZoneTrust(t, objects, spec, tc.withCA)

			var policy networkingv1.NetworkPolicy
			decode(t, objects, "NetworkPolicy/keycard-sandbox-egress", &policy)
			tcp, https := corev1.ProtocolTCP, intstr.FromInt32(443)
			wantRule := networkingv1.NetworkPolicyEgressRule{
				To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: localGateway + "/32"}}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &https}},
			}
			if len(policy.Spec.Egress) != 3 || !reflect.DeepEqual(policy.Spec.Egress[1], wantRule) {
				t.Errorf("egress rules = %s\nwant the gateway rule second: %s", asJSON(policy.Spec.Egress), asJSON(wantRule))
			}
		})
	}
}

// assertZoneTrust checks that the install chart renders the roots and mounts
// them only on the proxy when withCA is set. Otherwise it checks that the
// chart does not render or mount them.
func assertZoneTrust(t *testing.T, objects map[string][]byte, spec corev1.PodSpec, withCA bool) {
	t.Helper()
	if withCA {
		var trust corev1.ConfigMap
		decode(t, objects, "ConfigMap/egress-zone-trust", &trust)
		if got := strings.TrimSpace(trust.Data["zone-ca.pem"]); got != localRootCA {
			t.Errorf("zone-ca.pem = %q, want %q", got, localRootCA)
		}
	} else if keys(objects)["ConfigMap/egress-zone-trust"] {
		t.Error("ConfigMap/egress-zone-trust is rendered without a CA file")
	}
	mounts := map[string][]corev1.VolumeMount{}
	for _, container := range append(spec.InitContainers, spec.Containers...) {
		for _, mount := range container.VolumeMounts {
			if mount.Name == "zone-trust" {
				mounts[container.Name] = append(mounts[container.Name], mount)
			}
		}
	}
	wantMounts := map[string][]corev1.VolumeMount{}
	if withCA {
		wantMounts["egress-proxy"] = []corev1.VolumeMount{
			{Name: "zone-trust", MountPath: "/etc/keycard-zone-trust", ReadOnly: true},
		}
	}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Errorf("zone-trust mounts = %+v, want %+v", mounts, wantMounts)
	}
}

// TestLocalDocument checks that the document allows the gateway, and that it
// trusts the mounted roots only with a CA file.
func TestLocalDocument(t *testing.T) {
	t.Parallel()
	for _, tc := range caCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			proxy := parseDocument(t, configDocument(t, renderLocalSandbox(t, tc.withCA)))
			for _, problem := range validateDocument(proxy) {
				t.Errorf("proxy document: %s", problem)
			}
			var wantTLS *tlsDoc
			if tc.withCA {
				wantTLS = &tlsDoc{UpstreamCAFiles: []string{"/etc/keycard-zone-trust/zone-ca.pem"}}
			}
			if !reflect.DeepEqual(proxy.TLS, wantTLS) {
				t.Errorf("tls = %+v, want %+v", proxy.TLS, wantTLS)
			}
			if want := []string{localGateway + "/32"}; !reflect.DeepEqual(proxy.Policy.DenyListExceptions, want) {
				t.Errorf("deny_list_exceptions = %v, want %v", proxy.Policy.DenyListExceptions, want)
			}
		})
	}
}

// TestInstallRefusesZoneValues checks that rendering fails on an allowCIDRs
// entry wider than one address or in 169.254.0.0/16, and on a root
// certificate with no ConfigMap to mount it.
func TestInstallRefusesZoneValues(t *testing.T) {
	t.Parallel()
	dir, err := filepath.Abs("../../install")
	if err != nil {
		t.Fatal(err)
	}
	const notSingle = "must be a single address"
	cases := []struct {
		name, values, want string
	}{
		{"prefix wider than one address", `network: {allowCIDRs: ["192.168.97.0/24"]}`, notSingle},
		{"bare address", `network: {allowCIDRs: ["192.168.97.1"]}`, notSingle},
		{"IPv6 /32", `network: {allowCIDRs: ["fd00::/32"]}`, notSingle},
		{"metadata address", `network: {allowCIDRs: ["169.254.169.254/32"]}`, "is in 169.254.0.0/16"},
		{"mapped metadata address", `network: {allowCIDRs: ["::ffff:169.254.169.254/128"]}`, "is in 169.254.0.0/16"},
		{"root with no ConfigMap", `zoneTrust: {certificate: x, configMap: ""}`, "zoneTrust.configMap is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			install, _ := runValues(t, false)
			values := []string{localValues(t, "install.yaml"), install, writeValues(t, tc.values+"\n")}
			_, err := chart.Render(t.Context(), dir, release, "agents", values...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// TestExistingClusterValues renders both charts with the values `sandbox
// run` writes for an existing cluster. The two pods must match, every
// container must run a ghcr.io/keycardai image with the given tag, and the
// proxy's document must list the cluster's ranges.
func TestExistingClusterValues(t *testing.T) {
	t.Parallel()
	ranges := []string{"10.0.0.0/16", "172.20.0.0/16"}
	install, sandboxYAML, err := sandbox.LocalValues(sandbox.LocalValuesInput{
		ZoneURL: localZoneURL, ImageTag: "0.2.0", ClusterCIDRs: ranges,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs("../../install")
	if err != nil {
		t.Fatal(err)
	}
	installObjects := renderChart(t, dir, release, localValues(t, "install.yaml"), writeValues(t, string(install)))
	sandboxObjects := renderChart(t, sandboxDir(t), sandboxUserID,
		sandboxValues(t, localValues(t, "sandbox.yaml"), writeValues(t, string(sandboxYAML)))...)
	assertPodsMatch(t, installObjects, sandboxObjects)

	var tmpl sandboxTemplate
	decode(t, installObjects, "SandboxTemplate/"+release, &tmpl)
	spec := tmpl.Spec.PodTemplate.Spec
	for _, container := range append(spec.InitContainers, spec.Containers...) {
		if !strings.HasPrefix(container.Image, "ghcr.io/keycardai/") || !strings.HasSuffix(container.Image, ":0.2.0") {
			t.Errorf("container %s image = %q, want ghcr.io/keycardai/...:0.2.0", container.Name, container.Image)
		}
	}
	proxy := parseDocument(t, configDocument(t, sandboxObjects))
	if !reflect.DeepEqual(proxy.Policy.ClusterCIDRs, ranges) {
		t.Errorf("cluster_cidrs = %v, want %v", proxy.Policy.ClusterCIDRs, ranges)
	}
}
