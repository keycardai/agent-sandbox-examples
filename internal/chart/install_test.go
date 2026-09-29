package chart_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/keycardai/agent-sandbox-poc/internal/chart"
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
	docs, err := chart.Render(t.Context(), dir, release, values...)
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
		"Namespace/agents":                  true,
		"Role/keycard-sandbox-admin":        true,
		"RoleBinding/keycard-sandbox-admin": true,
	}
	if got := keys(objects); !reflect.DeepEqual(got, want) {
		t.Errorf("objects = %v, want %v", got, want)
	}

	var ns metav1.PartialObjectMetadata
	decode(t, objects, "Namespace/agents", &ns)
	for _, mode := range []string{"enforce", "audit", "warn"} {
		if got := ns.Labels["pod-security.kubernetes.io/"+mode]; got != "privileged" {
			t.Errorf("pod-security %s = %q, want privileged", mode, got)
		}
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
		{APIGroups: []string{""}, Resources: []string{"pods", "pods/log"}, Verbs: read},
		{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{create, get}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Errorf("role rules = %+v\nwant %+v", role.Rules, wantRules)
	}

	noSubjects := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(noSubjects, []byte("rbac:\n  subjects: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := render(t, noSubjects)["RoleBinding/keycard-sandbox-admin"]; ok {
		t.Error("a RoleBinding is rendered with no subjects")
	}
}

// A namespace that is not a DNS label fails the render, so a value cannot
// inject fields into the manifests or defer the error to install time.
func TestInstallRefusesBadNamespace(t *testing.T) {
	t.Parallel()
	dir, err := filepath.Abs("../../install")
	if err != nil {
		t.Fatal(err)
	}
	for name, namespace := range map[string]string{
		"underscore": "bad_name",
		"injection":  `agents\n  annotations:\n    attacker.example/mark: injected`,
	} {
		values := filepath.Join(t.TempDir(), name+".yaml")
		if err := os.WriteFile(values, []byte("namespace: \""+namespace+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := chart.Render(t.Context(), dir, release, values)
		if err == nil || !strings.Contains(err.Error(), "must be a DNS label") {
			t.Errorf("%s: err = %v, want a render failure naming the DNS label rule", name, err)
		}
	}
}
