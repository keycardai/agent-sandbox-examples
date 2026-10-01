# install

The once-per-cluster Helm chart for Keycard agent sandboxes. It installs:

- the sandbox namespace, which is the release namespace, labeled with the
  `privileged` Pod Security level and kept when the release is deleted
- the sandbox-admin Role in that namespace, and a RoleBinding to
  `rbac.subjects` when that list is not empty
- a NetworkPolicy on every pod in the namespace that denies ingress and allows
  egress only to TCP 443 on public addresses and to the cluster DNS

`values.yaml` documents every key. `values.example.yaml` is a starting point.

## Install

`install.sh` applies the agent-sandbox controller, then installs the chart. It
stops before any change when an existing namespace sets a Pod Security level
other than `privileged`, and prints the `kubectl label` command that fixes it.
It also stops unless the namespace enforces `privileged` after the install.

    ./install/install.sh --context NAME --values my-values.yaml

The controller manifest in `agent-sandbox/` is the upstream release asset.
`agent-sandbox/refresh.sh` downloads it and checks it against the digest GitHub
publishes.

## The sandbox-admin Role is effectively node admin

The Role grants API access in the sandbox namespace only, but its effect
reaches the node. Bind it only to people you would trust as node
administrators. A Sandbox embeds an arbitrary pod template, and the
namespace admits privileged pods, so creating a Sandbox is equivalent to
administering the node it lands on.

It is also equivalent to impersonating every enrolled user. The Role can
create a Sandbox under any user's ServiceAccount in the namespace, and that
Sandbox acts with the user's identity.
