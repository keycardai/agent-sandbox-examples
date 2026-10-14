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
the zone (or creates one), the user, which demo to run, the demo's
questions, and how the zone reaches the cluster's issuer. It writes
`local/local.env` and `zone-setup.yaml`, and `local/answers.yaml` for the
demo's answers that are not secrets. Secrets you type go in
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
`sandbox run` then imports them into the k3d cluster. `sandbox run` refuses
the tag `latest`, because Kubernetes always pulls an image with that tag.

To change something, run `go run ./cmd/sandbox init` again. It shows your
current settings and asks what to change: nothing, everything, or one part,
such as the demo or the cluster. Each question defaults to your current
answer. A saved secret stays unless you answer n to "Keep the saved ...?".
Keys `init` doesn't ask about, such as `namespace` and `policy_set`, keep
their values, and so do resources that no scenario lists. `init` drops the
comments in `zone-setup.yaml`. If `zone-setup.yaml` does not parse, `init`
asks every question and replaces the file. The change takes effect on the
next `sandbox up` or `sandbox run`. zone-setup does not delete what it
created in Keycard for your old answers.

Each demo is a scenario in `scenarios/`. The `smoke` scenario runs an idle
agent and the checks. It calls GitHub, so it needs a GitHub token. With a
GitHub provider in the zone, each user signs in to GitHub and their sandbox
acts as them. With a personal access token, every user's sandbox acts as
the token's owner. With an OpenRouter API key, `sandbox check` also has the
agent fetch the key's details from OpenRouter. That call does not run a
model.

`up` runs the steps in order. The tunnel and the mirror run in the
background and log to `local/out/tunnel.log` and `local/out/mirror.log`.
Before each step, `up` prints the command that runs it by hand. If a step
fails or you press Ctrl-C, `up` prints the commands that are left. After the
checks, `up` keeps the tunnel and the mirror running until you press Ctrl-C.
It asks whether to run the checks again, and stops when you answer no. For
a scenario in shell mode, `up` opens the agent in your terminal after the
checks instead. When you exit the agent, `up` asks whether to open it
again, run the checks, or stop. For a scenario in background mode, `up`
follows the agent's log after the checks, and stops if the agent exits. If
the last checks failed, `up` exits with status 1 when it stops. If the
cluster was created with a different issuer, or your kubeconfig has no
context for it, `up` asks to delete the cluster and create it again. The
default answer keeps the cluster.

Before it creates the cluster, `up` asks to remove the containers of an
earlier k3d cluster of the same name that k3d does not list. k3d cannot
create the cluster while they exist. The default answer keeps them.

`up` and `down` ask questions only when stdin is a terminal. If it isn't,
`up` doesn't offer to run the checks again or to open the agent again, it
keeps such a cluster, and it stops at a leftover container. `down` then
deletes only with `--yes`.

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

## Scenarios

A scenario is a directory `scenarios/<name>/` with a `scenario.yaml`, and
optionally a `Dockerfile` and the files it copies. `KEYCARD_LOCAL_SCENARIO`
names the scenario, and `sandbox init` sets it. `scenario.yaml` has these
keys:

| Key | What it does |
|---|---|
| `title` | the name `sandbox init` shows |
| `inputs` | the questions `sandbox init` asks, in order. Each has an `id` of lowercase letters, digits and dashes, and a `kind`. |
| `resources` | the zone-setup resources. Each has an `identifier`, a `name`, and a `credential` that names a `secret` or `github` input. `sandbox init` writes each with `prefix: true`. |
| `endpoints` | the sandbox chart's `endpoints`, as `sandbox/values.yaml` documents them |
| `agent.args` | the agent container's args, which the sandbox-base entrypoint runs |
| `agent.env` | the agent's environment. `{id}` in a value is the answer to the text input `id`. |
| `run.mode` | what `sandbox up` does once the sandbox is up. `checks` runs the checks. `shell` runs the checks, then `run.command` in the agent container, attached to your terminal. `background` runs the checks, then follows the log of the agent that `agent.args` starts. |
| `run.command` | in shell mode, the command `sandbox up` and `sandbox shell` run in the agent container |

The input kinds:

| Kind | What `sandbox init` asks |
|---|---|
| `text` | `prompt`, with `default` filled in. The answer goes in `local/answers.yaml`. |
| `secret` | `prompt`. The answer goes in `local/out/secrets/<id>`, and the resources that name the input read it from there. With `confirm`, `init` first asks that yes or no question, and a no leaves out those resources. |
| `github` | a GitHub provider in the zone, or a personal access token in `local/out/secrets/github-token` |

A resource's identifier must equal the canonical `url` of a credentialed
endpoint. A secret never goes in the agent's environment. The proxy adds it
to the request. If the agent needs the variable set, set it to a
placeholder. `sandbox` refuses a scenario whose `agent.env` names a secret
input.

The checks fetch `https://pypi.org/simple/` and `https://api.github.com/user`
from the agent, so every scenario must list `pypi.org` and GitHub in
`endpoints`.

With a `Dockerfile`, `sandbox run` builds the agent image from the
scenario's directory, tags it `ghcr.io/keycardai/sandbox-<name>:local`, and
imports it into the k3d cluster. The build gets the sandbox-base image as
the `BASE_IMAGE` build argument: the one the charts pin, or
`ghcr.io/keycardai/sandbox-base:<tag>` with `KEYCARD_LOCAL_IMAGE_TAG` set.
The Dockerfile declares `ARG BASE_IMAGE` and builds `FROM ${BASE_IMAGE}`.
The image must keep `sh` and `curl` for the checks. Without a Dockerfile, the agent runs
sandbox-base.

