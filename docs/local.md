# Running the sandbox locally

The steps under `local/` and `cmd/sandbox` run the sandbox flow on a
workstation against a real Keycard zone, a real user and real upstreams.
Only the Kubernetes cluster is local. It is a k3d cluster that
`sandbox bootstrap` creates, named by `KEYCARD_LOCAL_CLUSTER`. The steps write
only to that cluster and to the zone.

The steps create their own cluster because of the ServiceAccount issuer. A
zone fetches `<issuer>/.well-known/openid-configuration` and compares its
`issuer` with the `iss` of each token, byte for byte. k3s mints
`https://kubernetes.default.svc.cluster.local` unless told otherwise, and
`--service-account-issuer` can only be set when the cluster is created.

## Before you start

- Docker, and the tools in `mise.toml` (`mise install`).
- A Keycard zone with an enrolled user. The steps do not create a zone.
- The three images, built in the egress-proxy repository and tagged
  `ghcr.io/keycardai/egress-netsetup:local`,
  `ghcr.io/keycardai/egress-proxy:local` and
  `ghcr.io/keycardai/sandbox-base:local`.
- A zone-setup values file for the zone. Start from `zone-setup.example.yaml`.
  `run.sh` passes the cluster's issuer to zone-setup with `--issuer-url`,
  which replaces `issuer_url` in the file.

Then:

    cp local/local.env.example local/local.env
    $EDITOR local/local.env            # the zone URL and the user's email
    go run ./cmd/sandbox tunnel        # in its own terminal, until you stop it
    go run ./cmd/sandbox bootstrap
    go run ./cmd/sandbox mirror        # in another terminal, until you stop it
    go run ./cmd/sandbox check-issuer
    ./local/run.sh

zone-setup stops if another policy set is active. Run `run.sh` with
`--replace-active-policy-set` to replace it for everyone in the zone.

`go run ./cmd/sandbox down` deletes the cluster. What zone-setup created in
the zone stays.

## Publishing the issuer

The zone fetches `<issuer>/.well-known/openid-configuration` and
`<issuer>/openid/v1/jwks` for every exchange. `sandbox mirror` serves both
over plain HTTP on a local address, `127.0.0.1:8480` unless `--listen` names
another. `sandbox tunnel` gives that address a public https URL through a
Cloudflare tunnel. Every exchange fails while the mirror or the tunnel is
down.

With `KEYCARD_LOCAL_TUNNEL` and `KEYCARD_LOCAL_ISSUER` empty, `sandbox
tunnel` starts a quick tunnel. It doesn't need a Cloudflare account.
Cloudflare says quick tunnels have no uptime guarantee and are for testing
and development
([Quick Tunnels](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/)).
The tunnel gets a new `https://<name>.trycloudflare.com` URL every time it
starts. `sandbox tunnel` writes it to `local/out/issuer` and deletes the
file when it exits. The other steps read the issuer from that file. The
issuer is fixed when the cluster is created, so start `sandbox tunnel`
before `sandbox bootstrap`. After a restart, run `sandbox down` and
`sandbox bootstrap` again. zone-setup then creates a new provider for the
new issuer, and keeps the old one.

To keep one URL, use a named tunnel in your Cloudflare account. Its
hostname must be in a zone whose DNS Cloudflare serves. Set it up once:

    cloudflared tunnel login
    cloudflared tunnel create <name>
    cloudflared tunnel route dns <name> <hostname>

Then set `KEYCARD_LOCAL_TUNNEL=<name>` and
`KEYCARD_LOCAL_ISSUER=https://<hostname>`. `sandbox tunnel` runs the tunnel
and forwards it to the mirror. It does not need a cloudflared config file.

To publish the issuer another way, set `KEYCARD_LOCAL_ISSUER` and do not run
`sandbox tunnel`. Forward the issuer's https URL to the mirror, for example
with a reverse proxy that terminates TLS for the issuer host.

`sandbox check-issuer` fetches the issuer at its https URL, as the zone
does, and verifies a new token against it. `run.sh` does not publish the
issuer. The mirror serves the cluster's signing key as it was when the
mirror started, so run `sandbox mirror` again after you recreate the
cluster.

## Extra trusted roots

Set `KEYCARD_LOCAL_CA_FILE` when no public CA signed the zone's or the
issuer's certificate. It is an absolute path to a PEM file of the roots that
did. `sandbox check-issuer` trusts them, and `run.sh` gives them to the
proxy through `zoneTrust`. Both add these roots to the system roots. When it
is empty, they trust only the system roots.

## The steps

| Step | What it does |
|---|---|
| `local/common.sh` | the settings, the checks, the Docker gateway and the CA file. Sourced, never run. |
| `sandbox tunnel` | publishes the mirror at the issuer's https URL through a Cloudflare tunnel |
| `sandbox bootstrap` | creates the cluster with the issuer, or checks that an existing one has it |
| `sandbox mirror` | reads the cluster's discovery document and JWKS and serves them |
| `sandbox check-issuer` | mints a token and verifies it against the published issuer, as the zone does |
| `run.sh` | `sandbox bootstrap`, the images, `zone-setup`, `install.sh`, `new-sandbox`, then `checks.sh` |
| `checks.sh` | the end-to-end checks against a running sandbox. It does not write to any cluster. |
| `sandbox down` | deletes the cluster, after asking unless `--yes` |

Every id that `run.sh` passes on comes from the zone-setup output in
`local/out/zone-setup.yaml`.

## How a pod reaches the zone

`run.sh` writes `local/out/install.values.yaml` and
`local/out/sandbox.values.yaml`. The charts read them after
`local/values/install.yaml` and `local/values/sandbox.yaml`. They set
`zoneURL`, and `zoneTrust` when `KEYCARD_LOCAL_CA_FILE` is set. The proxy
dials the zone at the address its host name resolves to.

Set `KEYCARD_LOCAL_ZONE_ON_HOST=true` for a zone that is not on the
internet and answers on the Docker host. A pod in the k3d cluster
reaches the Docker host at the gateway of the cluster's Docker network.
`run.sh` reads that address on every run and adds these values:

| Value | What it does |
|---|---|
| `hostAliases` | maps the zone's host to the gateway. The agent's traffic goes to the proxy whatever a name resolves to, so this changes only the address the proxy dials. |
| `network.allowCIDRs` | allows the gateway on TCP 443 in the NetworkPolicy, which otherwise blocks every private address |
| `policy.denyListExceptions` | lets the proxy dial the gateway. The proxy refuses a zone address on its deny list. |

`zoneTrust` puts the roots from `KEYCARD_LOCAL_CA_FILE` in a ConfigMap,
mounts it only on the proxy, and adds it to `tls.upstream_ca_files`.

The tests in `internal/chart` render both charts with the Docker host
values, with and without a CA file, and check that the two pods match.
