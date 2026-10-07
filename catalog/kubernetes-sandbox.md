Install this once per zone. It creates the zone half of a Kubernetes agent
sandbox: an IAM Provider for your cluster's OIDC issuer, and the
Application the egress proxy authenticates as. The cluster half is a Helm
chart in
[`keycardai/agent-sandbox-poc`](https://github.com/keycardai/agent-sandbox-poc).
This guide covers both halves, in order.

Values in angle brackets are placeholders. Replace each one with your own
value.

## Before you install

- An Amazon EKS cluster with EC2 node groups. A Fargate profile that
  matches the sandbox namespace breaks the sandbox pod, because
  "Privileged containers aren't supported on Fargate"
  ([EKS Fargate considerations](https://docs.aws.amazon.com/eks/latest/userguide/fargate.html)).
- [mise](https://mise.jdx.dev). Run `mise install` in the repository to get
  the `kubectl`, `helm`, and `go` versions it pins. The scripts refuse any
  other version.
- A kubeconfig context that can create CRDs and namespaces.

## Find your cluster's OIDC issuer URL

This install takes the OIDC issuer URL of the cluster the sandboxes run
in, and an optional namespace that defaults to `agents`. The zone fetches
the cluster's JWKS from the issuer and verifies each sandbox's projected
ServiceAccount token against it.

On EKS, read the issuer from the cluster:

```sh
aws eks describe-cluster --name <cluster> --query "cluster.identity.oidc.issuer" --output text
```

It looks like `https://oidc.eks.<region>.amazonaws.com/id/<id>`
([Create an IAM OIDC provider for your cluster](https://docs.aws.amazon.com/eks/latest/userguide/enable-iam-roles-for-service-accounts.html)).
Use the same value for `issuer_url` in the `zone-setup` values file.

## What this install produces

| Output | What it is |
|---|---|
| `provider_id` | the IAM Provider for the cluster's issuer |
| `application_id` | the egress proxy Application |
| `application_identifier` | `urn:keycard:sandbox-proxy:<namespace>`, the well-known identifier the per-user item uses to find this Application |
| `namespace` | the Kubernetes namespace the sandboxes run in |

You already know two of these. `namespace` is your input, and
`application_identifier` is `urn:keycard:sandbox-proxy:` followed by the
namespace. Read the other two from this install's outputs in the Console.
If the Console does not show them, run `zone-setup`, which prints all four
under `k8s_sandbox`. A sandbox runs without them. `new-sandbox` takes
`provider_id` and `application_id` as optional flags and records them as
annotations on the objects it creates.

## The namespace must enforce the Privileged Pod Security level

Don't install into a namespace that enforces `baseline` or `restricted`.
The sandbox pod's `egress-netsetup` init container adds `NET_ADMIN`,
`NET_RAW`, and `SETPCAP` to write the iptables rules that redirect the
agent's traffic into the proxy. `NET_ADMIN` and `NET_RAW` are not on the
Baseline profile's allowed capability list (`AUDIT_WRITE`, `CHOWN`,
`DAC_OVERRIDE`, `FOWNER`, `FSETID`, `KILL`, `MKNOD`, `NET_BIND_SERVICE`,
`SETFCAP`, `SETGID`, `SETPCAP`, `SETUID`, `SYS_CHROOT`). Pod Security
Admission rejects the pod, and no sandbox starts. The Privileged profile
"is defined by an absence of restrictions"
([Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)).
Istio's `istio-init` has the same requirement for the same reason. It
"requires `NET_ADMIN` and `NET_RAW` capabilities to configure iptables
traffic redirection", and "the namespace must use the `privileged` Pod
Security Admission enforcement level"
([Istio application requirements](https://istio.io/latest/docs/ops/deployment/application-requirements/)).

A default EKS cluster admits the init container. EKS turns on Pod Security
Admission from version 1.23 with "the Privileged PSS profile configured by
default for all PSA modes"
([AWS Containers blog, 2022-10-27](https://aws.amazon.com/blogs/containers/implementing-pod-security-standards-in-amazon-eks/)).
The chart labels the namespace `privileged` for the enforce, audit, and
warn modes. If the namespace already exists and sets another level,
`install.sh` stops before it changes anything and prints the
`kubectl label` command that fixes it. It also stops if the namespace does
not enforce `privileged` after the install. A policy engine such as
Kyverno or Gatekeeper needs its own exemption for the init container.

## Install the cluster half

```sh
git clone https://github.com/keycardai/agent-sandbox-poc
cd agent-sandbox-poc
cp install/values.example.yaml my-cluster.yaml
$EDITOR my-cluster.yaml          # zone URL, cluster ranges, who can create sandboxes
./install/install.sh --context <context> --values my-cluster.yaml --namespace <namespace>
```

Set these three keys in the values file:

- `zoneURL` is the zone that decides every token exchange. It is also the
  audience of the projected ServiceAccount token.
- `network.clusterCIDRs` is this cluster's pod and service ranges. The
  NetworkPolicy adds them to the ranges it blocks on egress.
- `rbac.subjects` names the users and groups who can create sandboxes in
  the namespace. Bind it only to people you trust as node administrators,
  because a `Sandbox` embeds a pod template and the namespace admits
  privileged pods.

`install/values.yaml` documents every other key.

`install.sh` applies the agent-sandbox controller from the release
manifest in `install/agent-sandbox/`, waits for the controller, and runs
`helm upgrade --install`. `--context` is required and names the cluster.
You can repeat `--values`; a later file overrides an earlier one.
`--namespace` defaults to `agents`.

The chart creates the namespace, the `SandboxTemplate` with the reference
pod, a NetworkPolicy, and a Role for the people who run `new-sandbox`. The
NetworkPolicy denies every inbound connection. On egress, it allows only
TCP 443 to public addresses and DNS to the cluster DNS. The
`SandboxTemplate` sets `networkPolicyManagement: Unmanaged`, so
agent-sandbox does not add a NetworkPolicy of its own.

## Add a Resource for each credentialed destination

Each destination the agent reaches with a credential is a Resource in the
zone, and it must be a dependency of the egress proxy Application. Without
the dependency, the zone refuses the token exchange with `access_denied`.
This install does not create Resources.
`zone-setup` creates them from the `resources` list in its values file,
along with the vault secret behind each static credential. A destination
the agent reaches without a credential does not need a Resource. The proxy
forwards that request without calling the zone.

## Fallback: zone-setup

Use `zone-setup` from the same repository when this catalog item is not
available. Its code is also the specification of what this item produces.
It creates the same Provider and Application under the same identifiers
through the management API, and prints the four outputs listed earlier.

```sh
cp zone-setup.example.yaml zone-setup.yaml
$EDITOR zone-setup.yaml                      # URLs, service account id, issuer, users, resources
export KEYCARD_MANAGEMENT_CLIENT_SECRET=...  # the organization service account secret
go run ./cmd/zone-setup --dry-run
go run ./cmd/zone-setup
```

`zone-setup` is idempotent. It leaves an entity alone when it already
matches the values file, patches it when its settings differ, and never
deletes anything. `--dry-run` reports what each step would do and writes
nothing. Run against a zone this item installed into, it reports nothing
to create or patch, which confirms that the install and the values file
describe the same zone.

The organization service account in the values file is NEVER the egress
proxy's identity. It authenticates `zone-setup` to the management API, and
the zone's token endpoint rejects it.

## Next

Install **Sandbox User Access** once per user. Its guide turns that
install's outputs into a running sandbox.
