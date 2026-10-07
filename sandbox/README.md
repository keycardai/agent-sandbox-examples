# sandbox

This chart creates one sandbox for one user. `new-sandbox` renders it once
per user into three objects in the sandbox namespace:

| Object | Name | What it is |
|---|---|---|
| ServiceAccount | the user's public id | the subject the workload-identity credential is bound to |
| ConfigMap | `<sandbox>-proxy-config` | the proxy's configuration document: the identity, the cluster ranges and the endpoints |
| Sandbox | the user's public id, or `--name` | the pod of the install chart's `SandboxTemplate`, running as that ServiceAccount |

    cp sandbox/values.example.yaml my-sandbox.yaml
    $EDITOR my-sandbox.yaml          # zone URL, cluster ranges, endpoints
    go run ./cmd/new-sandbox --values my-sandbox.yaml \
      --namespace agents \
      --user-id <user_id> --user-email <user_email> \
      --service-account <service_account> --client-id <client_id> \
      --provider-id <provider_id> --application-id <application_id> \
      --application-identifier <application_identifier>

The identity flags take the zone-setup outputs, under the names zone-setup
prints. `new-sandbox` applies the result with the pinned `kubectl apply -f -`.
With `--dry-run`, it renders and does not apply. The manifests go to stdout
and progress goes to stderr.

`--service-account` must be `system:serviceaccount:<namespace>:<user_id>`, the
subject the credential is bound to. If it does not match, `new-sandbox` stops
before rendering. The zone would refuse every exchange from that sandbox with
`access_denied`.

`values.yaml` documents every key. The admin sets the zone URL, the cluster
ranges and the endpoints. The pod keys have the same meaning as in
`install/values.yaml`.

## Both charts define the pod

An `agents.x-k8s.io/v1beta1` `Sandbox` has its own pod spec. It does not have
a field that names a `SandboxTemplate`. In agent-sandbox v1.0.4, the version in
`install/agent-sandbox/`, only `SandboxWarmPool.spec.sandboxTemplateRef` refers
to a template. Every replica of a warm pool runs as the one ServiceAccount the
template names. Each user needs their own ServiceAccount, so this chart
renders the whole pod.

The tests in `internal/chart` render both charts and check that the two pod
specs differ only in the ServiceAccount and the ConfigMap name.

## A zone that is not on the internet

Give `zoneTrust` and `hostAliases` the values the install chart has.
A non-empty `zoneTrust.configMap` mounts the root certificate on the proxy and
writes its path into the document's `tls.upstream_ca_files`. The proxy
refuses to dial a zone address on its deny list, so a private zone address
also goes in `policy.denyListExceptions`.

## Checking the configuration document

`egress-proxy` does not have a subcommand that validates a document. Running
an egress-proxy build here would also make this repository depend on that
one. `internal/chart/document_test.go` holds a copy of the rules in
egress-proxy's `internal/config/config.go` and `docs/configuration.md`, and
the tests check the rendered document against it. The decode is strict: a
key the proxy does not read is an error. A table test checks that the copy
refuses one broken document per rule. Replace the copy with the proxy's own
check when one exists.

## Assumptions

| Item | What this chart does | Why it is an assumption |
|---|---|---|
| `sandbox.id` | the `Sandbox` name, written into the document | Each sandbox has its own document, so the id is written in. The proxy does not read it through the Downward API. |
| `Sandbox` fields | `spec.service: false` and `spec.podTemplate` | Read from the CRD in `install/agent-sandbox/`, agent-sandbox v1.0.4. Nothing in the cluster calls a sandbox. |

## Open: the agent's placeholder environment

The agent holds a placeholder where a credential goes. The proxy replaces
the placeholder before the request leaves the pod, so the agent may send any
string. Nothing in the pod sets that string in a variable such as
`GITHUB_TOKEN`.

The agent container mounts only `/shared-ca`. The `sandbox-base` entrypoint
in keycardai/egress-proxy waits for the proxy's CA certificate, installs it
and runs the agent. It does not read a configuration document. A placeholder
is not a secret, so a literal `env:` value on the agent container would
work. The egress-proxy owners choose the mechanism.
