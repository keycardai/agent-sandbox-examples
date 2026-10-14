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

Then:

    go run ./cmd/sandbox up

The first time, `up` runs `sandbox init`. It asks for your service account,
the zone (or creates one), the user, where the GitHub token comes from, an
optional OpenRouter API key, and how the zone reaches the cluster's issuer.
It writes `local/local.env` and `zone-setup.yaml`. Secrets you type go in
`local/out/secrets/`, readable only by you. `init` doesn't ask for the
service account secret when an environment variable holds it:
`KEYCARD_MANAGEMENT_CLIENT_SECRET`, or the variable that `client_secret_env`
in `zone-setup.yaml` names. A `client_secret_file` in `zone-setup.yaml`
takes precedence over both. To write the files by hand, start from
`local/local.env.example` and `zone-setup.example.yaml`.

The cluster pulls `egress-netsetup`, `egress-proxy` and `sandbox-base` from
`ghcr.io/keycardai`, at the release digests the charts pin. To run images
you built in the egress-proxy repository, tag them
`ghcr.io/keycardai/<name>:local` and set `KEYCARD_LOCAL_IMAGE_TAG=local`.
`sandbox run` then imports them into the k3d cluster.

To change something, run `go run ./cmd/sandbox init` again. It shows your
current settings and asks which part to change. Each question defaults to
your current answer. A saved secret stays unless you answer n to "Keep the
saved ...?". Keys `init` doesn't ask about, such as `namespace` and
`policy_set`, keep their values. `init` drops the comments in
`zone-setup.yaml`. If `zone-setup.yaml` does not parse, `init` asks every
question and replaces the file. The change takes effect on the next
`sandbox up` or `sandbox run`. zone-setup does not delete what it created in
Keycard for your old answers.

The demo calls GitHub, so it needs a GitHub token. With a GitHub provider in
the zone, each user signs in to GitHub and their sandbox acts as them. With
a personal access token, every user's sandbox acts as the token's owner.
If you add an OpenRouter API key, `sandbox check` also has the agent fetch
the key's details from OpenRouter. This call does not run a model.

`up` runs the steps in order. The tunnel and the mirror run in the
background and log to `local/out/tunnel.log` and `local/out/mirror.log`.
Before each step, `up` prints the command that runs it by hand. If a step
fails or you press Ctrl-C, `up` prints the commands that are left. After the
checks, `up` keeps the tunnel and the mirror running until you press Ctrl-C.
It asks whether to run the checks again, and stops when you answer no. If
the last checks failed, `up` exits with status 1 when it stops. If the
cluster was created with a different issuer, `up` asks to delete the cluster
and create it again. The default answer keeps the cluster.

`up` and `down` ask questions only when stdin is a terminal. If it isn't,
`up` doesn't offer to run the checks again, and it keeps a cluster with a
different issuer. `down` then deletes only with `--yes`.

`sandbox up --print` prints the commands without running them:

    go run ./cmd/sandbox tunnel        # in its own terminal, until you stop it
    go run ./cmd/sandbox bootstrap
    go run ./cmd/sandbox mirror        # in another terminal, until you stop it
    go run ./cmd/sandbox check-issuer
    go run ./cmd/sandbox run

When `up` stops, it stops the tunnel and the mirror, and the zone can't read
the cluster's issuer until they run again. A quick tunnel gets a new URL
every time it starts, but the cluster keeps the issuer it was created with.
To keep one quick tunnel across restarts of `up`, start `sandbox tunnel` in
its own terminal first. `up` uses a quick tunnel that is already running.
With a named tunnel, `up` always runs cloudflared itself, and the URL stays
the same.

zone-setup stops if another policy set is active. Run `sandbox run` with
`--replace-active-policy-set` to replace it for everyone in the zone.
`sandbox run --skip-zone-setup` reuses the Keycard setup of the last run, in
`local/out/zone-setup.yaml`. To run the checks again against the running
sandbox, use `go run ./cmd/sandbox check`.

`go run ./cmd/sandbox down` deletes the cluster. It does not delete what
zone-setup created in the zone.

## Using an existing cluster

To run the sandbox in an existing cluster, such as EKS, choose its
kubeconfig context in `sandbox init`. `init` saves the context in
`KEYCARD_LOCAL_KUBE_CONTEXT`. The cluster must:

- Publish its ServiceAccount issuer on the internet, as EKS does. `init`
  reads the issuer from the cluster and shows it.
- Pull images from `ghcr.io/keycardai`. With `KEYCARD_LOCAL_IMAGE_TAG` set,
  the cluster pulls the images at that tag instead of the ones the charts
  pin.
- Enforce NetworkPolicy. `sandbox check` fails when the cluster does not.
  The sandbox steps do not change the cluster's network setup.
- Allow the privileged Pod Security level in the sandbox namespace. The
  egress-netsetup init container adds `NET_ADMIN` and `NET_RAW`.

