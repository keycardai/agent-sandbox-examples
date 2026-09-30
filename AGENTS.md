# AGENTS.md

Reference deployment for running an agent in a Kubernetes sandbox whose
outbound HTTPS the Keycard egress proxy mediates.

## Commands

- `make all` runs every gate CI runs: build, `go test -race`, golangci-lint,
  govulncheck and `go mod tidy -diff`. A change is done when it passes.
- `make fmt` applies the formatters `.golangci.yml` enables.
- Go tools are `tool` directives in `go.mod`, run as `go tool <name>`. NEVER run
  a gate tool from `$PATH`.
- Every other tool except Docker is pinned in `mise.toml`, with its checksums
  in `mise.lock`. Run `mise install`. Docker comes from OrbStack or Docker
  Desktop. Add any other new tool to `mise.toml`, never as a requirement on
  `$PATH`.
- Scripts and tests run those tools through `scripts/pinned`, which resolves
  them through mise against this `mise.toml` and stops when mise does not
  provide the pinned version.

## Git workflow

- One Linear issue per slice, and one branch and one PR per issue. Name the
  branch with the issue's Linear branch name, and put `Closes <issue ID>` in
  the PR description.
- PR titles follow Conventional Commits (`feat:`, `fix(scope):`, `chore:`).
- One purpose per PR, under 2,000 non-generated lines. Stack at most one PR on
  another that is still open.
- Scoped, logical commits. Push early and often.
- Never rewrite shared history without approval.

## Safety

- A security check fails closed: an error while it decides refuses rather
  than allows, and its test asserts which refusal.
- Never simplify away security, trust-boundary validation, data-loss
  protection, accessibility, observability, or repo-specific safeguards.
- Ask before risky or destructive changes. Run `make all` before finishing.

## Code

- Before adding code, use the first rung that works: remove the need; reuse
  repo code, config, tooling or workflow; use the standard library or native
  features; use an installed dependency; write small local code; only then add
  an abstraction, file, service, config surface or dependency. Prefer fewer
  files and explicit, boring code.
- Work in vertical slices and keep changes reviewable. Simplify touched code.
  Follow local patterns only when sound and intentional. Prefer the correct
  fix over the smallest patch, explain the tradeoff, and rewrite bad structure
  in reviewable slices.
- Comments state what the code does now. History goes in the commit message.
  Never describe anything that does not exist.

## Tests

- Tests are executable acceptance criteria, not a design method. For agreed
  behavior or a confirmed bug, prefer one smallest scenario through a public
  interface or artifact boundary, and extend an existing scenario first. For
  a bug, prove the failure when practical, then keep the regression.
- Otherwise use existing checks or focused manual verification. No
  test-first TDD, tests of private mechanics, speculative cases, equivalent
  branches or coverage targets. Avoid test-only APIs, fixture scaffolding and
  mocks except at system boundaries.
- Use the standard library `testing` package, the keycardai standard.

## Dependencies

- Depend only on public modules. CI has no private-module access, so a
  private import fails the build.
- Ask before adding a dependency. Prefer the standard library, an installed
  dependency or small local code. Otherwise prefer a module Keycard's other
  repositories already use, pinned at its latest release. Evaluate
  maintenance, license, docs, security and transitive dependencies, and say
  in the PR where Keycard already uses it.
