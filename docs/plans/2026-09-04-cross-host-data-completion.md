# Cross-Host Data Completion Implementation Plan

**Source:** `docs/specs/001-pulumi-ssh-controller/context.md` sections 3 and 5-10; user decisions that cross-Host Docker data is central and nftables owns source-IP access control.
**Goal:** A PostgreSQL/Redis service on one configured server safely serves Sub2API Apps on other configured servers over internal addresses, with derived network admission and Pulumi-controlled ordering.

## Current State And Gaps

- Environment validation recognizes cross-server relations, but Program rejects them.
- Host targets lack network bindings, allowed sources, and data clients.
- Data containers are Host-private, while App env lacks the complete upstream connection contract.
- Program has App/publication ordering but no data-Host-to-App-Host DAG.
- Existing Host ownership, inventory, journal, approval, and data-preservation mechanisms must remain the only Host-side lifecycle state.

## Shared Constraints

- Preserve one custom `Host` per server and Pulumi as the only global graph/state engine.
- Derive policy from service owner, App placement, and configured internal addresses. Do not add user-maintained allowlists or expose nft/container identities.
- Data binds only internal addresses; nftables admits exact source IPs. Same-Host clients use the private Docker network.
- A Host may own data and run Apps. Reject only invalid topology, address/socket conflicts, and dependency cycles.
- Preserve persistent data, unowned objects, secret isolation, exact data-link approval, and response-loss recovery.
- No barrier, scheduler, ledger, saved-plan interpreter, daemon, or additional custom Pulumi resource.
- No local build, release assembly, broad Go test, Pulumi/Provider-linked test, or memory-heavy command. Local checks are formatting and focused low-concurrency non-Pulumi package/symbol tests. Program, Provider, Engine, race, binary, nft network-namespace, release, and candidate checks are CI-only.
- The checkout is intentionally dirty. Never reset, clean, or revert existing changes. Implementers do not stage, commit, or push.
- Each Task uses a fresh scoped implementation agent, independent reviewer, and fix/re-review loop. Writers are sequential because core paths overlap. A fresh final requirements/security reviewer inspects the integrated result.

## Requirements Disposition

- Executable central scope: config derivation, Host contract/schema, Program DAG, App connection env, data-client provisioning, nftables lifecycle, safe relation removal, integration/CI/release evidence, examples, and accurate docs.
- Separately authorized: production migration/cutover. Compatibility must not be regressed, but this plan does not execute migration.
- External production prerequisites: approved immutable PostgreSQL/Redis image digests, actual machine-identity suitability, root/sudo policy, internal-network anti-spoofing, and production credentials.
- Separate owner-contract gaps: Neon, MicroSocks, and tunnel connector. A final audit reports these; this plan cannot invent their contracts or claim complete original-001 implementation while they remain open.

## Tasks

### Task 1: Cross-Host Contract And Program Graph

**Owns:** `internal/environment/**`, `internal/hostcontract/**`, `internal/hostprovider/provider.go`, relevant Provider schema tests, and `internal/program/**`.
**Depends on:** none.
**Produces:** canonical Host-local data bindings and clients, scoped client secrets, remote App data identities, and an acyclic deterministic Host DAG.
**Requirements:** Prefer common internal IPv4, otherwise IPv6; reject no-common-family relations before registration. A local service target carries sorted bindings `{address, allowedSources[]}` and sorted clients `{appId, username, database}`. Project client passwords to the data owner keyed by App ID. Reject incompatible duplicate usernames within one service. Reject socket collisions by owner/bind/TCP-port. Docker data identity is `docker:<owner>:<service>`; endpoint is a stable local network alias for same-Host consumers or selected owner internal IP remotely. Preserve Pulumi unknown/secret semantics. Register data predecessors before App consumers and publication; reject cycles before any resource.
**Acceptance:** Tests prove cross-Host PostgreSQL/Redis acceptance, exact target/secret scoping, same/mixed Host behavior, deterministic projection/DAG, cycles and invalid families rejected, and schema/contract validity.

### Task 2: Runtime Data Networking And nftables

