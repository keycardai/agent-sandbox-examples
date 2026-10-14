# agent-sandbox-examples

The reference deployment for running an agent in a Kubernetes sandbox whose
outbound HTTPS the Keycard egress proxy mediates.

## Development

The pinned tools install with [mise](https://mise.jdx.dev):

    mise install

To try the sandbox, run:

    go run ./cmd/sandbox up

It asks for what it needs, then sets up a local k3d cluster against your
Keycard zone. It also needs Docker, from OrbStack or Docker Desktop.
`docs/local.md` describes each step.

Run every gate CI runs:

    make all

`AGENTS.md` describes how work gets into the repository.