`sandbox run` writes the scenario's endpoints and agent to
`local/out/scenario.values.yaml`, and passes it to both charts after the
other values files. The sandbox controller does not change a pod that
exists. When the sandbox's template changes, such as after you choose
another demo, `run` deletes the pod and the controller creates it again.
`run` also replaces a pod whose agent exited.

## The pi scenario

The `pi` scenario runs the [pi coding agent](https://pi.dev) in your
terminal. pi calls a model on OpenRouter and works in a clone of the
GitHub repository you name. `sandbox init` asks for GitHub access, the
OpenRouter API key, the repository and the model. The agent does not see
either credential. The proxy adds the OpenRouter key to each model request
and the GitHub token to each GitHub request, including `git push`.

The image adds Node.js 24 to sandbox-base, Debian's fd-find, ripgrep, jq,
make, patch, unzip, tree and less, and pi at the version
`scenarios/pi/package-lock.json` pins. To update pi, change its version in
`scenarios/pi/package.json`, run `npm install --package-lock-only
--ignore-scripts` in that directory, and check that each package in
`package-lock.json` has an `integrity` hash.

## The slack scenario

The `slack` scenario runs an agent in the background that answers your
direct messages in Slack, and ignores everyone else. Each message is one
command:

    github whoami
    github issues OWNER/REPO
    github issue OWNER/REPO TITLE
    linear whoami
    linear issues
    linear issue TEAM-KEY TITLE

The agent is a Python script, `scenarios/slack/slack-agent`. It does not
call a model. It reads the direct message with `conversations.history` every
3 seconds, 20 calls a minute, and answers with `chat.postMessage`. Slack
allows an internal app 50 or more `conversations.history` calls a minute
([conversations.history](https://docs.slack.dev/reference/methods/conversations.history)).
The proxy adds the bot token to every call to `https://slack.com/api`.

Before `sandbox init`, create a Slack app for your workspace:

1. Add the bot token scopes `im:write`, `im:history` and `chat:write`.
2. In App Home, allow users to send messages from the Messages tab.
3. Install the app, and copy the bot token, which starts with `xoxb-`.
4. Copy your member ID from your Slack profile.

`sandbox init` also asks for the GitHub token and a Linear personal API
key, which you create in Linear's security settings. The proxy sends the
Linear key in the `Authorization` header without a scheme, as Linear
expects ([Linear API](https://linear.app/developers/graphql)).

## Using an existing cluster

To run the sandbox in an existing cluster, such as EKS, choose its
kubeconfig context in `sandbox init`. `init` saves the context in
`KEYCARD_LOCAL_KUBE_CONTEXT`. The cluster must:

- Publish its ServiceAccount issuer on the internet, as EKS does. `init`
  reads the issuer from the cluster and shows it.
- Pull images from `ghcr.io/keycardai`. With `KEYCARD_LOCAL_IMAGE_TAG` set,
  the cluster pulls the images at that tag instead of the ones the charts
  pin. Nothing publishes the images of scenarios with a Dockerfile, so
  `run` refuses those scenarios on an existing cluster.
- Enforce NetworkPolicy. `sandbox check` fails when the cluster does not.
  The sandbox steps do not change the cluster's network setup.
- Allow the privileged Pod Security level in the sandbox namespace. The
  egress-netsetup init container adds `NET_ADMIN` and `NET_RAW`.

`init` also asks for the cluster's pod and service ranges and saves them in
`KEYCARD_LOCAL_CLUSTER_CIDRS`. The sandbox NetworkPolicy and the proxy
block these ranges.

With an existing cluster, `sandbox up` runs only `check-issuer` and `run`.
In checks mode it stops after the checks. In shell mode it stops when you
choose to stop or press Ctrl-C, or after the agent exits when stdin is not a
terminal. In background mode it stops when you press Ctrl-C or the agent
exits. `run` doesn't create a cluster or import images. `bootstrap`,
`down`, `tunnel` and `mirror` fail. The other steps take the context in
`--cluster`.

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
| `sandbox run` | for k3d, runs `sandbox bootstrap`, and imports the images when `KEYCARD_LOCAL_IMAGE_TAG` is set. Then it runs zone-setup and `sandbox install`, creates the user's sandbox, and runs `sandbox check` |
| `sandbox check` | runs the checks against the running sandbox. To check that the cluster enforces NetworkPolicy, it starts a pod in the sandbox namespace that tries to connect to CoreDNS on a port the policy blocks. kubectl deletes the pod when it exits. The check changes nothing else in the cluster. |
| `sandbox logs` | follows the log of the running sandbox's agent, until you press Ctrl-C or the agent exits |
| `sandbox shell` | runs the scenario's `run.command` in the running sandbox's agent container, or `sh` when it has none, attached to your terminal |
| `sandbox down` | deletes the local cluster. It asks first, unless you pass `--yes`. |

Every ID that `sandbox run` passes on comes from the zone-setup output in
`local/out/zone-setup.yaml`.

## How a pod reaches the zone

`sandbox run` writes `local/out/install.values.yaml` and
`local/out/sandbox.values.yaml`. The charts read them after
`local/values/install.yaml` and `local/values/sandbox.yaml`. They set
`zoneURL`, `zoneTrust` when `KEYCARD_LOCAL_CA_FILE` is set, the images when
`KEYCARD_LOCAL_IMAGE_TAG` is set, and the cluster's ranges for an existing
cluster. The proxy dials the zone at the address its host name resolves to.

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
