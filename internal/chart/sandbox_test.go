package chart_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/keycardai/agent-sandbox-poc/internal/chart"
)

// The user of the rendered sandbox. The ServiceAccount, the Helm release and
// the Sandbox all take the user's public id as their name.
const (
	sandboxUserID    = "igmua2g0fwapl21qll7tczdw7t"
	sandboxUserEmail = "someone@example.com"
	sandboxClientID  = "cred_01hzx"
	proxyConfigMap   = sandboxUserID + "-proxy-config"
	agentConfigMap   = sandboxUserID + "-agent-config"
	// The pod volumes that read the two ConfigMaps.
	egressConfig = "egress-config"
	agentConfig  = "agent-config"
)

// sandboxObject is the part of an agents.x-k8s.io/v1beta1 Sandbox that the
// sandbox chart renders.
type sandboxObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`

	Spec struct {
		Service     *bool          `json:"service"`
		PodTemplate map[string]any `json:"podTemplate"`
	} `json:"spec"`
}

func writeValues(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sandboxValues are the files new-sandbox passes to helm: the admin's example
// values, then the zone-setup outputs for one user, then any extra file.
func sandboxValues(t *testing.T, extra ...string) []string {
	t.Helper()
	generated := writeValues(t, `userID: `+sandboxUserID+`
userEmail: `+sandboxUserEmail+`
serviceAccount: system:serviceaccount:agents:`+sandboxUserID+`
clientID: `+sandboxClientID+`
providerID: prv_01hzx
applicationID: app_01hzx
applicationIdentifier: urn:keycard:sandbox-proxy:agents
`)
	return append([]string{filepath.Join(sandboxDir(t), "values.example.yaml"), generated}, extra...)
}

func sandboxDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../../sandbox")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func renderSandbox(t *testing.T) map[string][]byte {
	t.Helper()
	return renderChart(t, sandboxDir(t), sandboxUserID, sandboxValues(t)...)
}

// TestSandbox checks the objects one sandbox renders to, with their namespace,
// labels and annotations.
func TestSandbox(t *testing.T) {
	t.Parallel()
	objects := renderSandbox(t)
	want := map[string]bool{
		"ConfigMap/" + agentConfigMap:     true,
		"ConfigMap/" + proxyConfigMap:     true,
		"Sandbox/" + sandboxUserID:        true,
		"ServiceAccount/" + sandboxUserID: true,
	}
	if got := keys(objects); !reflect.DeepEqual(got, want) {
		t.Errorf("objects = %v, want %v", got, want)
	}
	for key, doc := range objects {
		var meta metav1.PartialObjectMetadata
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Namespace != "agents" {
			t.Errorf("%s is in namespace %q, want agents", key, meta.Namespace)
		}
		if got := meta.Labels["keycard.ai/user-id"]; got != sandboxUserID {
			t.Errorf("%s user-id label = %q, want %q", key, got, sandboxUserID)
		}
		for name, want := range map[string]string{
			"keycard.ai/client-id":  sandboxClientID,
			"keycard.ai/user-email": sandboxUserEmail,
		} {
			if got := meta.Annotations[name]; got != want {
				t.Errorf("%s annotation %s = %q, want %q", key, name, got, want)
			}
		}
	}
}

// TestSandboxIdentity checks that the ServiceAccount does not automount a
// token and that the Sandbox does not get a Service.
func TestSandboxIdentity(t *testing.T) {
	t.Parallel()
	objects := renderSandbox(t)
	var account corev1.ServiceAccount
	decode(t, objects, "ServiceAccount/"+sandboxUserID, &account)
	if account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken {
		t.Error("the ServiceAccount does not set automountServiceAccountToken: false")
	}

	var box sandboxObject
	decode(t, objects, "Sandbox/"+sandboxUserID, &box)
	if box.APIVersion != "agents.x-k8s.io/v1beta1" {
		t.Errorf("Sandbox apiVersion = %q, want agents.x-k8s.io/v1beta1", box.APIVersion)
	}
	if box.Spec.Service == nil || *box.Spec.Service {
		t.Error("the Sandbox does not set service: false")
	}
}

// TestSandboxRefuses checks that rendering fails without a zone URL, a client
// id or a user id, and with a serviceAccount other than the subject the chart
// binds.
func TestSandboxRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		values string
		want   string
	}{
		{"no zone URL", `zoneURL: ""`, "zoneURL is required"},
		{"no client id", `clientID: ""`, "clientID is required"},
		{"no user id", `userID: ""`, "userID is required"},
		{
			"subject from another namespace",
			"serviceAccount: system:serviceaccount:other:" + sandboxUserID,
			"this sandbox binds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := sandboxValues(t, writeValues(t, tc.values+"\n"))
			_, err := chart.Render(t.Context(), sandboxDir(t), sandboxUserID, "agents", files...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// TestSandboxPodMatchesInstall checks that the sandbox chart renders the pod
// of the install chart's SandboxTemplate, apart from the user's
// ServiceAccount and the two ConfigMap names. The install chart's tests check
// each field of the template, so they cover the sandbox chart's pod too.
func TestSandboxPodMatchesInstall(t *testing.T) {
	t.Parallel()
	assertPodsMatch(t, render(t), renderSandbox(t))
}

// assertPodsMatch compares the install chart's SandboxTemplate pod with the
// sandbox chart's Sandbox pod after the three per-sandbox substitutions.
func assertPodsMatch(t *testing.T, install, sandbox map[string][]byte) {
	t.Helper()
	var tmpl struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata"`

		Spec struct {
			NetworkPolicyManagement string         `json:"networkPolicyManagement"`
			PodTemplate             map[string]any `json:"podTemplate"`
		} `json:"spec"`
	}
	decode(t, install, "SandboxTemplate/"+release, &tmpl)
	want := tmpl.Spec.PodTemplate
	spec, ok := want["spec"].(map[string]any)
	if !ok {
		t.Fatalf("the install pod template has no spec: %v", want)
	}
	spec["serviceAccountName"] = sandboxUserID
	perSandbox := map[string]string{egressConfig: proxyConfigMap, agentConfig: agentConfigMap}
	rewired := 0
	volumes, _ := spec["volumes"].([]any)
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		volumeName, _ := volume["name"].(string)
		source, isConfigMap := volume["configMap"].(map[string]any)
		if name, wanted := perSandbox[volumeName]; isConfigMap && wanted {
			source["name"] = name
			rewired++
		}
	}
	if rewired != 2 {
		t.Fatalf("rewired %d ConfigMap volumes in the install pod, want 2", rewired)
	}

	var box sandboxObject
	decode(t, sandbox, "Sandbox/"+sandboxUserID, &box)
	if !reflect.DeepEqual(box.Spec.PodTemplate, want) {
		t.Errorf("the sandbox pod template differs from the install chart's:\ngot  %s\nwant %s",
			asJSON(box.Spec.PodTemplate), asJSON(want))
	}
}

