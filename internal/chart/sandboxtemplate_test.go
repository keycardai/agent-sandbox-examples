package chart_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/keycardai/agent-sandbox-examples/internal/chart"
)

// sandboxTemplate is the part of an extensions.agents.x-k8s.io/v1beta1
// SandboxTemplate that the install chart renders.
type sandboxTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`

	Spec struct {
		NetworkPolicyManagement string `json:"networkPolicyManagement"`
		PodTemplate             struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
			Spec     corev1.PodSpec    `json:"spec"`
		} `json:"podTemplate"`
	} `json:"spec"`
}

func asJSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(out)
}

// TestInstallSandboxTemplate renders the chart with the example values and
// compares every field of the reference pod. A change to any security setting
// fails it.
func TestInstallSandboxTemplate(t *testing.T) {
	t.Parallel()
	var tmpl sandboxTemplate
	const disabled, sharedCA, egressRun = "disabled", "shared-ca", "egress-run"
	probe := func(route string) *corev1.ExecAction {
		return &corev1.ExecAction{Command: []string{"/ko-app/egress-proxy", "probe", route}}
	}
	decode(t, render(t), "SandboxTemplate/keycard-sandbox", &tmpl)

	if got := tmpl.APIVersion; got != "extensions.agents.x-k8s.io/v1beta1" {
		t.Errorf("apiVersion = %q, want extensions.agents.x-k8s.io/v1beta1", got)
	}
	if got := tmpl.Spec.NetworkPolicyManagement; got != "Unmanaged" {
		t.Errorf("networkPolicyManagement = %q, want Unmanaged, so agent-sandbox does not add a policy", got)
	}
	wantAnnotations := map[string]string{
		"sidecar.istio.io/inject":                "false",
		"linkerd.io/inject":                      disabled,
		"kuma.io/sidecar-injection":              disabled,
		"appmesh.k8s.aws/sidecarInjectorWebhook": disabled,
	}
	if got := tmpl.Spec.PodTemplate.Metadata.Annotations; !reflect.DeepEqual(got, wantAnnotations) {
		t.Errorf("pod annotations = %v\nwant %v", got, wantAnnotations)
	}

	spec := tmpl.Spec.PodTemplate.Spec
	podLevel := spec
	podLevel.InitContainers, podLevel.Containers, podLevel.Volumes = nil, nil, nil
	runtimeDefault := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	wantPodLevel := corev1.PodSpec{
		ServiceAccountName:           release,
		AutomountServiceAccountToken: new(false),
		ShareProcessNamespace:        new(false),
		RestartPolicy:                corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: runtimeDefault,
			Sysctls:        []corev1.Sysctl{{Name: "net.ipv4.ip_unprivileged_port_start", Value: "1024"}},
		},
	}
	if !reflect.DeepEqual(podLevel, wantPodLevel) {
		t.Errorf("pod-level fields = %s\nwant %s", asJSON(podLevel), asJSON(wantPodLevel))
	}

	dropAll := []corev1.Capability{"ALL"}
	wantInit := []corev1.Container{
		{
			Name:  "egress-netsetup",
			Image: "ghcr.io/keycardai/egress-netsetup:v0.2.0",
			Args: []string{
				"--proxy-uid=1337", "--redirect-port=1015", "--dns-port=53", "--capture-ports=80,443",
				"--ipv6=auto", "--receipt=/run/egress/netsetup/netsetup.json",
			},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:                new(int64(0)),
				RunAsNonRoot:             new(false),
				AllowPrivilegeEscalation: new(false),
				ReadOnlyRootFilesystem:   new(true),
				Capabilities: &corev1.Capabilities{
					Drop: dropAll,
					Add:  []corev1.Capability{"NET_ADMIN", "NET_RAW", "SETPCAP"},
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "xtables-run", MountPath: "/run"},
				{Name: egressRun, MountPath: "/run/egress"},
			},
		},
		{
			Name:          "egress-proxy",
			Image:         "ghcr.io/keycardai/egress-proxy:v0.2.0",
			RestartPolicy: new(corev1.ContainerRestartPolicyAlways),
			Args:          []string{"--config=/etc/egress/config.yaml"},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:                new(int64(1337)),
				RunAsGroup:               new(int64(1337)),
				RunAsNonRoot:             new(true),
				AllowPrivilegeEscalation: new(false),
				ReadOnlyRootFilesystem:   new(true),
				Capabilities: &corev1.Capabilities{
					Drop: dropAll,
					Add:  []corev1.Capability{"NET_BIND_SERVICE"},
				},
			},
			StartupProbe: &corev1.Probe{
				Exec:             probe("readyz"),
				PeriodSeconds:    1,
				FailureThreshold: 60,
			},
			LivenessProbe: &corev1.Probe{
				Exec:          probe("healthz"),
				PeriodSeconds: 10,
			},
			ReadinessProbe: &corev1.Probe{
				Exec:          probe("readyz"),
				PeriodSeconds: 10,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: egressRun, MountPath: "/run/egress"},
				{Name: egressConfig, MountPath: "/etc/egress", ReadOnly: true},
				{Name: "keycard-token", MountPath: "/var/run/secrets/keycard", ReadOnly: true},
				{Name: "ca-key", MountPath: "/var/lib/egress/ca"},
				{Name: sharedCA, MountPath: "/shared-ca"},
			},
		},
	}
	wantContainers := []corev1.Container{
		{
			Name:  "agent",
			Image: "ghcr.io/keycardai/sandbox-base:v0.2.0",
			Args:  []string{"sleep", "infinity"},
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:             new(true),
				RunAsUser:                new(int64(1000)),
				RunAsGroup:               new(int64(1000)),
				AllowPrivilegeEscalation: new(false),
				Capabilities:             &corev1.Capabilities{Drop: dropAll},
				SeccompProfile:           runtimeDefault,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: sharedCA, MountPath: "/shared-ca", ReadOnly: true},
			},
		},
	}
	for _, tc := range []struct {
		field     string
		got, want []corev1.Container
	}{
		{"initContainers", spec.InitContainers, wantInit},
		{"containers", spec.Containers, wantContainers},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s = %s\nwant %s", tc.field, asJSON(tc.got), asJSON(tc.want))
		}
	}

	memory := &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}
	wantVolumes := []corev1.Volume{
		{Name: egressRun, EmptyDir: memory},
		{Name: "ca-key", EmptyDir: memory},
		{Name: sharedCA, EmptyDir: &corev1.EmptyDirVolumeSource{}},
		{Name: "xtables-run", EmptyDir: memory},
		{Name: egressConfig, ConfigMap: &corev1.ConfigMapVolumeSource{Name: "egress-proxy-config"}},
		{Name: "keycard-token", Projected: &corev1.ProjectedVolumeSource{
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Path:              "token",
				Audience:          "https://acme.keycard.cloud",
				ExpirationSeconds: new(int64(3600)),
			}}},
		}},
	}
	if !reflect.DeepEqual(spec.Volumes, wantVolumes) {
		t.Errorf("volumes = %s\nwant %s", asJSON(spec.Volumes), asJSON(wantVolumes))
	}
}

// TestInstallRefusesValues checks that the chart fails to render values that
// would weaken the sandbox. The token audience has no safe default. An agent
// with the proxy's uid is exempt from the redirect and the DROP. A container
// without a uid runs as its image's USER, which can be the proxy's uid.
func TestInstallRefusesValues(t *testing.T) {
	t.Parallel()
	dir, err := filepath.Abs("../../install")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		values string
		want   string
	}{
		{"no zoneURL", "", "zoneURL is required"},
		{
			"agent uid is the proxy uid",
			"zoneURL: https://acme.keycard.cloud\nagent:\n  uid: 1337\n",
			"agent.uid must differ from proxy.uid",
		},
		{
			"no agent uid",
			"zoneURL: https://acme.keycard.cloud\nagent:\n  uid: null\n",
			"agent.uid is required",
		},
		{
			"no proxy uid",
			"zoneURL: https://acme.keycard.cloud\nproxy:\n  uid: null\n",
			"proxy.uid is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			values := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(values, []byte(tc.values), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := chart.Render(t.Context(), dir, release, "agents", values)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a render failure containing %q", err, tc.want)
			}
		})
	}
}
