# AGENTS.md

Reference deployment for running an agent in a Kubernetes sandbox whose
outbound HTTPS the Keycard egress proxy mediates.

## Commands

- `make all` runs every CI gate. Run it before finishing.
- `make fmt` formats the code.
- Go tools are `tool` directives in `go.mod`, run as `go tool <name>`. Never
  run a gate tool from `$PATH`.
- Every other tool except Docker is pinned in `mise.toml`, with its checksums
  in `mise.lock`. Run `mise install`. Docker comes from OrbStack or Docker
  Desktop. Add a new tool to `mise.toml`, never as a requirement on `$PATH`.
- Go code runs those tools through `internal/pinned`, and shell scripts
  through `scripts/pinned`. Both stop when mise does not provide the pinned
  version.

## Git

- Work in vertical slices, each on its own branch and PR. When an issue needs
  more than one slice, make each slice a Linear sub-issue.
- Open a slice's PR only once merging it could close its sub-issue.
- Make scoped, logical commits, and push early and often.
- Never rewrite shared history without approval.

## Safety

- A security check fails closed: an error while it decides refuses rather
  than allows, and its test asserts which refusal.
- Never simplify away security, trust-boundary validation, data-loss
  protection, accessibility, observability, or repo-specific safeguards.
- Ask before risky or destructive changes.

## Code

- All code is debt, tests included. Keep code only while its value justifies
  its maintenance cost, and delete what no longer earns its place.
- Before adding code, use the first rung that works: remove the need; reuse
  repo code, config, tooling or workflow; use the standard library or native
  features; use an installed dependency; write small local code; only then add
  an abstraction, file, service, config surface or dependency. Prefer fewer
  files and explicit, boring code.
- Keep changes reviewable. Simplify touched code.
  Follow local patterns only when sound and intentional. Prefer the correct
  fix over the smallest patch, explain the tradeoff, and rewrite bad structure
  in reviewable slices.

## Tests

- Tests are executable acceptance criteria, not a design method. For agreed
  behavior or a confirmed bug, prefer one smallest scenario through a public
  interface or artifact boundary, and extend an existing scenario first. For
  a bug, prove the failure when practical, and keep the regression while it
  can still happen. Stop once the test tells broken from fixed.
- Otherwise use existing checks or focused manual verification. Don't write
  tests first, test private mechanics, add speculative cases or equivalent
  branches, or chase coverage targets. Avoid test-only APIs, fixture
  scaffolding and mocks except at system boundaries. Add BDD tooling only when
  asked.
- When changing behavior or touching tests, reassess the affected tests.
  Delete or consolidate tests of retired requirements, regressions that can
  no longer happen, private details, or behavior covered elsewhere. Keep
  protection for current contracts, possible regressions and critical
  invariants. Check history when intent is unclear; age, inconvenience or
  failure alone does not justify a deletion. Explain every removal.

## Dependencies

- Depend only on public modules. CI has no private-module access, so a
  private import fails the build.
- Before adding a dependency, compare credible alternatives, including none.
  Assess fit, license, maintenance and security from current sources,
  including vulnerabilities, provenance and transitive dependencies. Weigh the
  code it replaces against the obligations it adds. Recommend one with sources
  and risks, then ask for approval.
