package chart_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/keycardai/agent-sandbox-examples/internal/chart"
)

const release = "keycard-sandbox"

// render renders the install chart with the example values and any extra
// values files, keyed by "Kind/name".
func render(t *testing.T, extraValues ...string) map[string][]byte {
	t.Helper()
	dir, err := filepath.Abs("../../install")
	if err != nil {
		t.Fatal(err)
	}
	values := append([]string{filepath.Join(dir, "values.example.yaml")}, extraValues...)
	return renderChart(t, dir, release, values...)
}

// renderChart renders the chart at dir into the agents namespace, keyed by
// "Kind/name".
func renderChart(t *testing.T, dir, release string, values ...string) map[string][]byte {
	t.Helper()
	docs, err := chart.Render(t.Context(), dir, release, "agents", values...)
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string][]byte{}
	for _, doc := range docs {
		var meta metav1.PartialObjectMetadata
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatal(err)
		}
		objects[meta.Kind+"/"+meta.Name] = doc
	}
	return objects
}

func decode(t *testing.T, objects map[string][]byte, key string, into any) {
	t.Helper()
	doc, ok := objects[key]
	if !ok {
		t.Fatalf("no %s rendered", key)
	}
	if err := yaml.UnmarshalStrict(doc, into); err != nil {
		t.Fatalf("decoding %s: %v", key, err)
	}
}

func keys(objects map[string][]byte) map[string]bool {
	out := map[string]bool{}
	for key := range objects {
		out[key] = true
	}
	return out
}

func TestInstall(t *testing.T) {
	t.Parallel()
	objects := render(t)
	want := map[string]bool{
		"Namespace/agents":                     true,
		"NetworkPolicy/keycard-sandbox-egress": true,
		"Role/keycard-sandbox-admin":           true,
		"RoleBinding/keycard-sandbox-admin":    true,
		"SandboxTemplate/keycard-sandbox":      true,
	}
	if got := keys(objects); !reflect.DeepEqual(got, want) {
		t.Errorf("objects = %v, want %v", got, want)
	}

	var namespace metav1.PartialObjectMetadata
	decode(t, objects, "Namespace/agents", &namespace)
	for _, mode := range []string{"enforce", "audit", "warn"} {
		if got := namespace.Labels["pod-security.kubernetes.io/"+mode]; got != "privileged" {
			t.Errorf("pod-security %s = %q, want privileged", mode, got)
		}
	}
	if got := namespace.Annotations["helm.sh/resource-policy"]; got != "keep" {
		t.Errorf("resource-policy = %q, want keep", got)
	}

	var role rbacv1.Role
	decode(t, objects, "Role/keycard-sandbox-admin", &role)
	const get, create = "get", "create"
	crud := []string{get, "list", "watch", create, "update", "patch", "delete"}
	read := []string{get, "list", "watch"}
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{"agents.x-k8s.io"}, Resources: []string{"sandboxes"}, Verbs: crud},
		{APIGroups: []string{"extensions.agents.x-k8s.io"}, Resources: []string{"sandboxtemplates"}, Verbs: read},
		{APIGroups: []string{""}, Resources: []string{"serviceaccounts", "configmaps"}, Verbs: crud},
		{APIGroups: []string{""}, Resources: []string{"pods", "pods/log", "events"}, Verbs: read},
		{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{create, get}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Errorf("role rules = %+v\nwant %+v", role.Rules, wantRules)
	}

	var policy networkingv1.NetworkPolicy
	decode(t, objects, "NetworkPolicy/keycard-sandbox-egress", &policy)
	if got := policy.Annotations["helm.sh/resource-policy"]; got != "keep" {
		t.Errorf("network policy resource-policy = %q, want keep, so sandboxes that outlive the release stay confined", got)
	}
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	https, dns := intstr.FromInt32(443), intstr.FromInt32(53)
	wantSpec := networkingv1.NetworkPolicySpec{
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		Egress: []networkingv1.NetworkPolicyEgressRule{
			{
				To: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{
						"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
						"100.64.0.0/10", "127.0.0.0/8", "0.0.0.0/8",
					}}},
					{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{
						"fc00::/7", "fe80::/10", "::1/128", "::/128",
						"100::/64", "64:ff9b::/96", "64:ff9b:1::/48",
					}}},
				},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &https}},
			},
			{
				To: []networkingv1.NetworkPolicyPeer{
					{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"},
						},
						PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					},
				},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
			},
		},
	}
	if !reflect.DeepEqual(policy.Spec, wantSpec) {
		t.Errorf("network policy spec = %+v\nwant %+v", policy.Spec, wantSpec)
	}

	noSubjects := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(noSubjects, []byte("rbac:\n  subjects: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := render(t, noSubjects)["RoleBinding/keycard-sandbox-admin"]; ok {
		t.Error("a RoleBinding is rendered with no subjects")
	}
}

func TestInstallClusterCIDRs(t *testing.T) {
	t.Parallel()
	clusterCIDRs := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(
		clusterCIDRs,
		[]byte("network:\n  clusterCIDRs: [198.18.0.0/15, \"2001:db8:42::/56\"]\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	var policy networkingv1.NetworkPolicy
	decode(t, render(t, clusterCIDRs), "NetworkPolicy/keycard-sandbox-egress", &policy)
	for i, want := range []string{"198.18.0.0/15", "2001:db8:42::/56"} {
		block := policy.Spec.Egress[0].To[i].IPBlock
		if got := block.Except[len(block.Except)-1]; got != want {
			t.Errorf("%s except ends with %q, want %q", block.CIDR, got, want)
		}
	}
}

// A namespace that is not a DNS label fails the render, so a release
// namespace cannot inject fields into the manifests or defer the error to
// install time.
func TestInstallRefusesBadNamespace(t *testing.T) {
	t.Parallel()
	dir, err := filepath.Abs("../../install")
	if err != nil {
		t.Fatal(err)
	}
	for name, namespace := range map[string]string{
		"underscore": "bad_name",
		"injection":  "agents\n  annotations:\n    attacker.example/mark: injected",
	} {
		_, err := chart.Render(t.Context(), dir, release, namespace)
		if err == nil || !strings.Contains(err.Error(), "must be a DNS label") {
			t.Errorf("%s: err = %v, want a render failure naming the DNS label rule", name, err)
		}
	}
}
