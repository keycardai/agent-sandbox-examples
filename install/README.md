# install

The once-per-cluster Helm chart for Keycard agent sandboxes. It installs:

- the sandbox namespace, which is the release namespace, labeled with the
  `privileged` Pod Security level and kept when the release is deleted
- the `SandboxTemplate` with the reference sandbox pod: the netsetup init
  container, the proxy sidecar and the agent. It sets
  `networkPolicyManagement: Unmanaged`, so agent-sandbox adds no
  NetworkPolicy of its own. `zoneURL` is required.
- the sandbox-admin Role in that namespace, and a RoleBinding to
  `rbac.subjects` when that list is not empty
- a NetworkPolicy on every pod in the namespace that denies ingress and allows
  egress only to TCP 443 on public addresses, to TCP 443 on each address in
  `network.allowCIDRs`, and to the cluster DNS

`values.yaml` documents every key. `values.example.yaml` is a starting point.

`new-sandbox` renders the same pod into one `Sandbox` per user. See
`sandbox/README.md`.

## Install

`install.sh` applies the agent-sandbox controller, then installs the chart. It
stops before any change when an existing namespace sets a Pod Security level
other than `privileged`, and prints the `kubectl label` command that fixes it.
It also stops unless the namespace enforces `privileged` after the install.

    ./install/install.sh --context NAME --values my-values.yaml

The controller manifest in `agent-sandbox/` is the upstream release asset.
`agent-sandbox/refresh.sh` downloads it and checks it against the digest GitHub
publishes.

## A zone that is not on the internet

Three values let the proxy reach a zone on a private address behind a private
CA. All three are empty by default.

- `zoneTrust` holds the root certificate. The chart renders it as a ConfigMap
  and mounts it only on the proxy container.
- `hostAliases` maps the zone's host name to the private address.
- `network.allowCIDRs` allows that address on TCP 443. Each entry is a single
  address, and none can be in 169.254.0.0/16.

Give the sandbox chart the same `zoneTrust` and `hostAliases`. `docs/local.md`
shows them filled in.

## The pod contract

The pod follows `docs/containers.md` in `keycardai/egress-proxy`. Each row
is a chart value.

| Item | Value |
|---|---|
| Containers | `egress-netsetup`, `egress-proxy` as a native sidecar, `agent` |
| Proxy uid, agent uid | 1337, 1000 |
| Redirect port | 1015 (`proxy.redirectPort` and `--redirect-port` in `netsetup.args`). The sandbox chart writes it into `listen.transparent_port`. |
| Resolver port | 53 (`--dns-port` in `netsetup.args`). The sandbox chart writes `:53` into `listen.dns`. |
| `net.ipv4.ip_unprivileged_port_start` | 1024 (`proxy.unprivilegedPortStart`), a pod sysctl. The proxy refuses to start when it is at or below the higher of the redirect port and the resolver port. |
| Agent command | `agent.args`, `["sleep", "infinity"]` by default. The `sandbox-base` entrypoint exits 64 when there are no args. |
| Netsetup receipt | `/run/egress/netsetup/netsetup.json`, on the memory-backed `egress-run` volume that both proxy-side containers mount |
| Proxy capabilities | `drop: ["ALL"]`, `add: ["NET_BIND_SERVICE"]`. NEVER remove the addition: the binary has `cap_net_bind_service=+ep`, and the kernel refuses to exec it without the capability in the bounding set. |
| Admin socket | `/run/egress/admin/admin.sock`, the binary default, on `egress-run` |
| CA certificate | `/shared-ca/out/ca.crt`, the binary default, on `shared-ca`, which the agent mounts read-only |
| Probes | `probe readyz` for startup and readiness, `probe healthz` for liveness |
| Proxy configuration | a ConfigMap named per sandbox, mounted on the proxy alone |
| Agent volumes | `/shared-ca` read-only, and nothing else |

Each default path is a directory inside a volume, never the mount point,
so the proxy creates and owns it. `internal/chart/settings_test.go` runs
`egress-proxy settings` and checks both charts against what it prints: the
netsetup arguments, the proxy uid, the sysctl, the probe routes, the
default paths, `listen.transparent_port` and `listen.dns`. It runs when
`EGRESS_PROXY` names an `egress-proxy` binary and skips otherwise.

## The sandbox-admin Role is effectively node admin

The Role grants API access in the sandbox namespace only, but its effect
reaches the node. Bind it only to people you would trust as node
administrators. A Sandbox embeds an arbitrary pod template, and the
namespace admits privileged pods, so creating a Sandbox is equivalent to
administering the node it lands on.

It is also equivalent to impersonating every enrolled user. The Role can
create a Sandbox under any user's ServiceAccount in the namespace, and that
Sandbox acts with the user's identity.
