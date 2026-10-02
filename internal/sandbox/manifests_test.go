package sandbox_test

import (
	"maps"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/keycardai/agent-sandbox-poc/internal/sandbox"
)

const testZoneURL = "https://zone.localdev.keycard.sh"

func aliceSandbox() sandbox.Sandbox {
	return sandbox.Sandbox{
		Name:            "sandbox-usr_alic",
		Namespace:       "agents",
		ServiceAccount:  "usr_alice",
		ZoneURL:         testZoneURL,
		ClientID:        "cred_alice",
		ImpersonateUser: "alice-identifier",
		HostGateway:     "192.168.1.1",
		UserID:          "usr_alice",
		Resources: []sandbox.Resource{
			{Slug: sandbox.SlugGitHub, APIURL: "https://api.github.com"},
			{Slug: sandbox.SlugLinear, APIURL: "https://api.linear.app"},
		},
	}
}

// aliceObjects responds with the pod and the proxy ConfigMap of Alice's
// sandbox.
func aliceObjects(t *testing.T) (*corev1.Pod, *corev1.ConfigMap) {
	t.Helper()
	all, err := aliceSandbox().Objects()
	if err != nil {
		t.Fatal(err)
	}
	var pods []*corev1.Pod
	var configs []*corev1.ConfigMap
	for _, object := range all {
		switch typed := object.(type) {
		case *corev1.Pod:
			pods = append(pods, typed)
		case *corev1.ConfigMap:
			configs = append(configs, typed)
		}
	}
	if len(pods) != 1 || len(configs) != 1 {
		t.Fatalf("want one Pod and one ConfigMap, got %d and %d", len(pods), len(configs))
	}
	return pods[0], configs[0]
}

func alicePod(t *testing.T) corev1.PodSpec {
	t.Helper()
	pod, _ := aliceObjects(t)
	return pod.Spec
}

// proxyConfig is the part of egress-proxy's configuration the tests read.
type proxyConfig struct {
	Identity struct {
		ZoneURL     string `json:"zone_url"`
		ClientID    string `json:"client_id"`
		Impersonate struct {
			User string `json:"user"`
		} `json:"impersonate"`
	} `json:"identity"`
	Policy struct {
		DenyListExceptions []string `json:"deny_list_exceptions"`
	} `json:"policy"`
	Endpoints []struct {
		Host       string `json:"host"`
		Credential struct {
			Placement struct {
				Name     string `json:"name"`
				Template string `json:"template"`
			} `json:"placement"`
		} `json:"credential"`
	} `json:"endpoints"`
}

func aliceProxyConfig(t *testing.T) proxyConfig {
	t.Helper()
	var config proxyConfig
	_, configMap := aliceObjects(t)
	if err := yaml.Unmarshal([]byte(configMap.Data["config.yaml"]), &config); err != nil {
		t.Fatalf("the configuration is not YAML: %v", err)
	}
	return config
}

// TestProxyActsForTheUser checks who the proxy is, who it acts for, and that
// it may reach the zone at the host gateway.
func TestProxyActsForTheUser(t *testing.T) {
	t.Parallel()

	config := aliceProxyConfig(t)
	if config.Identity.ZoneURL != testZoneURL || config.Identity.ClientID != "cred_alice" {
		t.Errorf("identity = %+v, want the zone URL and Alice's credential", config.Identity)
	}
	if config.Identity.Impersonate.User != "alice-identifier" {
		t.Errorf("impersonate user = %q, want Alice's identifier", config.Identity.Impersonate.User)
	}
	if !slices.Equal(config.Policy.DenyListExceptions, []string{"192.168.1.1/32"}) {
		t.Errorf("deny list exceptions = %v, want only the host gateway", config.Policy.DenyListExceptions)
	}
}

// TestProxyWritesEachAPIsCredential checks how the proxy writes each API's
// credential: a bearer token for GitHub, and the bare key for Linear, which
// reads a personal API key without a scheme.
func TestProxyWritesEachAPIsCredential(t *testing.T) {
	t.Parallel()

	templates := map[string]string{}
	for _, endpoint := range aliceProxyConfig(t).Endpoints {
		if endpoint.Credential.Placement.Name != "Authorization" {
			t.Errorf("%s writes its credential to %q", endpoint.Host, endpoint.Credential.Placement.Name)
		}
		templates[endpoint.Host] = endpoint.Credential.Placement.Template
	}
	want := map[string]string{"api.github.com": "Bearer {credential}", "api.linear.app": "{credential}"}
	if !maps.Equal(templates, want) {
		t.Errorf("templates = %v, want %v", templates, want)
	}
}

// TestProxyIsANativeSidecar checks that the proxy keeps running beside the
// agent, under the uid the traffic capture exempts.
func TestProxyIsANativeSidecar(t *testing.T) {
	t.Parallel()

	containers := alicePod(t).InitContainers
	if len(containers) != 2 || containers[1].Name != "proxy" {
		t.Fatalf("init containers = %v, want netsetup then the proxy", containers)
	}
	proxy := containers[1]
	if proxy.RestartPolicy == nil || *proxy.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("the proxy is not a native sidecar")
	}
	if uid := proxy.SecurityContext.RunAsUser; uid == nil || *uid != 1337 {
		t.Errorf("the proxy runs as %v, want 1337", uid)
	}
}

// TestAgentRunsUnprivileged checks the agent cannot gain privileges or read a
// ServiceAccount token of its own.
func TestAgentRunsUnprivileged(t *testing.T) {
	t.Parallel()

	spec := alicePod(t)
	security := spec.Containers[0].SecurityContext
	if uid := security.RunAsUser; uid == nil || *uid != 1000 {
		t.Errorf("the agent runs as %v, want 1000", uid)
	}
	if escalate := security.AllowPrivilegeEscalation; escalate == nil || *escalate {
		t.Error("the agent may escalate privileges")
	}
	if automount := spec.AutomountServiceAccountToken; automount == nil || *automount {
		t.Error("the pod mounts the default ServiceAccount token")
	}
}

// TestWorkloadTokenIsForTheZone checks the token the proxy authenticates with
// is minted for the zone.
func TestWorkloadTokenIsForTheZone(t *testing.T) {
	t.Parallel()

	var audiences []string
	for _, volume := range alicePod(t).Volumes {
		if volume.Projected != nil {
			audiences = append(audiences, volume.Projected.Sources[0].ServiceAccountToken.Audience)
		}
	}
	if !slices.Equal(audiences, []string{testZoneURL}) {
		t.Errorf("token audiences = %v, want the zone URL", audiences)
	}
}

func TestPodNameUsesTheStartOfTheUserID(t *testing.T) {
	t.Parallel()

	if got := sandbox.PodName("llsxumigz7hy2m0kf2je7pbim0"); got != "sandbox-llsxumig" {
		t.Errorf("PodName = %q", got)
	}
	if got := sandbox.PodName("abc"); got != "sandbox-abc" {
		t.Errorf("PodName of a short id = %q", got)
	}
}
