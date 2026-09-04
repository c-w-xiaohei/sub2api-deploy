# Task 1 Brief: Cross-Host Contract And Program Graph

## Outcome

Implement Task 1 from `docs/plans/2026-09-04-cross-host-data-completion.md` (approved SHA-256 `74165d76786b9b823e37e70adf558711557a4bb7815eef33ef8035485665e76a`). A Docker PostgreSQL/Redis owner Host must project safe canonical internal-network policy and remote App data links; Host registration must form a deterministic acyclic data-to-App DAG.

## Authority

- `docs/specs/001-pulumi-ssh-controller/context.md` sections 3.1-3.4, 3.7-3.10, 6-8, and 10.
- User decisions: cross-Host local data is central; nftables owns source-IP admission; do not run local Pulumi-heavy builds/tests.
- The approved plan above.

## Required Shape

- Environment YAML remains unchanged: no `accessFrom` or firewall fields.
- Derive each remote relation from the Docker service owner and `apps.<id>.servers`.
- Select common internal IPv4 when both endpoints have it; otherwise common internal IPv6; reject no common family before any Pulumi registration. Canonicalize IP literals.
- A `LocalDataServiceTarget` carries sorted bindings, each with owner bind address and sorted exact allowed source addresses, and sorted clients carrying App ID, declared username, and PostgreSQL database or Redis database string.
- `LocalDataServiceSecrets` carries admin password and data-client passwords keyed by App ID. Only the owning data Host receives these client passwords; it receives no unrelated App JWT/TOTP/admin/runtime secrets.
- Reject an incompatible duplicate username within one service before registration. Identical principal tuples may be de-duplicated only if their password is also identical.
- Reject collisions by owner Host, bind address, TCP port.
- Docker link provider identity is `docker:<owner-server>:<service-id>`. Same-Host endpoint is a stable service-ID network alias. Remote endpoint is the selected data Host internal IP. Preserve the configured port/database and correct TLS flags.
- A Host may own data and run Apps.
- Build a complete dependency graph: every remote Docker data owner precedes each consuming App Host; each App's placements retain deterministic rolling order consistent with the DAG; public resources remain after selected public App Hosts. Reject cycles before registering any Pulumi resource.
- Preserve Pulumi unknown and secret metadata and one Host resource per configured server.

## TDD And Verification

- Add focused behavior tests first. Record expected RED by source inspection or local-safe tests only.
- Local allowed: `gofmt`, `GOMAXPROCS=2 go test -p=1 ./internal/environment`, `GOMAXPROCS=2 go test -p=1 ./internal/hostcontract` and narrower symbols.
- Do not locally run `go test` for `internal/program` or `internal/hostprovider`, any build, race, release command, Engine harness, or broad package pattern. Add those tests but mark verification CI-only in the report.
- Do not weaken or delete existing assertions merely to make the new behavior pass. Replace the old cross-Host rejection assertions with positive behavior and invalid-topology assertions.

## Allowed Paths

- `internal/environment/environment.go`
- `internal/environment/environment_test.go`
- `internal/hostcontract/contract.go`
- `internal/hostcontract/contract_test.go`
- `internal/hostprovider/provider.go`
- `internal/hostprovider/provider_test.go`
- `internal/program/program.go`
- `internal/program/program_test.go`
- `docs/plans/task-1-cross-host-contract-report.md`

## Forbidden Scope

- Runtime/nftables implementation, CLI workflow, docs/examples, CI/release, migration, Neon, MicroSocks, connector.
- Staging, committing, pushing, resetting, cleaning, or reverting any existing worktree change.

## Report

Write `docs/plans/task-1-cross-host-contract-report.md` with status, behavior, changed paths/stat, tests and RED/GREEN evidence, CI-only checks, self-review, and concerns. Return only status, changed paths, one-line test summary, report path, and concerns.
