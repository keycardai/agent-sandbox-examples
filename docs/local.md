# Local cluster

The scripts in `local/` run the sandbox flow on a workstation against a real
Keycard zone, a real user and real upstreams. Only the cluster is local.

Two clusters take part.

| Cluster | What it is | What the scripts do to it |
|---|---|---|
| `localdev` | the Keycard stack, with the Gateway for every `*.localdev.keycard.sh` host | Nothing. **NEVER write to it.** Every script refuses the name. |
| `KEYCARD_LOCAL_CLUSTER` | a k3d cluster for the sandbox | create it, install into it, delete it |

The sandbox needs its own cluster because of the ServiceAccount issuer. A
zone fetches `<issuer>/.well-known/openid-configuration` and compares its
`issuer` with the `iss` of each token, byte for byte. k3s mints
`https://kubernetes.default.svc.cluster.local` unless told otherwise, and
`--service-account-issuer` can only be set when the cluster is created.

## Before you start

- Docker, and the tools in `mise.toml` (`mise install`).
- The localdev stack, running, with a zone and an enrolled user. The scripts
  do not create a zone.
- The three images, built in the egress-proxy repository and tagged
  `ghcr.io/keycardai/egress-netsetup:local`,
  `ghcr.io/keycardai/egress-proxy:local` and
  `ghcr.io/keycardai/sandbox-base:local`.
- A zone-setup values file for the zone. Start from `zone-setup.example.yaml`.

Then:

    cp local/local.env.example local/local.env
    $EDITOR local/local.env      # the zone URL and the user's email
    ./local/bootstrap.sh
    ./local/mirror.sh            # in another terminal. It serves until interrupted.
    ./local/route.sh             # read the output, then apply it to localdev yourself
    ./local/check-issuer.sh
    ./local/run.sh

The zone fetches the issuer at `KEYCARD_LOCAL_ISSUER` for every exchange.
`mirror.sh` serves it. The route that `route.sh` prints sends the issuer host
from the localdev Gateway to the mirror. Every exchange fails while either
one is missing. `run.sh` does not publish the issuer. The mirror serves a
snapshot of the cluster's signing key, so rerun `mirror.sh` after you
recreate the cluster.

`./local/down.sh` deletes the k3d cluster. The zone entities that zone-setup
created stay.

## The scripts

| Script | What it does |
|---|---|
| `lane.sh` | the settings, the guards, the Docker gateway and the localdev root. Sourced, never run. |
| `bootstrap.sh` | creates the cluster with the issuer, or checks that an existing one has it |
| `mirror.sh` | snapshots the cluster's discovery document and JWKS and serves them with `cmd/oidc-mirror` |
| `route.sh` | prints the Service, EndpointSlice and HTTPRoute that send the issuer host to the mirror. It does not apply them. |
| `check-issuer.sh` | mints a token and verifies it against the published issuer, as the zone does |
| `run.sh` | `bootstrap.sh`, the images, `zone-setup`, `install.sh`, `new-sandbox`, then `checks.sh` |
| `checks.sh` | the end-to-end checks against a running sandbox. It does not write to any cluster. |
| `down.sh` | deletes the k3d cluster, after asking unless `--yes` |

Every id that `run.sh` passes on comes from the zone-setup output in
`local/out/zone-setup.yaml`.

## How a pod reaches the zone

The zone runs behind the localdev Gateway on the Docker host. A pod in the
k3d cluster reaches the Docker host at the gateway of the cluster's Docker
network. On every run, `run.sh` reads that address and the localdev root
certificate and writes them to `local/out/install.values.yaml` and
`local/out/sandbox.values.yaml`. The charts read those files after
`local/values/install.yaml` and `local/values/sandbox.yaml`.

| Value | What it does |
|---|---|
| `hostAliases` | maps the zone's host to the gateway. The agent's traffic goes to the proxy whatever a name resolves to, so this changes only the address the proxy dials. |
| `zoneTrust` | puts the localdev root in a ConfigMap, mounts it only on the proxy, and adds it to `tls.upstream_ca_files` |
| `network.allowCIDRs` | allows the gateway on TCP 443 in the NetworkPolicy, which otherwise blocks every private address |
| `policy.denyListExceptions` | lets the proxy dial the gateway. The proxy refuses a zone address on its deny list. |

The tests in `internal/chart` render both charts with these values and check
that the two pods match.
