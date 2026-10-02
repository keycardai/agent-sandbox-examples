package sandbox

import (
	"fmt"
	"net/url"
	"strings"
)

// The uids egress-proxy's pod contract names: the proxy runs as one the
// capture exempts, and the agent as another.
const (
	proxyUID     = 1337
	agentUID     = 1000
	redirectPort = 1015
)

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
func NamespaceObjects(namespace, zoneCA string) []map[string]any {
	return []map[string]any{
		{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{"name": namespace, "labels": map[string]any{
				"pod-security.kubernetes.io/enforce": "privileged",
				"app.kubernetes.io/managed-by":       managedBy,
			}},
		},
		{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   meta("zone-ca", namespace),
			"data":       map[string]any{"ca.pem": zoneCA},
		},
	}
}

// Objects are the ServiceAccount, the proxy configuration and the pod.
func (s Sandbox) Objects() []map[string]any {
	labels := map[string]any{"app.kubernetes.io/managed-by": managedBy, "keycard.ai/sandbox": s.Name}
	if s.UserID != "" {
		labels["keycard.ai/user-id"] = s.UserID
	}
	zoneHost := strings.TrimPrefix(s.ZoneURL, "https://")
	return []map[string]any{
		{
			"apiVersion": "v1",
			"kind":       "ServiceAccount",
			"metadata": map[string]any{
				"name":      s.ServiceAccount,
				"namespace": s.Namespace,
				"labels":    labels,
			},
			"automountServiceAccountToken": false,
		},
		{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": s.Name + "-proxy", "namespace": s.Namespace, "labels": labels},
			"data":       map[string]any{"config.yaml": s.proxyConfig()},
		},
		{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata":   map[string]any{"name": s.Name, "namespace": s.Namespace, "labels": labels},
			"spec":       s.podSpec(zoneHost),
		},
	}
}

// proxyConfig is the egress-proxy document. The proxy reaches the zone at
// the host gateway, an RFC 1918 address the platform deny list covers, so the
// document names it as an exception. The proxy writes each resource's
// credential into the Authorization header whether or not the agent sends one.
func (s Sandbox) proxyConfig() string {
	var b strings.Builder
	fmt.Fprintf(&b, `version: 1
identity:
  zone_url: %q
  client_id: %q
  workload_token_file: "/var/run/secrets/keycard/token"
  impersonate:
    user: %q
listen:
  transparent_port: %d
netsetup:
  receipt: "/run/egress/netsetup/netsetup.json"
ca:
  out_dir: "/shared-ca/out"
tls:
  upstream_ca_files: ["/etc/keycard/zone-ca/ca.pem"]
policy:
  cluster_cidrs: ["10.42.0.0/16", "10.43.0.0/16"]
  deny_list_exceptions: [%q]
endpoints:
`, s.ZoneURL, s.ClientID, s.ImpersonateUser, redirectPort, s.HostGateway+"/32")
	for _, r := range s.Resources {
		host := r.APIURL
		if u, err := url.Parse(r.APIURL); err == nil && u.Host != "" {
			host = u.Host
		}
		fmt.Fprintf(&b, `  - host: %q
    resource: %q
    credential:
      placement: {kind: header, name: Authorization, template: %q}
      env: %s
`, host, r.APIURL, authorization(r.Slug), placeholderEnv(r.Slug))
	}
	return b.String()
}

// authorization is the Authorization header the proxy writes for a resource.
// Linear reads a personal API key without a scheme. The other API servers
// take their key as a bearer token.
func authorization(slug string) string {
	if slug == "linear" {
		return "{credential}"
	}
	return "Bearer {credential}"
}

// placeholderEnv is the variable the agent holds a placeholder in for a
// resource, for tools that refuse to run without a token.
func placeholderEnv(slug string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(slug)) + "_TOKEN"
}

