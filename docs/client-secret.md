# Running the sandbox with a client secret

By default, the egress proxy authenticates to the zone with the pod's
ServiceAccount token, and the zone's policy ties each token to one user. In
client secret mode, the proxy authenticates with a client ID and secret
instead, so the zone does not need to read the cluster's OIDC issuer.

Use this mode only for a demo. Every sandbox shares one secret, and the
exchange does not name a ServiceAccount. The zone cannot tell one sandbox
from another, so anyone who holds the secret can act as every enrolled user.
See "Known limits" in the egress-proxy repository's
`docs/security-model.md`.

## With `sandbox up`

Set `KEYCARD_LOCAL_CLIENT_SECRET=true` and run the demo as `docs/local.md`
describes:

    KEYCARD_LOCAL_CLIENT_SECRET=true go run ./cmd/sandbox up

Only the environment sets this. The settings file does not hold it, so set
it again for each command, such as `sandbox run` or `sandbox check`.

In this mode:

- `up` runs only `bootstrap` and `run`. It does not start a tunnel or the
  mirror, and it does not run `check-issuer`. The k3d cluster gets the
  issuer `https://kubernetes.default.svc.cluster.local`, unless
  `KEYCARD_LOCAL_ISSUER` names another one. The zone never reads it.
- zone-setup does not create the issuer's provider. It gives the
  Application one password credential and writes its client ID and secret
  to `local/out/client-secret.json`, readable only by you. A later run
  reuses that credential while the Application still has it. The zone shows
  the secret only when it creates the credential, so if you delete the file,
  the next run creates another credential.
- `run` stores the secret in the Secret `egress-proxy-client-secret` in the
  sandbox namespace, and each sandbox reads it. A running pod keeps the
  secret it started with.

An existing cluster, named by `KEYCARD_LOCAL_KUBE_CONTEXT`, works the same
way. `up` runs only `run` there.

## By hand

1. In the zone-setup values file, set `application_credential:
   client_secret`. `issuer_url` is then optional. Run zone-setup with a file
   to keep the secret in:

       zone-setup --values zone-setup.yaml --client-secret-file ./client-secret.json \
         > zone-setup.out.yaml

2. Create the Secret in the sandbox namespace, under the key
   `client-secret`:

       kubectl create secret generic egress-proxy-client-secret --namespace agents \
         --from-literal=client-secret="$(jq -r .client_secret client-secret.json)"

3. Create each sandbox with `--client-secret-name`. Leave out
   `--service-account` and `--provider-id`, which zone-setup does not output
   in this mode:

       new-sandbox --user-id <user_id> --client-id <client_id> \
         --client-secret-name egress-proxy-client-secret --values my-values.yaml

## What changes in the zone

Both policies keep their names, so a run in one mode replaces the policies
of the other mode.

- The impersonation policy permits the Application only when it
  authenticates with a password credential. It does not check the
  ServiceAccount.
- The access policy permits the user without a condition on the actor. Every
  exchange also needs the impersonation policy, which limits it to the
  Application's dependencies.

zone-setup does not delete the issuer's provider or the token credentials of
an earlier run in workload identity mode.

## What changes in the pod

The proxy refuses a secret file that a group or another user can read. The
kubelet makes a Secret volume's files owned by root, so the proxy, which
runs as uid 1337, cannot read a file with mode 0400. An init container named
`client-secret` therefore runs as uid 1337, reads the Secret, and writes it
with mode 0400 to a memory-backed volume that the proxy mounts. Only that
init container mounts the Secret. The agent container mounts neither volume.

Only the sandbox chart has this mode. The install chart's SandboxTemplate
still uses the ServiceAccount token.
