package chart_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/keycardai/agent-sandbox-poc/internal/chart"
)

// local/run.sh renders both charts with the files under local/values and one
// more file per chart that it writes from the running cluster. laneInstall
// and laneSandbox copy the keys run.sh writes. Update them when run.sh
// changes.
const (
	laneGateway = "192.168.97.1"
	laneZoneURL = "https://kx7z9v2m4q8w1r6t3y5u7i9o0p.localdev.keycard.sh"
	laneRootCA  = "-----BEGIN CERTIFICATE-----\nZmFrZQ==\n-----END CERTIFICATE-----"
	laneInstall = `zoneURL: ` + laneZoneURL + `
zoneTrust:
  certificate: |
    -----BEGIN CERTIFICATE-----
    ZmFrZQ==
    -----END CERTIFICATE-----
hostAliases:
  - ip: ` + laneGateway + `
    hostnames: [kx7z9v2m4q8w1r6t3y5u7i9o0p.localdev.keycard.sh]
network:
  allowCIDRs: ["` + laneGateway + `/32"]
`
	laneSandbox = `zoneURL: ` + laneZoneURL + `
hostAliases:
  - ip: ` + laneGateway + `
    hostnames: [kx7z9v2m4q8w1r6t3y5u7i9o0p.localdev.keycard.sh]
policy:
  denyListExceptions: ["` + laneGateway + `/32"]
`
)

func laneValues(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("../../local/values", name))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func renderLaneInstall(t *testing.T) map[string][]byte {
	t.Helper()
	dir, err := filepath.Abs("../../install")
	if err != nil {
		t.Fatal(err)
	}
	return renderChart(t, dir, release, laneValues(t, "install.yaml"), writeValues(t, laneInstall))
}

func renderLaneSandbox(t *testing.T) map[string][]byte {
	t.Helper()
	values := sandboxValues(t, laneValues(t, "sandbox.yaml"), writeValues(t, laneSandbox))
	return renderChart(t, sandboxDir(t), sandboxUserID, values...)
}

// TestLanePodMatchesInstall checks that the two charts render the same pod
// with the local values, which add the zone's host alias and trust.
func TestLanePodMatchesInstall(t *testing.T) {
	t.Parallel()
	assertPodsMatch(t, renderLaneInstall(t), renderLaneSandbox(t))
}

// TestLaneReachesZone checks what a pod needs to reach a zone on the Docker
// host: the zone's name mapped to the gateway, the localdev root mounted only
// on the proxy, and a NetworkPolicy rule that allows the gateway on 443.
func TestLaneReachesZone(t *testing.T) {
	t.Parallel()
	objects := renderLaneInstall(t)

	var trust corev1.ConfigMap
	decode(t, objects, "ConfigMap/egress-zone-trust", &trust)
	if got := strings.TrimSpace(trust.Data["zone-ca.pem"]); got != laneRootCA {
		t.Errorf("zone-ca.pem = %q, want %q", got, laneRootCA)
	}

	var tmpl sandboxTemplate
	decode(t, objects, "SandboxTemplate/"+release, &tmpl)
	spec := tmpl.Spec.PodTemplate.Spec
	wantAliases := []corev1.HostAlias{
		{IP: laneGateway, Hostnames: []string{"kx7z9v2m4q8w1r6t3y5u7i9o0p.localdev.keycard.sh"}},
	}
	if !reflect.DeepEqual(spec.HostAliases, wantAliases) {
		t.Errorf("hostAliases = %+v, want %+v", spec.HostAliases, wantAliases)
	}
	mounts := map[string][]corev1.VolumeMount{}
	for _, container := range append(spec.InitContainers, spec.Containers...) {
		for _, mount := range container.VolumeMounts {
			if mount.Name == "zone-trust" {
				mounts[container.Name] = append(mounts[container.Name], mount)
			}
		}
	}
	wantMounts := map[string][]corev1.VolumeMount{
		"egress-proxy": {{Name: "zone-trust", MountPath: "/etc/keycard-zone-trust", ReadOnly: true}},
	}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Errorf("zone-trust mounts = %+v, want %+v", mounts, wantMounts)
	}

	var policy networkingv1.NetworkPolicy
	decode(t, objects, "NetworkPolicy/keycard-sandbox-egress", &policy)
	tcp, https := corev1.ProtocolTCP, intstr.FromInt32(443)
	wantRule := networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: laneGateway + "/32"}}},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &https}},
	}
	if len(policy.Spec.Egress) != 3 || !reflect.DeepEqual(policy.Spec.Egress[1], wantRule) {
		t.Errorf("egress rules = %s\nwant the gateway rule second: %s", asJSON(policy.Spec.Egress), asJSON(wantRule))
	}
}

// TestLaneDocuments checks that both documents trust the mounted root and
// allow the gateway, and that they differ only in the identity section.
func TestLaneDocuments(t *testing.T) {
	t.Parallel()
	objects := renderLaneSandbox(t)
	proxy := parseDocument(t, configDocument(t, objects, proxyConfigMap))
	agent := parseDocument(t, configDocument(t, objects, agentConfigMap))
	for _, problem := range validateDocument(proxy) {
		t.Errorf("proxy document: %s", problem)
	}
	wantTLS := &tlsDoc{UpstreamCAFiles: []string{"/etc/keycard-zone-trust/zone-ca.pem"}}
	if !reflect.DeepEqual(proxy.TLS, wantTLS) {
		t.Errorf("tls = %+v, want %+v", proxy.TLS, wantTLS)
	}
	if want := []string{laneGateway + "/32"}; !reflect.DeepEqual(proxy.Policy.DenyListExceptions, want) {
		t.Errorf("deny_list_exceptions = %v, want %v", proxy.Policy.DenyListExceptions, want)
	}
	proxy.Identity = nil
	if !reflect.DeepEqual(agent, proxy) {
		t.Errorf("agent document = %s\nwant the proxy's without identity: %s", asJSON(agent), asJSON(proxy))
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
			values := []string{
				laneValues(t, "install.yaml"), writeValues(t, laneInstall), writeValues(t, tc.values+"\n"),
			}
			_, err := chart.Render(t.Context(), dir, release, "agents", values...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}
