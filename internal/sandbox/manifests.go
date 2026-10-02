package sandbox

import (
	"fmt"
	"maps"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// The uids egress-proxy's pod contract names: the proxy runs as one the
// capture exempts, and the agent as another.
const (
	proxyUID     = 1337
	agentUID     = 1000
	redirectPort = 1015
)

// labelManagedBy marks everything the CLI creates, so `sandbox down` can find
// what it made outside its own cluster.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedBy      = "keycard-sandbox"
)

// The volumes of a sandbox pod and where its containers mount them.
const (
	zoneCAName      = "zone-ca"
	volumeEgressRun = "egress-run"
	volumeSharedCA  = "shared-ca"
	volumeConfig    = "config"
	volumeToken     = "keycard-token"

	egressRunDir    = "/run/egress"
	sharedCADir     = "/shared-ca"
	configDir       = "/etc/keycard/config"
	zoneCADir       = "/etc/keycard/zone-ca"
	tokenDir        = "/var/run/secrets/keycard" //nolint:gosec // Where the token is mounted, not a credential.
	tokenFile       = "token"
	netsetupReceipt = egressRunDir + "/netsetup/netsetup.json"
)

// capAll drops every capability a container would otherwise get.
const capAll corev1.Capability = "ALL"

// Sandbox describes one sandbox pod.
type Sandbox struct {
	Name            string
	Namespace       string
	ServiceAccount  string
	ZoneURL         string
	ClientID        string
	ImpersonateUser string
	HostGateway     string
	Resources       []Resource
	UserID          string
}

// NamespaceObjects are the namespace the sandboxes run in and the zone CA the
// proxies trust. Pod Security is privileged because the netsetup init
// container needs NET_ADMIN and NET_RAW.
func NamespaceObjects(namespace, zoneCA string) []any {
	return []any{
		&corev1.Namespace{
			APIVersion: "v1", Kind: "Namespace",
			ObjectMeta: managedMeta(namespace, "", map[string]string{
				"pod-security.kubernetes.io/enforce": "privileged",
			}),
		},
		&corev1.ConfigMap{
			APIVersion: "v1", Kind: "ConfigMap",
			ObjectMeta: managedMeta(zoneCAName, namespace, nil),
			Data:       map[string]string{"ca.pem": zoneCA},
		},
	}
}

// Objects are the ServiceAccount, the proxy configuration and the pod.
func (s Sandbox) Objects() ([]any, error) {
	config, err := yaml.Marshal(s.proxyConfig())
	if err != nil {
		return nil, fmt.Errorf("encode the proxy configuration: %w", err)
	}
	labels := map[string]string{"keycard.ai/sandbox": s.Name}
	if s.UserID != "" {
		labels["keycard.ai/user-id"] = s.UserID
	}
	return []any{
		&corev1.ServiceAccount{
			APIVersion: "v1", Kind: "ServiceAccount",
			ObjectMeta:                   managedMeta(s.ServiceAccount, s.Namespace, labels),
			AutomountServiceAccountToken: new(false),
		},
		&corev1.ConfigMap{
			APIVersion: "v1", Kind: "ConfigMap",
			ObjectMeta: managedMeta(s.configMapName(), s.Namespace, labels),
			Data:       map[string]string{"config.yaml": string(config)},
		},
		&corev1.Pod{
			APIVersion: "v1", Kind: "Pod",
			ObjectMeta: managedMeta(s.Name, s.Namespace, labels),
			Spec:       s.podSpec(),
		},
	}, nil
}

func (s Sandbox) configMapName() string { return s.Name + "-proxy" }

// proxyConfig is the egress-proxy document. The proxy reaches the zone at the
// host gateway, an RFC 1918 address the platform deny list covers, so the
// document names it as an exception. The proxy writes each resource's
// credential into the Authorization header whether or not the agent sends one.
func (s Sandbox) proxyConfig() proxyDocument {
	doc := proxyDocument{
		Version: 1,
		Identity: proxyIdentity{
			ZoneURL:           s.ZoneURL,
			ClientID:          s.ClientID,
			WorkloadTokenFile: tokenDir + "/" + tokenFile,
			Impersonate:       proxyImpersonate{User: s.ImpersonateUser},
		},
		Listen:   proxyListen{TransparentPort: redirectPort},
		Netsetup: proxyNetsetup{Receipt: netsetupReceipt},
		CA:       proxyCA{OutDir: sharedCADir + "/out"},
		TLS:      proxyTLS{UpstreamCAFiles: []string{zoneCADir + "/ca.pem"}},
		Policy: proxyPolicy{
			ClusterCIDRs:       []string{"10.42.0.0/16", "10.43.0.0/16"},
			DenyListExceptions: []string{s.HostGateway + "/32"},
		},
	}
	for _, r := range s.Resources {
		host := r.APIURL
		if parsed, err := url.Parse(r.APIURL); err == nil && parsed.Host != "" {
			host = parsed.Host
		}
		doc.Endpoints = append(doc.Endpoints, proxyEndpoint{
			Host:     host,
			Resource: r.APIURL,
			Credential: proxyCredential{
				Placement: proxyPlacement{Kind: "header", Name: "Authorization", Template: authorization(r.Slug)},
				Env:       placeholderEnv(r.Slug),
			},
		})
	}
	return doc
}