**Owns:** `internal/hostruntime/**` and directly required Host process/provider lifecycle tests.
**Depends on:** Task 1 contract.
**Produces:** address-bound data containers, provisioned clients, complete App env, owned nftables policy, readiness/drift evidence, and journal-safe recovery.
**Requirements:** Add only a fixed-purpose nft execution seam with fixed executable/argv and stdin rules. Own one deterministic `inet` table per Host with ownership comment and a `prerouting` filter chain at priority `-110`, before Docker DNAT. Match original destination internal IP/TCP port; accept exact source sets immediately before dropping all others. Inspect exact ownership/shape before mutation and atomically replace only the owned table. Never flush/modify foreign tables. PostgreSQL uses SCRAM and authenticated generated roles/databases with no trust rule. Create/rotate clients and grant first-party schema migration ability; removed clients become `NOLOGIN`, but roles/databases/data are never dropped. Redis uses generated ACL users; removed users disappear from ACL without deleting data. Reject conflicting principals before remote writes. App env uses upstream `DATABASE_*` and `REDIS_*`. Inspect is read-only; retirement removes containers before owned firewall shell and preserves data.
**Acceptance:** Tests prove exact IPv4/IPv6 binds, no wildcard/public bind, exact source/socket nft rules, client add/rotate/non-destructive removal, Redis ACL removal, conflict rejection, complete App env, allowlist-only updates without data loss, drift/foreign-table failure, response-loss recovery, and preserve-data retirement. CI includes a privileged network-namespace functional test proving authorized traffic passes and unauthorized traffic is dropped on the pre-DNAT path.

### Task 3: Safe Removal And User Workflow

**Owns:** `cmd/sub2api-deploy/**`, `Pulumi.production.example.yaml`, `README.md`, 001 tech/test specs, and directly related tests.
**Depends on:** Tasks 1-2.
**Produces:** an explicit standard-Pulumi staged removal path and accurate usage documentation.
**Requirements:** Pure removal writes final config, targets publication deletion, then affected consumer Host App detachment in reverse DAG order, then runs full update to tighten data Host policy. Mixed moves use an explicit operator-authored additive intermediate config containing old and new placements, fully apply it, then use pure-removal stages for final config. Pulumi checkpoints are the only resume state. Server retirement keeps the server configured until its App target is detached. Define exact commands/URN inputs and safe rerun behavior; do not claim atomic cross-Host transactions.
**Acceptance:** CLI tests prove exact target/stage construction and fail-closed ordering without production execution. Docs cover dedicated data Host, remote Apps, pure removal, mixed move, interruption/retry, and server retirement using only natural Environment concepts.

### Task 4: Integration, CI, And Release Evidence

**Owns:** `internal/integration/enginegraph/**`, `internal/integration/providerruntime/**`, `.github/workflows/**`, release verification/tests, and required inventory changes.
**Depends on:** Tasks 1-3 and the final read-only requirements audit.
**Produces:** `MX-ALLOWLIST-01` Engine/runtime evidence and a consumable exact-SHA candidate.
**Requirements:** Prove data allow/readiness before App readiness before publication; data failure prevents App/publication; App failure prevents publication; staged removal is reverse ordered; unrelated nftables state and secrets remain untouched. Required no-skip jobs are `Verify`, `Engine Graph`, `Provider SSH`, `Provider Runtime`, `Provider Import`, `Host Controller (amd64)`, `Host Controller (arm64)`, and `Target Release`. `MX-ALLOWLIST-01` symbols belong in Engine Graph and Provider Runtime.
**Acceptance:** Before commit/push authorization, source and workflow are CI-ready only. After explicit authorization, every named job succeeds for the exact SHA and target candidate consumption covers cross-Host Provider-to-Runtime reconciliation.

## Verification And Closure

- Run a fresh read-only original-001 requirements/security audit after Tasks 1-3. Code findings return to the owning Task before Task 4. Classify all requirements as implemented central scope, separately authorized migration, external production prerequisite, or named owner-contract gap.
- Task 4 and exact-SHA CI run last. Any later code correction requires a new SHA and complete rerun.
- Commit/push is an explicit user authorization checkpoint. No production deployment, SSH mutation, cloud mutation, data migration, or cutover occurs without separate authorization.
