# Running the sandbox locally

The scripts under `local/` run the sandbox flow on a workstation against a
real Keycard zone, a real user and real upstreams. Only the Kubernetes
cluster is local. It is a k3d cluster that `bootstrap.sh` creates, named by
`KEYCARD_LOCAL_CLUSTER`. The scripts write only to that cluster and to the
zone.

The scripts create their own cluster because of the ServiceAccount issuer. A
zone fetches `<issuer>/.well-known/openid-configuration` and compares its
`issuer` with the `iss` of each token, byte for byte. k3s mints
`https://kubernetes.default.svc.cluster.local` unless told otherwise, and
`--service-account-issuer` can only be set when the cluster is created.

## Before you start

- Docker, and the tools in `mise.toml` (`mise install`).
- A Keycard zone with an enrolled user. The scripts do not create a zone.
- The three images, built in the egress-proxy repository and tagged
  `ghcr.io/keycardai/egress-netsetup:local`,
  `ghcr.io/keycardai/egress-proxy:local` and
  `ghcr.io/keycardai/sandbox-base:local`.
- A zone-setup values file for the zone. Start from `zone-setup.example.yaml`.
- An https URL for the issuer that you can point at `mirror.sh`. See
  [Publishing the issuer](#publishing-the-issuer).

Then:

    cp local/local.env.example local/local.env
    $EDITOR local/local.env      # the issuer, the zone URL and the user's email
    ./local/bootstrap.sh
    ./local/mirror.sh            # in another terminal. It serves until interrupted.
    ./local/check-issuer.sh      # once the issuer URL reaches the mirror
    ./local/run.sh

`./local/down.sh` deletes the cluster. The zone entities zone-setup created
stay.

## Publishing the issuer

The zone fetches `<issuer>/.well-known/openid-configuration` and
`<issuer>/openid/v1/jwks` for every exchange. `mirror.sh` serves a snapshot of
both over plain HTTP on a local address, `127.0.0.1:8480` unless `--listen`
names another. Point the issuer's https URL at the mirror yourself, for
example with a reverse proxy that terminates TLS for the issuer host and
forwards to the mirror. The zone must be able to resolve and connect to that
host. Every exchange fails while the mirror is down or the URL does not lead
to it.

`check-issuer.sh` fetches the issuer at its https URL, as the zone does, and
verifies a new token against it. `run.sh` does not publish the issuer. The
mirror serves a snapshot of the cluster's signing key, so rerun `mirror.sh`
after you recreate the cluster.

## Extra trusted roots

Set `KEYCARD_LOCAL_CA_FILE` when no public CA signed the zone's or the
issuer's certificate. It is an absolute path to a PEM file of the roots that
did. `check-issuer.sh` passes it to `oidc-check`, and `run.sh` gives it to
the proxy through `zoneTrust`. Both add these roots to the system roots. When
it is empty, they trust only the system roots.

## The scripts

| Script | What it does |
|---|---|
| `common.sh` | the settings, the checks, the Docker gateway and the CA file. Sourced, never run. |
| `bootstrap.sh` | creates the cluster with the issuer, or checks that an existing one has it |
| `mirror.sh` | snapshots the cluster's discovery document and JWKS and serves them with `cmd/oidc-mirror` |
| `check-issuer.sh` | mints a token and verifies it against the published issuer, as the zone does |
| `run.sh` | `bootstrap.sh`, the images, `zone-setup`, `install.sh`, `new-sandbox`, then `checks.sh` |
| `checks.sh` | the end-to-end checks against a running sandbox. It does not write to any cluster. |
| `down.sh` | deletes the cluster, after asking unless `--yes` |

Every id that `run.sh` passes on comes from the zone-setup output in
`local/out/zone-setup.yaml`.

## How a pod reaches the zone

`run.sh` expects the zone to answer on the Docker host. A pod in the k3d
cluster reaches the Docker host at the gateway of the cluster's Docker
network. `run.sh` reads that address on every run and writes it to
`local/out/install.values.yaml` and `local/out/sandbox.values.yaml`, with
the CA file when `KEYCARD_LOCAL_CA_FILE` is set. The charts read those files
after `local/values/install.yaml` and `local/values/sandbox.yaml`.

| Value | What it does |
|---|---|
| `hostAliases` | maps the zone's host to the gateway. The agent's traffic goes to the proxy whatever a name resolves to, so this changes only the address the proxy dials. |
| `zoneTrust` | set only with `KEYCARD_LOCAL_CA_FILE`. It puts the roots in a ConfigMap, mounts it only on the proxy, and adds it to `tls.upstream_ca_files`. |
| `network.allowCIDRs` | allows the gateway on TCP 443 in the NetworkPolicy, which otherwise blocks every private address |
| `policy.denyListExceptions` | lets the proxy dial the gateway. The proxy refuses a zone address on its deny list. |

The tests in `internal/chart` render both charts with these values, with and
without a CA file, and check that the two pods match.