// proxyDocument is the part of egress-proxy's configuration the CLI writes.
type proxyDocument struct {
	Version   int             `json:"version"`
	Identity  proxyIdentity   `json:"identity"`
	Listen    proxyListen     `json:"listen"`
	Netsetup  proxyNetsetup   `json:"netsetup"`
	CA        proxyCA         `json:"ca"`
	TLS       proxyTLS        `json:"tls"`
	Policy    proxyPolicy     `json:"policy"`
	Endpoints []proxyEndpoint `json:"endpoints"`
}

type proxyIdentity struct {
	ZoneURL           string           `json:"zone_url"`
	ClientID          string           `json:"client_id"`
	WorkloadTokenFile string           `json:"workload_token_file"`
	Impersonate       proxyImpersonate `json:"impersonate"`
}

type proxyImpersonate struct {
	User string `json:"user"`
}

type proxyListen struct {
	TransparentPort int `json:"transparent_port"`
}

type proxyNetsetup struct {
	Receipt string `json:"receipt"`
}

type proxyCA struct {
	OutDir string `json:"out_dir"`
}

type proxyTLS struct {
	UpstreamCAFiles []string `json:"upstream_ca_files"`
}

type proxyPolicy struct {
	ClusterCIDRs       []string `json:"cluster_cidrs"`
	DenyListExceptions []string `json:"deny_list_exceptions"`
}

type proxyEndpoint struct {
	Host       string          `json:"host"`
	Resource   string          `json:"resource"`
	Credential proxyCredential `json:"credential"`
}

type proxyCredential struct {
	Placement proxyPlacement `json:"placement"`
	Env       string         `json:"env"`
}

type proxyPlacement struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Template string `json:"template"`
}

// authorization is the Authorization header the proxy writes for a resource.
// Linear reads a personal API key without a scheme. The other API servers
// take their key as a bearer token.
func authorization(slug string) string {
	if slug == SlugLinear {
		return "{credential}"
	}
	return "Bearer {credential}"
}

// placeholderEnv is the variable the agent holds a placeholder in for a
// resource, for tools that refuse to run without a token.
func placeholderEnv(slug string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(slug)) + "_TOKEN"
}

func (s Sandbox) podSpec() corev1.PodSpec {
	return corev1.PodSpec{
		ServiceAccountName:           s.ServiceAccount,
		AutomountServiceAccountToken: new(false),
		HostAliases: []corev1.HostAlias{
			{IP: s.HostGateway, Hostnames: []string{strings.TrimPrefix(s.ZoneURL, "https://")}},
		},
		// The proxy binds the redirect port below 1024 and refuses to start
		// unless the kernel agrees. containerd sets this sysctl to 0.
		SecurityContext: &corev1.PodSecurityContext{
			Sysctls: []corev1.Sysctl{{Name: "net.ipv4.ip_unprivileged_port_start", Value: "1024"}},
		},
		InitContainers: []corev1.Container{s.netsetup(), s.proxy()},
		Containers:     []corev1.Container{s.agent()},
		Volumes:        s.volumes(),
	}
}

func (s Sandbox) agent() corev1.Container {
	env := make([]corev1.EnvVar, 0, 1+len(s.Resources))
	env = append(env, corev1.EnvVar{Name: "KEYCARD_CA_FILE", Value: sharedCADir + "/out/ca.crt"})
	for _, r := range s.Resources {
		env = append(env, corev1.EnvVar{Name: placeholderEnv(r.Slug), Value: "keycard-placeholder"})
	}
	return corev1.Container{
		Name:            "agent",
		Image:           "sandbox-base:local",
		ImagePullPolicy: corev1.PullNever,
		Args:            []string{"sleep", "infinity"},
		Env:             env,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                new(int64(agentUID)),
			RunAsGroup:               new(int64(agentUID)),
			RunAsNonRoot:             new(true),
			AllowPrivilegeEscalation: new(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{capAll}},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: volumeSharedCA, MountPath: sharedCADir, ReadOnly: true}},
	}
}

