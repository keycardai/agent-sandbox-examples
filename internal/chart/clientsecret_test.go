package chart_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const (
	clientSecretName = "egress-proxy-client-secret" //nolint:gosec // G101: a Secret's name, not a secret.
	// copier names the init container that copies the client secret, the
	// volume of the Secret it reads, and the Secret's key.
	copier          = "client-secret"
	proxyContainer  = "egress-proxy"
	keycardToken    = "keycard-token"
	clientSecretOut = "/var/run/secrets/keycard/client-secret" //nolint:gosec // G101: a file path.
)

// clientSecretValues are the sandbox values with clientSecret.secretName set
// and no ServiceAccount subject.
func clientSecretValues(t *testing.T) []string {
	t.Helper()
	return sandboxValues(t, writeValues(t, "serviceAccount: \"\"\nclientSecret:\n  secretName: "+clientSecretName+"\n"))
}

// TestSandboxClientSecret checks the pod and document with a client secret.
// The proxy refuses a secret file that a group or another user can read, and
// the kubelet makes a Secret volume's files owned by root. The init container
// therefore copies the file as the proxy's uid with mode 0400, and only it
// mounts the Secret.
func TestSandboxClientSecret(t *testing.T) {
	t.Parallel()
	objects := renderChart(t, sandboxDir(t), sandboxUserID, clientSecretValues(t)...)
	pod := sandboxPod(t, objects)
	t.Run("document", func(t *testing.T) {
		t.Parallel()
		checkClientSecretDocument(t, parseDocument(t, configDocument(t, objects)))
	})
	t.Run("init containers", func(t *testing.T) {
		t.Parallel()
		checkCopier(t, pod.Spec)
	})
	t.Run("volumes", func(t *testing.T) {
		t.Parallel()
		checkClientSecretVolumes(t, pod.Spec)
	})
	t.Run("mounts", func(t *testing.T) {
		t.Parallel()
		mounts := map[string][]string{}
		for _, c := range slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers) {
			for _, m := range c.VolumeMounts {
				mounts[m.Name] = append(mounts[m.Name], c.Name)
			}
		}
		if want := []string{copier}; !slices.Equal(mounts[copier], want) {
			t.Errorf("containers that mount the Secret = %v, want %v", mounts[copier], want)
		}
		if want := []string{copier, proxyContainer}; !slices.Equal(mounts[keycardToken], want) {
			t.Errorf("containers that mount %s = %v, want %v", keycardToken, mounts[keycardToken], want)
		}
	})
}

func checkClientSecretDocument(t *testing.T, doc document) {
	t.Helper()
	for _, problem := range validateDocument(doc) {
		t.Error(problem)
	}
	if doc.Identity == nil {
		t.Fatal("the document has no identity")
	}
	if doc.Identity.ClientSecretFile != clientSecretOut {
		t.Errorf("identity.client_secret_file = %q, want %q", doc.Identity.ClientSecretFile, clientSecretOut)
	}
	if doc.Identity.WorkloadTokenFile != "" {
		t.Errorf("identity.workload_token_file = %q, want none", doc.Identity.WorkloadTokenFile)
	}
}

func checkCopier(t *testing.T, spec corev1.PodSpec) {
	t.Helper()
	names := make([]string, 0, len(spec.InitContainers))
	for _, c := range spec.InitContainers {
		names = append(names, c.Name)
	}
	if want := []string{"egress-netsetup", copier, proxyContainer}; !slices.Equal(names, want) {
		t.Fatalf("init containers = %v, want %v", names, want)
	}
	copierSpec := container(t, spec.InitContainers, copier)
	wantCommand := []string{"install", "-m", "0400", "/client-secret/" + copier, clientSecretOut}
	if !slices.Equal(copierSpec.Command, wantCommand) {
		t.Errorf("%s command = %q, want %q", copier, copierSpec.Command, wantCommand)
	}
	if copierSpec.Image != spec.Containers[0].Image {
		t.Errorf("%s image = %q, want the agent image %q", copier, copierSpec.Image, spec.Containers[0].Image)
	}
	checkCopierSecurity(t, copierSpec.SecurityContext)
}

// checkCopierSecurity checks that the copier runs as the proxy's uid, so it
// owns the file it writes, and without privilege.
func checkCopierSecurity(t *testing.T, security *corev1.SecurityContext) {
	t.Helper()
	switch {
	case security == nil || security.RunAsUser == nil || *security.RunAsUser != 1337:
		t.Errorf("%s securityContext = %+v, want runAsUser 1337", copier, security)
	case security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation:
		t.Errorf("%s does not set allowPrivilegeEscalation: false", copier)
	case security.Capabilities == nil || !slices.Equal(security.Capabilities.Drop, []corev1.Capability{"ALL"}):
		t.Errorf("%s capabilities = %+v, want drop ALL", copier, security.Capabilities)
	}
}

func checkClientSecretVolumes(t *testing.T, spec corev1.PodSpec) {
	t.Helper()
	volumes := map[string]corev1.VolumeSource{}
	for _, v := range spec.Volumes {
		volumes[v.Name] = v.VolumeSource
	}
	token := volumes[keycardToken]
	if token.EmptyDir == nil || token.EmptyDir.Medium != corev1.StorageMediumMemory || token.Projected != nil {
		t.Errorf("%s = %s, want a memory emptyDir and no projected token", keycardToken, asJSON(token))
	}
	readable := int32(0o444)
	want := &corev1.SecretVolumeSource{
		SecretName:  clientSecretName,
		DefaultMode: &readable,
		Items:       []corev1.KeyToPath{{Key: copier, Path: copier}},
	}
	if got := volumes[copier].Secret; !reflect.DeepEqual(got, want) {
		t.Errorf("%s volume = %s, want %s", copier, asJSON(got), asJSON(want))
	}
}

// sandboxPod decodes the pod template of the rendered Sandbox.
func sandboxPod(t *testing.T, objects map[string][]byte) corev1.PodTemplateSpec {
	t.Helper()
	var box sandboxObject
	decode(t, objects, "Sandbox/"+sandboxUserID, &box)
	raw, err := json.Marshal(box.Spec.PodTemplate)
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.PodTemplateSpec
	if err = yaml.UnmarshalStrict(raw, &pod); err != nil {
		t.Fatalf("decoding the pod template: %v", err)
	}
	return pod
}
