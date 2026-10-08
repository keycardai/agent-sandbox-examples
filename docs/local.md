# Running the sandbox locally

`go run ./cmd/sandbox` runs the sandbox demo from your workstation against a
real Keycard zone, a real user and real upstream APIs. Only the Kubernetes
cluster is local. It's a k3d cluster that `sandbox bootstrap` creates, named
by `KEYCARD_LOCAL_CLUSTER`. The steps change only that cluster and the zone.
Each step reads its settings from `local/local.env` and writes its output
under `local/out/`.

The steps create their own cluster because of the ServiceAccount issuer. A
zone fetches `<issuer>/.well-known/openid-configuration` and compares its
`issuer` with the `iss` of each token, byte for byte. k3s mints
`https://kubernetes.default.svc.cluster.local` unless told otherwise, and
`--service-account-issuer` can only be set when the cluster is created.

## Before you start

- Docker, and the tools in `mise.toml` (`mise install`).
- The client ID and secret of a Keycard organization service account.
- A user in the zone for the sandbox to act for. The management API can't
  create users. Add one in the Keycard console, or have the person sign in
  to the zone.
- The three images, built in the egress-proxy repository and tagged
  `ghcr.io/keycardai/egress-netsetup:local`,
  `ghcr.io/keycardai/egress-proxy:local` and
  `ghcr.io/keycardai/sandbox-base:local`.

Then:

    go run ./cmd/sandbox up

The first time, `up` runs `sandbox init`. It asks for your service account,
the zone (or creates one), the user, where the GitHub token comes from, an
optional OpenRouter API key, and how the zone reaches the cluster's issuer.
It writes `local/local.env` and `zone-setup.yaml`. Secrets you type go in
`local/out/secrets/`, readable only by you. If
`KEYCARD_MANAGEMENT_CLIENT_SECRET` is set, `init` uses it and doesn't ask for
the secret. To write the files by hand, start from
`local/local.env.example` and `zone-setup.example.yaml`.

To change something, run `go run ./cmd/sandbox init` again. It shows your
current settings and asks what to change: nothing, everything, or one part,
such as the GitHub token or the cluster. It asks each question with your
current answer as the default, and keeps a saved secret unless you answer n
to "Keep the saved …?". The change takes effect on the next `sandbox up`
or `sandbox run`. zone-setup does not delete what it created in Keycard for
your old answers.

The demo calls GitHub, so it needs a GitHub token. With a GitHub provider in
the zone, each user signs in to GitHub and their sandbox acts as them. With
a personal access token, every user's sandbox acts as the token's owner.

`up` runs the steps in order. The tunnel and the mirror run in the
background and log to `local/out/tunnel.log` and `local/out/mirror.log`.
Before each step, `up` prints the command that runs it by hand. If a step
fails or you press Ctrl-C, `up` prints the commands that are left. After the
checks, `up` keeps the tunnel and the mirror running until you press Ctrl-C.
Press Enter to run the checks again. If the cluster was created with a
different issuer, `up` asks to delete the cluster and create it again.
Press Enter to accept, or answer n to keep the cluster.

`sandbox up --print` prints the commands without running them:

    go run ./cmd/sandbox tunnel        # in its own terminal, until you stop it
    go run ./cmd/sandbox bootstrap
    go run ./cmd/sandbox mirror        # in another terminal, until you stop it
    go run ./cmd/sandbox check-issuer
    go run ./cmd/sandbox run

A quick tunnel's URL lasts only as long as its process, and the cluster
keeps the issuer it was created with. To run the steps by hand after `up`,
start `sandbox tunnel` in its own terminal first. `up` uses that tunnel, and
the cluster keeps working after `up` stops.

zone-setup stops if another policy set is active. Run `sandbox run` with
`--replace-active-policy-set` to replace it for everyone in the zone.
`sandbox run --skip-zone-setup` reuses the Keycard setup of the last run, in
`local/out/zone-setup.yaml`. To run the checks again against the running
sandbox, use `go run ./cmd/sandbox check`.

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
does, and verifies a new token against it. `sandbox run` doesn't publish the
issuer. The mirror serves the cluster's signing key from when it started,
so restart `sandbox mirror` after you create the cluster again.

## Extra trusted roots

Set `KEYCARD_LOCAL_CA_FILE` when no public CA signed the certificate of the
management API, the zone or the issuer. Set it to the absolute path of a
PEM file with the roots that signed them. `sandbox init` trusts these roots
for the management API, `sandbox check-issuer` for the issuer, and the proxy
for the zone, through the chart's `zoneTrust` value. Each trusts them in
addition to the system roots. `sandbox init` asks for this file only when
the management API URL is not `https://api.keycard.ai`.

## The steps

| Step | What it does |
|---|---|
| `sandbox init` | asks for the zone, the user and the credentials, and writes `local/local.env` and `zone-setup.yaml` |
| `sandbox up` | `sandbox init` when its files are missing, then every step below, in order, in one process |
| `sandbox tunnel` | publishes the mirror at the issuer's https URL through a Cloudflare tunnel |
| `sandbox bootstrap` | creates the cluster with the issuer, or checks that an existing one has it |
| `sandbox mirror` | reads the cluster's discovery document and JWKS and serves them |
| `sandbox check-issuer` | mints a token and verifies it against the published issuer, as the zone does |
| `sandbox run` | `sandbox bootstrap`, the images, zone-setup, `install.sh`, the user's sandbox, then `sandbox check` |
| `sandbox check` | the end-to-end checks against a running sandbox. It does not write to any cluster. |
| `sandbox down` | deletes the cluster, after asking unless `--yes` |

Every id that `sandbox run` passes on comes from the zone-setup output in
`local/out/zone-setup.yaml`.

## How a pod reaches the zone

`sandbox run` writes `local/out/install.values.yaml` and
`local/out/sandbox.values.yaml`. The charts read them after
`local/values/install.yaml` and `local/values/sandbox.yaml`. They set
`zoneURL`, and `zoneTrust` when `KEYCARD_LOCAL_CA_FILE` is set. The proxy
dials the zone at the address its host name resolves to.

Set `KEYCARD_LOCAL_ZONE_ON_HOST=true` for a zone that is not on the
internet and answers on the Docker host. A pod in the k3d cluster
reaches the Docker host at the gateway of the cluster's Docker network.
`sandbox run` reads that address on every run and adds these values:

| Value | What it does |
|---|---|
| `hostAliases` | maps the zone's host to the gateway. The agent's traffic goes to the proxy whatever a name resolves to, so this changes only the address the proxy dials. |
| `network.allowCIDRs` | allows the gateway on TCP 443 in the NetworkPolicy, which otherwise blocks every private address |
| `policy.denyListExceptions` | lets the proxy dial the gateway. The proxy refuses a zone address on its deny list. |

`zoneTrust` puts the roots from `KEYCARD_LOCAL_CA_FILE` in a ConfigMap,
mounts it only on the proxy, and adds it to `tls.upstream_ca_files`.

The tests in `internal/chart` render both charts with the Docker host
values, with and without a CA file, and check that the two pods match.
