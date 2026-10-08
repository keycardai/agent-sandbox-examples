# agent-sandbox-examples

The reference deployment for running an agent in a Kubernetes sandbox whose
outbound HTTPS the Keycard egress proxy mediates.

## Development

The pinned tools install with [mise](https://mise.jdx.dev):

    mise install

`docs/local.md` describes how to run the sandbox with `go run ./cmd/sandbox`
on a local k3d cluster against a Keycard zone. It also needs Docker, from
OrbStack or Docker Desktop.

Run every gate CI runs:

    make all

`AGENTS.md` describes how work gets into the repository.
