package preflight_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/keycardai/agent-sandbox-poc/internal/preflight"
)

const (
	pod = "apiVersion: v1\nkind: Pod\nmetadata:\n  name: agent\nspec:\n" +
		"  containers:\n    - name: agent\n      image: agent\n"
	sandbox = "apiVersion: agents.x-k8s.io/v1beta1\nkind: Sandbox\nmetadata:\n  name: sbx\nspec:\n"
	// template is a SandboxTemplate header. Its spec keys follow.
	template = "apiVersion: extensions.agents.x-k8s.io/v1beta1\nkind: SandboxTemplate\n" +
		"metadata:\n  name: tpl\nspec:\n"
	// podTemplate is a pod template with one volume, indented under spec.
	// jsonPod is a Pod as one JSON object.
	jsonPod = `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"agent"},` +
		`"spec":{"containers":[{"name":"agent","image":"agent"}]}}`
	// jsonDeployment is a privileged Deployment as one JSON object.
	jsonDeployment = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"evil"},` +
		`"spec":{"template":{"spec":{"containers":[{"name":"evil","securityContext":{"privileged":true}}]}}}}`
	podTemplate = "  podTemplate:\n    spec:\n      containers:\n        - name: agent\n          image: agent\n" +
		"      volumes:\n        - name: shared-ca\n          emptyDir: {}\n"
)

func TestCheckRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want preflight.Rule
		yaml string
	}{
		{"env Allowed", preflight.RuleEnvInjection, template + "  envVarsInjectionPolicy: Allowed\n" + podTemplate},
		{"env Overrides", preflight.RuleEnvInjection, template + "  envVarsInjectionPolicy: Overrides\n" + podTemplate},
		{"a Pod with no spec", preflight.RuleNoPodSpec, "apiVersion: v1\nkind: Pod\n"},
		{
			"nothing judged beside empty and null documents", preflight.RuleNothingJudged,
			"---\n# a comment\n---\nnull\n---\napiVersion: v1\nkind: ConfigMap\n---\n",
		},
		{
			"a ConfigMap that carries list items", preflight.RuleUnknownField,
			pod + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings}\nitems:\n  - apiVersion: apps/v1\n" +
				"    kind: Deployment\n    metadata: {name: evil}\n" +
				"    spec: {template: {spec: {containers: [{name: evil, image: busybox, securityContext: {privileged: true}}]}}}\n",
		},
		{
			"a Sandbox field that only a SandboxTemplate has", preflight.RuleUnknownField,
			sandbox + "  envVarsInjectionPolicy: Disallowed\n" + podTemplate,
		},
		{
			"a nested key that differs from a field only in case", preflight.RuleUnknownField,
			pod + "      securityContext:\n        Privileged: true\n",
		},
		{"a JSON Pod and a JSON Deployment", preflight.RuleUnknownKind, jsonPod + "\n" + jsonDeployment},
		{"a JSON Pod and an adjacent JSON Deployment", preflight.RuleUnknownKind, jsonPod + jsonDeployment},
		{"a List", preflight.RuleUnknownKind, "apiVersion: v1\nkind: List\nitems:\n  - apiVersion: v1\n    kind: Pod\n"},
		{"a Pod claiming another apiVersion", preflight.RuleUnknownKind, "apiVersion: example.com/v1\nkind: Pod\n"},
		{"a custom resource", preflight.RuleUnknownKind, "apiVersion: example.com/v1\nkind: Widget\n"},
		{
			"a privileged Deployment", preflight.RuleUnknownKind,
			"apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n" +
				"        - name: a\n          securityContext:\n            privileged: true\n",
		},
		{
			"a Deployment after a CR in a separator comment", preflight.RuleUnreadableDocument,
			pod + "--- # split\r{apiVersion: apps/v1, kind: Deployment, metadata: {name: evil}}\n",
		},
		{
			"a Deployment after a NEL in a separator comment", preflight.RuleUnreadableDocument,
			pod + "--- # split\u0085{apiVersion: apps/v1, kind: Deployment, metadata: {name: evil}}\n",
		},
		{
			"a Deployment after a line separator in a separator comment", preflight.RuleUnreadableDocument,
			pod + "--- # split\u2028{apiVersion: apps/v1, kind: Deployment, metadata: {name: evil}}\n",
		},
		{
			"a Deployment after a paragraph separator in a separator comment", preflight.RuleUnreadableDocument,
			pod + "--- # split\u2029{apiVersion: apps/v1, kind: Deployment, metadata: {name: evil}}\n",
		},
		{
			"a Deployment after an end marker", preflight.RuleUnreadableDocument,
			pod + "...\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: evil\n",
		},
		{
			"a Deployment behind bare CR line endings", preflight.RuleUnreadableDocument,
			strings.ReplaceAll(pod+"---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: evil\n", "\n", "\r"),
		},
		{
			"a JSON Pod and a truncated JSON object", preflight.RuleUnreadableDocument,
			jsonPod + `{"apiVersion":"v1","kind":`,
		},
		{
			"a JSON Pod and an adjacent JSON Deployment after a comment", preflight.RuleUnreadableDocument,
			"# rendered\n" + jsonPod + jsonDeployment,
		},
		{
			"a JSON object that repeats a key", preflight.RuleUnreadableDocument,
			`{"kind":"ConfigMap","apiVersion":"v1","kind":"ConfigMap"}`,
		},
		{
			"a YAML document that repeats a key", preflight.RuleUnreadableDocument,
			"apiVersion: v1\nkind: Pod\nkind: ConfigMap\n",
		},
		{"a document that is not YAML", preflight.RuleUnreadableDocument, "kind: Pod\n  name: [unclosed\n"},
		{
			"a key that is not a string",
			preflight.RuleUnreadableDocument,
			pod + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {0777: b}\n",
		},
		{"a key that kubectl's reader takes for a separator", preflight.RuleUnreadableDocument, pod + "---#hidden: true\n"},
		{
			"a merge key",
			preflight.RuleUnreadableDocument,
			pod + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {<<: {a: b}}\n",
		},
		{
			"a number key", preflight.RuleUnreadableDocument,
			pod + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {42: b}\n",
		},
		{
			"an anchor and an alias",
			preflight.RuleUnreadableDocument,
			pod + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {a: &x b, c: *x}\n",
		},
		{
			"an anchored key", preflight.RuleUnreadableDocument,
			pod + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata: {&x a: b}\n",
		},
		{"an anchored null document", preflight.RuleUnreadableDocument, pod + "---\n&x null\n"},
		{
			"a volume claim template that swaps a pod volume", preflight.RuleVolumeClaimTemplates,
			sandbox + podTemplate + "  volumeClaimTemplates:\n    - metadata:\n        name: shared-ca\n" +
				"      spec:\n        accessModes: [ReadWriteOnce]\n",
		},
		{
			"a volumeClaimTemplatesPolicy other than Disallowed", preflight.RuleVolumeClaimTemplates,
			template + "  volumeClaimTemplatesPolicy: Allowed\n" + podTemplate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			report := preflight.Check([]byte(tt.yaml))
			if len(report.Refusals) != 1 || report.Refusals[0].Rule != tt.want {
				t.Errorf("refusals = %+v, want exactly one %s", report.Refusals, tt.want)
			}
		})
	}
}

func TestCheckPassesAJSONPod(t *testing.T) {
	t.Parallel()
	if report := preflight.Check([]byte(jsonPod)); !report.OK() {
		t.Errorf("refusals = %+v, want none", report.Refusals)
	}
}

func TestCheckPassesAllowlistedKindsAsUnjudged(t *testing.T) {
	t.Parallel()
	manifest := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ns\n---\n" +
		"apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: sa\n---\n" +
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\ndata:\n  key: value\nimmutable: yes\n---\n" +
		"apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\nmetadata:\n  name: np\nspec: {}\n---\n" +
		"apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: role\n---\n" +
		"apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata:\n  name: rb\n---\n" +
		pod + "---\n" +
		sandbox + podTemplate + "  volumeClaimTemplates:\n    - metadata:\n        name: workspace\n" +
		"      spec:\n        accessModes: [ReadWriteOnce]\n---\n" +
		template + "  envVarsInjectionPolicy: Disallowed\n  volumeClaimTemplatesPolicy: Disallowed\n" + podTemplate
	report := preflight.Check([]byte(manifest))
	if !report.OK() {
		t.Errorf("refusals = %+v, want none", report.Refusals)
	}
	got := make([]string, 0, len(report.Unjudged))
	for _, object := range report.Unjudged {
		got = append(got, object.Kind)
	}
	want := []string{"Namespace", "ServiceAccount", "ConfigMap", "NetworkPolicy", "Role", "RoleBinding"}
	if !slices.Equal(got, want) {
		t.Errorf("unjudged kinds = %v, want %v", got, want)
	}
}