// configDocument returns the document in a ConfigMap, under the file name in
// the proxy's --config argument.
func configDocument(t *testing.T, objects map[string][]byte, name string) string {
	t.Helper()
	var configMap corev1.ConfigMap
	decode(t, objects, "ConfigMap/"+name, &configMap)
	doc, ok := configMap.Data["config.yaml"]
	if !ok {
		t.Fatalf("ConfigMap %s has no config.yaml: %v", name, configMap.Data)
	}
	return doc
}

// TestSandboxDocumentValid checks both rendered documents against the copy of
// the proxy's rules in document_test.go.
func TestSandboxDocumentValid(t *testing.T) {
	t.Parallel()
	objects := renderSandbox(t)
	for _, name := range []string{proxyConfigMap, agentConfigMap} {
		for _, problem := range validateDocument(parseDocument(t, configDocument(t, objects, name))) {
			t.Errorf("%s: %s", name, problem)
		}
	}
}

// TestSandboxDocuments checks the proxy's document, and checks that the
// agent's copy is the same document without the identity section.
func TestSandboxDocuments(t *testing.T) {
	t.Parallel()
	objects := renderSandbox(t)
	proxy := parseDocument(t, configDocument(t, objects, proxyConfigMap))
	hosts := make([]string, 0, len(proxy.Endpoints))
	for _, e := range proxy.Endpoints {
		hosts = append(hosts, e.Host)
	}
	if want := []string{
		"api.github.com", "github.com", "openrouter.ai", "pypi.org", "files.pythonhosted.org",
	}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("endpoint hosts = %v, want %v", hosts, want)
	}
	got := proxy
	got.Endpoints = nil
	want := document{
		Version: 1,
		Sandbox: sandboxID{ID: sandboxUserID},
		//nolint:gosec // G101: a public client id and a file path, not a secret.
		Identity: &identity{
			ZoneURL:           "https://acme.keycard.cloud",
			ClientID:          sandboxClientID,
			WorkloadTokenFile: "/var/run/secrets/keycard/token",
		},
		Policy: policyDoc{ClusterCIDRs: []string{"10.0.0.0/16", "172.20.0.0/16"}, DenyListExceptions: []string{}},
	}
	want.Identity.Impersonate.User = sandboxUserID
	if !reflect.DeepEqual(got, want) {
		t.Errorf("proxy document = %s\nwant %s", asJSON(got), asJSON(want))
	}

	agentRaw := configDocument(t, objects, agentConfigMap)
	if strings.Contains(agentRaw, sandboxClientID) {
		t.Error("the agent's document names the workload-identity credential")
	}
	agent := parseDocument(t, agentRaw)
	proxy.Identity = nil
	if !reflect.DeepEqual(agent, proxy) {
		t.Errorf("agent document = %s\nwant the proxy's without identity: %s", asJSON(agent), asJSON(proxy))
	}
}