`init` also asks for the cluster's pod and service ranges and saves them in
`KEYCARD_LOCAL_CLUSTER_CIDRS`. The sandbox NetworkPolicy and the proxy
block these ranges.

With an existing cluster, `sandbox up` runs only `check-issuer` and `run`,
and stops after the checks. `run` doesn't create a cluster or import images.
`bootstrap`, `down`, `tunnel` and `mirror` fail. The other steps take the
context in `--cluster`.

## Publishing the issuer

The zone fetches `<issuer>/.well-known/openid-configuration` and
`<issuer>/openid/v1/jwks` for every exchange. `sandbox mirror` serves both
over plain HTTP on a local address, `127.0.0.1:8480` unless `--listen` names
another. `sandbox tunnel` gives that address a public https URL through a
Cloudflare tunnel. Every exchange fails while the mirror or the tunnel is
down.

With `KEYCARD_LOCAL_TUNNEL` and `KEYCARD_LOCAL_ISSUER` empty,
`sandbox tunnel` starts a quick tunnel. It doesn't need a Cloudflare account.
Cloudflare says quick tunnels have no uptime guarantee and are for testing
and development
([Quick Tunnels](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/do-more-with-tunnels/trycloudflare/)).
The tunnel gets a new `https://<name>.trycloudflare.com` URL every time it
starts. `sandbox tunnel` writes it to `local/out/issuer`, locks the file
while it runs, and deletes the file when it exits. The lock goes away when
the tunnel's process exits, even when it's killed. The other steps read the
issuer from that file, and refuse it when it isn't locked. `sandbox tunnel`
refuses to start while another tunnel has the file locked, and deletes a
file left by a killed tunnel. The issuer is fixed when the cluster is
created, so start `sandbox tunnel`
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
with a reverse proxy that terminates TLS for the issuer host. A proxy in a
container reaches the host at `host.docker.internal`
([OrbStack](https://docs.orbstack.dev/docker/network)). In testing on
OrbStack, that address also reaches the mirror's default `127.0.0.1`
listener, but OrbStack does not document it. Docker Desktop is untested. If
the proxy cannot connect to the mirror, pass `--listen 0.0.0.0:8480`. The
mirror serves only the two public documents.

`sandbox check-issuer` fetches the issuer at its https URL, as the zone
does, and verifies a new token against it. `sandbox run` does not publish the
issuer. `sandbox mirror` reads the signing key once, at start. Run it again
after you recreate the cluster.

## Extra trusted roots

Set `KEYCARD_LOCAL_CA_FILE` when no public CA signed the certificate of the
management API, the zone or the issuer. Set it to the absolute path of a
PEM file with the roots that signed them. `sandbox init` and `sandbox run`
trust these roots for the management API. `sandbox check-issuer` trusts them
for the issuer. The proxy trusts them for the zone, through the chart's
`zoneTrust` value. Each trusts them in addition to the system roots. `sandbox init` asks for this file only when
the management API URL is not `https://api.keycard.ai`.

## The steps

| Step | What it does |
|---|---|
| `sandbox init` | asks for your Keycard service account, zone, user and cluster, and writes `local/local.env` and `zone-setup.yaml` |
| `sandbox up` | runs `sandbox init` if the settings files are missing, then `tunnel`, `bootstrap`, `mirror`, `check-issuer` and `run`, in order, in one process |
| `sandbox tunnel` | publishes the cluster's issuer on the internet through a Cloudflare tunnel |
| `sandbox bootstrap` | creates the local k3d cluster with the issuer, or checks the one you have |
| `sandbox mirror` | serves the cluster's issuer documents for the tunnel |
| `sandbox check-issuer` | checks that a Keycard zone can verify the cluster's tokens |
| `sandbox run` | runs `sandbox bootstrap`, imports the images, runs zone-setup and `sandbox install`, creates the user's sandbox, then runs `sandbox check` |
| `sandbox check` | runs the checks against the running sandbox. To check that the cluster enforces NetworkPolicy, it starts a pod in the sandbox namespace that tries to connect to CoreDNS on a port the policy blocks. kubectl deletes the pod when it exits. The check changes nothing else in the cluster. |
| `sandbox down` | deletes the local cluster. It asks first, unless you pass `--yes`. |

Every ID that `sandbox run` passes on comes from the zone-setup output in
`local/out/zone-setup.yaml`.

## How a pod reaches the zone

`sandbox run` writes `local/out/install.values.yaml` and
`local/out/sandbox.values.yaml`. The charts read them after
`local/values/install.yaml` and `local/values/sandbox.yaml`. They set
`zoneURL`, and `zoneTrust` when `KEYCARD_LOCAL_CA_FILE` is set. The proxy
dials the zone at the address its host name resolves to.

Set `KEYCARD_LOCAL_ZONE_ON_HOST=true` for a zone that is not on the
internet and answers on the Docker host. `sandbox init` sets it when the
zone's host resolves to a loopback address. For a private address, `init`
asks. A pod in the k3d cluster
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