func (s Sandbox) podSpec(zoneHost string) map[string]any {
	agentEnv := []any{map[string]any{"name": "KEYCARD_CA_FILE", "value": "/shared-ca/out/ca.crt"}}
	for _, r := range s.Resources {
		agentEnv = append(agentEnv, map[string]any{"name": placeholderEnv(r.Slug), "value": "keycard-placeholder"})
	}
	return map[string]any{
		"serviceAccountName":           s.ServiceAccount,
		"automountServiceAccountToken": false,
		"hostAliases":                  []any{map[string]any{"ip": s.HostGateway, "hostnames": []any{zoneHost}}},
		// The proxy binds the redirect port below 1024 and refuses to start
		// unless the kernel agrees. containerd sets this sysctl to 0.
		"securityContext": map[string]any{"sysctls": []any{map[string]any{
			"name": "net.ipv4.ip_unprivileged_port_start", "value": "1024",
		}}},
		"initContainers": []any{s.netsetup(), s.proxy()},
		"containers": []any{map[string]any{
			"name": "agent", "image": "sandbox-base:local", "imagePullPolicy": "Never",
			"args": []any{"sleep", "infinity"},
			"env":  agentEnv,
			"securityContext": map[string]any{
				"runAsUser": agentUID, "runAsGroup": agentUID, "runAsNonRoot": true,
				"allowPrivilegeEscalation": false, "capabilities": map[string]any{"drop": []any{"ALL"}},
			},
			"volumeMounts": []any{map[string]any{"name": "shared-ca", "mountPath": "/shared-ca", "readOnly": true}},
		}},
		"volumes": []any{
			map[string]any{"name": "egress-run", "emptyDir": map[string]any{"medium": "Memory"}},
			map[string]any{"name": "shared-ca", "emptyDir": map[string]any{}},
			map[string]any{"name": "config", "configMap": map[string]any{"name": s.Name + "-proxy"}},
			map[string]any{"name": "zone-ca", "configMap": map[string]any{"name": "zone-ca"}},
			map[string]any{"name": "keycard-token", "projected": map[string]any{"defaultMode": 0o644, "sources": []any{
				map[string]any{"serviceAccountToken": map[string]any{
					"path": "token", "audience": s.ZoneURL, "expirationSeconds": 3600,
				}},
			}}},
		},
	}
}

func (s Sandbox) netsetup() map[string]any {
	return map[string]any{
		"name": "netsetup", "image": "egress-netsetup:local", "imagePullPolicy": "Never",
		"args": []any{
			"--receipt=/run/egress/netsetup/netsetup.json",
			fmt.Sprintf("--proxy-uid=%d", proxyUID),
			"--ipv6=off",
		},
		"securityContext": map[string]any{
			"runAsUser": 0, "allowPrivilegeEscalation": false,
			"capabilities": map[string]any{"drop": []any{"ALL"}, "add": []any{"NET_ADMIN", "NET_RAW", "SETPCAP"}},
		},
		"volumeMounts": []any{map[string]any{"name": "egress-run", "mountPath": "/run/egress"}},
	}
}

// proxy is a native sidecar: an init container that keeps running, so the
// kubelet holds the agent until the startup probe passes.
func (s Sandbox) proxy() map[string]any {
	probe := func(route string, period int) map[string]any {
		return map[string]any{
			"exec":          map[string]any{"command": []any{"/ko-app/egress-proxy", "probe", route}},
			"periodSeconds": period,
		}
	}
	startup := probe("readyz", 2)
	startup["failureThreshold"] = 30
	return map[string]any{
		"name": "proxy", "image": "egress-proxy:local", "imagePullPolicy": "Never", "restartPolicy": "Always",
		"env": []any{
			map[string]any{"name": "EGRESS_PROXY_CONFIG", "value": "/etc/keycard/config/config.yaml"},
			map[string]any{
				"name":      "EGRESS_PROXY_SANDBOX_ID",
				"valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}},
			},
			map[string]any{"name": "EGRESS_PROXY_ADMIN", "value": "/run/egress/admin/admin.sock"},
		},
		"securityContext": map[string]any{
			"runAsUser": proxyUID, "runAsGroup": proxyUID, "runAsNonRoot": true,
			"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
			"capabilities": map[string]any{"drop": []any{"ALL"}, "add": []any{"NET_BIND_SERVICE"}},
		},
		"startupProbe":   startup,
		"readinessProbe": probe("readyz", 5),
		"livenessProbe":  probe("healthz", 10),
		"volumeMounts": []any{
			map[string]any{"name": "egress-run", "mountPath": "/run/egress"},
			map[string]any{"name": "shared-ca", "mountPath": "/shared-ca"},
			map[string]any{"name": "config", "mountPath": "/etc/keycard/config", "readOnly": true},
			map[string]any{"name": "zone-ca", "mountPath": "/etc/keycard/zone-ca", "readOnly": true},
			map[string]any{"name": "keycard-token", "mountPath": "/var/run/secrets/keycard", "readOnly": true},
		},
	}
}

// PodName is the sandbox pod name for a user.
func PodName(userID string) string {
	if len(userID) > 8 {
		userID = userID[:8]
	}
	return "sandbox-" + userID
}