func (s Sandbox) volumes() []corev1.Volume {
	const (
		tokenMode     = 0o644
		tokenLifetime = 3600
	)
	return []corev1.Volume{
		{
			Name:     volumeEgressRun,
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
		},
		{Name: volumeSharedCA, EmptyDir: &corev1.EmptyDirVolumeSource{}},
		{Name: volumeConfig, ConfigMap: &corev1.ConfigMapVolumeSource{
			Name: s.configMapName(),
		}},
		{Name: zoneCAName, ConfigMap: &corev1.ConfigMapVolumeSource{
			Name: zoneCAName,
		}},
		{Name: volumeToken, Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: new(int32(tokenMode)),
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Path: tokenFile, Audience: s.ZoneURL, ExpirationSeconds: new(int64(tokenLifetime)),
			}}},
		}},
	}
}

func (s Sandbox) netsetup() corev1.Container {
	return corev1.Container{
		Name:            "netsetup",
		Image:           "egress-netsetup:local",
		ImagePullPolicy: corev1.PullNever,
		Args:            []string{"--receipt=" + netsetupReceipt, fmt.Sprintf("--proxy-uid=%d", proxyUID), "--ipv6=off"},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                new(int64(0)),
			AllowPrivilegeEscalation: new(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{capAll},
				Add:  []corev1.Capability{"NET_ADMIN", "NET_RAW", "SETPCAP"},
			},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: volumeEgressRun, MountPath: egressRunDir}},
	}
}

// proxy is a native sidecar: an init container that keeps running, so the
// kubelet holds the agent until the startup probe passes.
func (s Sandbox) proxy() corev1.Container {
	const (
		startupPeriod   = 2
		startupFailures = 30
		readyPeriod     = 5
		livePeriod      = 10
	)
	probe := func(route string, period int32) *corev1.Probe {
		return &corev1.Probe{
			Exec:          &corev1.ExecAction{Command: []string{"/ko-app/egress-proxy", "probe", route}},
			PeriodSeconds: period,
		}
	}
	startup := probe("readyz", startupPeriod)
	startup.FailureThreshold = startupFailures
	return corev1.Container{
		Name:            "proxy",
		Image:           "egress-proxy:local",
		ImagePullPolicy: corev1.PullNever,
		RestartPolicy:   new(corev1.ContainerRestartPolicyAlways),
		Env: []corev1.EnvVar{
			{Name: "EGRESS_PROXY_CONFIG", Value: configDir + "/config.yaml"},
			{Name: "EGRESS_PROXY_SANDBOX_ID", ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			}},
			{Name: "EGRESS_PROXY_ADMIN", Value: egressRunDir + "/admin/admin.sock"},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                new(int64(proxyUID)),
			RunAsGroup:               new(int64(proxyUID)),
			RunAsNonRoot:             new(true),
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{capAll},
				Add:  []corev1.Capability{"NET_BIND_SERVICE"},
			},
		},
		StartupProbe:   startup,
		ReadinessProbe: probe("readyz", readyPeriod),
		LivenessProbe:  probe("healthz", livePeriod),
		VolumeMounts: []corev1.VolumeMount{
			{Name: volumeEgressRun, MountPath: egressRunDir},
			{Name: volumeSharedCA, MountPath: sharedCADir},
			{Name: volumeConfig, MountPath: configDir, ReadOnly: true},
			{Name: zoneCAName, MountPath: zoneCADir, ReadOnly: true},
			{Name: volumeToken, MountPath: tokenDir, ReadOnly: true},
		},
	}
}

// managedMeta is the metadata of an object the CLI manages.
func managedMeta(name, namespace string, labels map[string]string) metav1.ObjectMeta {
	all := map[string]string{labelManagedBy: managedBy}
	maps.Copy(all, labels)
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: all}
}

// PodName is the sandbox pod name for a user, from the first characters of
// their id.
func PodName(userID string) string {
	const idChars = 8
	if len(userID) > idChars {
		userID = userID[:idChars]
	}
	return "sandbox-" + userID
}
