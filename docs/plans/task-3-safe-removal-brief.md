# Task 3 Brief: Safe Removal And User Workflow

## Outcome

Make cross-Host data placement and safe relation removal operable through the existing thin CLI and ordinary Pulumi checkpoints. Do not add a workflow engine, phase state, or automatic migration planner.

## Authority

- `docs/specs/001-pulumi-ssh-controller/context.md` sections 3.1-3.2, 5, 8, and 10.
- Approved `docs/plans/2026-09-04-cross-host-data-completion.md`, SHA-256 `74165d76786b9b823e37e70adf558711557a4bb7815eef33ef8035485665e76a`.
- Accepted Tasks 1-2 implementation and reports.

## Requirements

- Preserve the public CLI as a thin wrapper over standard Pulumi commands. Existing `--target=<URN>` is the staging primitive; do not add a custom plan engine, hidden config rewrite, saved plan, operation database, or automatic multi-command runner.
- Validate target arguments fail closed: only well-formed Pulumi URNs for this stack/project's known Cloudflare DNS records or `sub2api-host:index:Host::host-<valid-server-id>` may be used by the documented removal path. Preserve generic safe Pulumi use if narrowing would break existing external callers; a pure URN helper/test may define the documented contract without overrestricting unrelated commands.
- Pure relation removal uses final canonical config and separate completed Pulumi updates:
  1. target deletion of affected publication resources;
  2. target each affected consumer Host in reverse data/App DAG order so its App is stopped while old data admission remains checkpointed;
  3. run full `up`, allowing data Hosts to revoke nftables sources only after consumer checkpoints.
- Mixed move uses an explicit operator-authored additive intermediate Environment config containing old and new App placements. Full `preview/up` adds data admission, starts destination Apps, and publishes only intended servers. Then write final config and use the pure-removal stages for old consumers.
- Server retirement keeps the server configured through publication detachment and App removal. Remove the server only after its target is drained, then run full update and existing preserve-data retirement approval.
- Every stage is safely rerunnable through Pulumi state and existing Host journals. Do not claim atomic cross-Host transactions.
- Update README/capability statements: cross-Host Docker PostgreSQL/Redis is supported after Tasks 1-2; Neon, MicroSocks, Connector and production migration/cutover remain distinct gaps. Do not call complete original 001 finished while those remain.
- Replace the one-machine example with at least one dedicated data Host and two App Hosts using internal addresses. App config references service names normally; no user firewall/allowlist fields. Include a corresponding plaintext `environments/<env>/config.yaml` shape and encrypted secrets workflow explanation so the release entrypoint is actually usable.
- Document nftables/rootful Docker/internal-network anti-spoofing and root/sudo as production prerequisites, not inferred guarantees.

## Verification

- TDD for CLI parsing/target construction and fail-closed invalid URNs. No production Pulumi execution.
- Local-safe tests only: narrow fixed CLI symbols with `GOMAXPROCS=2 go test -p=1 ./cmd/sub2api-deploy -run '<regex>'` if they do not invoke/build Pulumi; otherwise CI-only source tests. Formatting and scoped diff checks are allowed.
- Do not run build, broad tests, Pulumi, Engine, Provider, release, Docker, nft, SSH, sudo, or production commands.

## Allowed Paths

- `cmd/sub2api-deploy/**`
- `Pulumi.production.example.yaml`
- `README.md`
- `docs/specs/001-pulumi-ssh-controller/tech-spec.md`
- `docs/specs/001-pulumi-ssh-controller/test-spec.md`
- Directly related docs/CLI contract tests
- `docs/plans/task-3-safe-removal-report.md`

## Git

- Shared dirty checkout. Preserve all prior work.
- Do not stage, commit, push, reset, clean, or revert.
