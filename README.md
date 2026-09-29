# agent-sandbox-poc

The reference deployment for running an agent in a Kubernetes sandbox whose
outbound HTTPS the Keycard egress proxy mediates.

## Development

The pinned tools install with [mise](https://mise.jdx.dev):

    mise install

The local lane also needs Docker, from OrbStack or Docker Desktop.

Run every gate CI runs:

    make all

`AGENTS.md` describes how work gets into the repository.
