# agent-sandbox-poc

The reference deployment for running an agent in a Kubernetes sandbox whose
outbound HTTPS the Keycard egress proxy mediates.

## Development

The pinned tools install with [mise](https://mise.jdx.dev):

    mise install

The scripts under `local/` run the sandbox on a local k3d cluster against a
Keycard zone. They also need Docker, from OrbStack or Docker Desktop.
`docs/local.md` describes how to run them.

Run every gate CI runs:

    make all

`AGENTS.md` describes how work gets into the repository.
