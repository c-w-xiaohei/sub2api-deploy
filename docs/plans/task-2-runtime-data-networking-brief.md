# Task 2 Brief: Runtime Data Networking And nftables

## Outcome

Implement Task 2 from the approved cross-Host plan. Consume Task 1's canonical data bindings, clients, client passwords, and TLS-aware links to make remote PostgreSQL/Redis connectivity real and fail-closed.

## Authority

- `docs/specs/001-pulumi-ssh-controller/context.md` sections 3.3-3.10, 6-8, and 10.
- `docs/plans/2026-09-04-cross-host-data-completion.md`, SHA-256 `74165d76786b9b823e37e70adf558711557a4bb7815eef33ef8035485665e76a`.
- Accepted Task 1 contract in `internal/hostcontract/contract.go`.
- User decision: nftables owns source-IP control.

## Runtime Contract

- Same-Host data stays on the owned Docker network and receives a stable network alias matching the service ID; it has no Host port publication.
- A bound data service publishes each exact configured internal binding as `<address>:<port>:<port>/tcp`; never wildcard, public fallback, host networking, or an SSH-resolved address.
- App env emits exact upstream names: `DATABASE_HOST`, `DATABASE_PORT`, `DATABASE_USER`, `DATABASE_PASSWORD`, `DATABASE_DBNAME`, `DATABASE_SSLMODE`, `REDIS_HOST`, `REDIS_PORT`, `REDIS_USERNAME`, `REDIS_PASSWORD`, `REDIS_DB`, `REDIS_ENABLE_TLS`. Deployment-owned keys cannot be overridden through runtime settings/secrets.
- nft execution is a narrow method on the existing runner, with only fixed `nft -j list table inet <owned-name>`, `nft -c -f -`, and `nft -f -` operations. Generated transactions go through stdin; no shell or configurable executable/argv.
- Own one deterministic `inet` table per Host, with an exact ownership comment. Its `prerouting` filter base chain uses priority `-110`. For each internal destination/TCP port, exact canonical source accepts immediately precede a drop for all other sources. Do not flush or mutate other tables.
- Inspect foreign/malformed/missing expected policy without mutation: foreign ownership is conflict/recovery; missing or changed owned policy is drift/not-ready. Reconcile validates current ownership, syntax-checks, atomically replaces only the owned table, then re-reads and verifies exact state.
- Reuse the existing Host lock, operation journal, inventory, and pending replay. Do not add another durable state file, phase engine, manager, or public interface.
- PostgreSQL: listen on container interfaces; password encryption SCRAM; generated HBA has no trust entry. Create/rotate configured roles and databases through SQL stdin, with strict identifiers. Grant enough rights for first-party startup schema migration. A removed client becomes `NOLOGIN`; never drop a role, database, schema, table, volume, or data path.
- Redis: generated config and ACL authenticate admin and declared clients. Removed clients disappear from the desired ACL. Never delete Redis data. Passwords must not be placed in argv, nft rules, logs, observations, inventory, or journal.
- Allowlist-only changes must not move or recreate the persistent data path. Avoid restarting a data container when only sources change if the current Docker publication is unchanged.
- Retirement removes Apps/routes, proxy, published data containers, then the owned nftables table/network shell, preserving all data paths and metadata. Never delete a same-named foreign nftables table.

## Required Tests

- Add behavior tests before production changes for exact App env and reserved keys.
- Add Docker argv tests for stable alias, local-only no-publish, exact IPv4 and bracketed IPv6 publication, and no wildcard/public fallback.
- Add generated nft transaction/state tests for table ownership, hook/priority, source/destination/port rules, deterministic sorting, no secrets, foreign/malformed conflict, missing/drift observation, atomic replacement, and delete ordering.
- Add client tests for PostgreSQL create/rotate/NOLOGIN without DROP, Redis ACL add/rotate/removal, duplicate-safe behavior, SQL/identifier containment, and no secret argv/output/state.
- Add pending response-loss tests for nft apply and delete using exact observed desired/absent state, plus unknown-state fail-closed behavior.
- Preserve and rerun existing Host Runtime behavior tests where locally safe.

## Verification Boundary

- Allowed local commands: `gofmt`, scoped `git diff --check`, and selected symbols only using `GOMAXPROCS=2 go test -p=1 ./internal/hostruntime -run '<fixed symbol regex>'`. Keep selections narrow; do not run the whole package if it materially links or exhausts memory.
- Do not run any build, broad test, race, Program/Provider/Engine test, release assembly, Docker integration, real nft mutation, sudo, SSH, or production command locally.
- Privileged real nft network-namespace verification belongs to Task 4 CI.

## Allowed Paths

- `internal/hostruntime/**`
- Directly required tests under `cmd/sub2api-host/**` or `internal/hostprovider/**` only if the runtime contract cannot otherwise be covered; do not change Provider lifecycle behavior.
- `docs/plans/task-2-runtime-data-networking-report.md`

## Git And Scope

- Shared dirty checkout; preserve all prior changes.
- Do not stage, commit, push, reset, clean, or revert.
- Do not edit Task 1 contract/schema/Program paths. If the accepted contract is insufficient, return `NEEDS_CONTEXT` with the exact incompatibility instead of changing it.
