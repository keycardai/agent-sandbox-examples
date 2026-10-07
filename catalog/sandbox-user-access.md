# Run a sandbox for one user

This install enrolls one user in the Kubernetes sandbox. It creates a
workload-identity credential on the egress proxy Application, bound to the
ServiceAccount that user's sandbox runs as, and two policies. One policy
lets that ServiceAccount, and nothing else, impersonate the user. The other
lets the user reach the destinations the proxy gets credentials for. A
token exchange needs both. Install this once per user, after **Kubernetes
Sandbox**.

Values in angle brackets are placeholders. Replace each one with your own
value.

## Before you install

The user must already exist in the zone. This install takes the user's
email, looks up their IAM public id, and fails if no user or more than one
user matches. Create the user yourself first. This step goes away once the
item can create a user from the email.

**Kubernetes Sandbox** must be installed in this zone, and its chart
installed in the cluster. This install finds the egress proxy Application
by its well-known identifier, `urn:keycard:sandbox-proxy:<namespace>`.

## What this install produces

| Output | What it is |
|---|---|
| `user_id` | the user's IAM public id, which is also the ServiceAccount's name |
| `service_account` | `system:serviceaccount:<namespace>:<user_id>`, the subject the credential is bound to |
| `client_id` | the workload-identity credential the proxy uses to authenticate its token exchange |
| `namespace` | the sandbox namespace, from the **Kubernetes Sandbox** install |

Read them from this install in the Console. If the Console does not show
them, run `zone-setup`, which prints the same four names under
`sandbox_user_access` for every user in its values file.

The credential has no secret. The cluster's API server signs the
ServiceAccount token, and the zone verifies it against the Provider, so a
pod cannot forge the actor in the exchange. A sandbox configured with a
different user fails the exchange with `access_denied`.

## Create the sandbox

From a clone of
[`keycardai/agent-sandbox-poc`](https://github.com/keycardai/agent-sandbox-poc):

```sh
cp sandbox/values.example.yaml my-sandbox.yaml
$EDITOR my-sandbox.yaml          # zone URL, cluster ranges, endpoints
go run ./cmd/new-sandbox --values my-sandbox.yaml \
  --namespace <namespace> \
  --user-id <user_id> --user-email <user_email> \
  --service-account <service_account> --client-id <client_id> \
  --provider-id <provider_id> --application-id <application_id> \
  --application-identifier <application_identifier>
```

`--user-id`, `--service-account`, and `--client-id` are required, and each
takes the output of the same name. `--namespace` takes `namespace` and
defaults to `agents`. The other four flags are optional. `new-sandbox`
records them as annotations on the objects it creates, so you can see which
zone entities a sandbox depends on.

`--service-account` must equal `system:serviceaccount:<namespace>:<user_id>`
for the namespace and user id of this run. Otherwise `new-sandbox` stops
before it renders anything.

`new-sandbox` renders the `sandbox/` chart with `helm template` and applies
the result with `kubectl apply -f -`. With `--dry-run`, it renders the
manifests and does not apply them. It writes the manifests to stdout and
its progress to stderr. `--name` sets the name of the `Sandbox`, which
defaults to the user's public id. `--kube-context` selects a kubeconfig
context.

One run creates three objects in the namespace:

- the ServiceAccount, named for the user's public id
- the ConfigMap `<sandbox>-proxy-config`, which holds the proxy's
  configuration document
- the `Sandbox`

Only the proxy container mounts the ConfigMap.

The endpoints in `my-sandbox.yaml` are the destinations the agent can
reach. Each `resource` is the identifier the proxy sends, unchanged, as
the exchange's `resource` parameter. It must be a Resource in the zone, a
dependency of the egress proxy Application, and a destination in the
user's access policy. `zone-setup` sets up all three for each entry in its
`resources` list. An endpoint without a `credential` is allowed, and the
proxy forwards its requests without a credential.

## Check that the sandbox is running

```sh
kubectl -n <namespace> get sandbox <user_id>
kubectl -n <namespace> logs <user_id> -c egress-proxy
kubectl -n <namespace> exec -it <user_id> -c agent -- sh
```

The `Sandbox`, the pod, and the ServiceAccount all use the user's public id
as their name, unless you passed `--name`.

The proxy logs one decision per request to stdout. Look there to find the
reason for an unexpected denial. The proxy reads its configuration once at
startup. To apply an edit to the ConfigMap, recreate the sandbox.

## The agent does not hold credentials

The agent container does not hold a credential. The zone mints a
credential for this sandbox's user, and the proxy writes it into each
request to a credentialed endpoint. The proxy replaces any
placeholder the agent sent, and adds the credential if the agent sent
none. The chart does not set the `credential.env` variable on the agent
container. The proxy blocks a request to a host that is not in the
endpoints list and logs the denial.

The zone's event log has one `credentials:issue` event per credentialed
request. Each event names the impersonated user and the ServiceAccount
that made the exchange.

## Fallback: zone-setup

Use `zone-setup` from the same repository when this catalog item is not
available. Its code is also the specification of what this item produces.
It creates the same credential and both policies for every user in its
values file. The user must already exist in the zone.

```yaml
users:
  - email: someone@example.com
```

When it runs, it prints the ids `new-sandbox` needs:

```yaml
sandbox_user_access:
- client_id: ...
  namespace: agents
  service_account: system:serviceaccount:agents:<user_id>
  user_email: someone@example.com
  user_id: ...
```

A zone has one active policy set. `zone-setup` puts both policies for
every user in one zone-wide set, publishes a new version, and activates
it. `policy_set` in its values file names the set and defaults to
`Kubernetes Sandbox`. `zone-setup` keeps entries in the active version
that it does not manage. If a different set is active in the zone, it
stops before it writes the policy set. To replace the active set, run it
with `--replace-active-policy-set`.
