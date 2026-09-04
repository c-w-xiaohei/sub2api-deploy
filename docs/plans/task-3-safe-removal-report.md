# Task 3 Safe Removal Report

## Status

DONE. Task 3's permitted CLI contract, focused test scope, and documentation are complete. No production Pulumi or other prohibited command was run.

## Delivered Contract

- The thin CLI continues to pass ordinary Pulumi `preview`, `up`, `refresh`, `destroy`, and managed import commands through without a second plan engine, saved plan, operation database, hidden config rewrite, or multi-command runner.
- Generic safe `--target` support is preserved. Each target now fails closed unless it is a strict current-stack/current-project Pulumi URN with nonempty three-part type and logical name; malformed, control-character, other-stack, other-project, split, positional, and separator-bypass forms are rejected. The removal documentation names only actual Program Cloudflare DNS and Host resources.
- README and `Pulumi.production.example.yaml` use one data Host plus two API Hosts with internal addresses, ordinary service-name references, and no user allowlist/firewall input.
- The documented operator sequence is data admission -> App readiness -> publication for additions, and publication detach -> reverse consumer checkpoint -> full update for removals. It includes additive moves, retries, and preserve-data server retirement.
- Tech/Test specs record `MX-ALLOWLIST-01` as implementation-present/evidence-pending rather than blocked, while Neon, MicroSocks, Tunnel Connector, successful Import, release, and migration/cutover gaps remain explicit.

## TDD Evidence

Initial RED:

```text
GOMAXPROCS=2 go test -p=1 ./cmd/sub2api-deploy -run '^(TestParsePulumiPlanAcceptsDocumentedRemovalTargets|TestParsePulumiPlanRejectsUnsafeDocumentedRemovalTargets)$'
```

Result: failed as expected because a target for stack `other` was accepted by `parsePulumiPlan`.

Initial GREEN:

```text
GOMAXPROCS=2 go test -p=1 ./cmd/sub2api-deploy -run '^(TestParsePulumiPlanAcceptsDocumentedRemovalTargets|TestParsePulumiPlanRejectsUnsafeDocumentedRemovalTargets)$'
```

Result: passed after the pure documented-target validator was added.

Review-fix RED/GREEN:

```text
GOMAXPROCS=2 go test -count=1 -p=1 ./cmd/sub2api-deploy -run '^TestParsePulumiPlan'
```

RED: `TestParsePulumiPlanAcceptsDocumentedRemovalTargets` failed because the
logical-name-specific validator rejected a valid Host server key containing
uppercase, underscore, and dot characters, a long generated DNS name, and a
valid Upstash target. GREEN: the same command passed after validation became
strict-URN-only and repeated valid targets retained their original order.
`TestParsePulumiPlanRejectsUnsafeDocumentedRemovalTargets` covers malformed,
control-character, other-stack/project, empty type/name, split-target, and
separator/positional bypass rejection.

## CI-Only Gaps

- Cross-Host Docker data is implementation-present/CI-evidence-pending. `MX-ALLOWLIST-01` and Task 4 still need exact-SHA evidence for data admission, App readiness, publication, failure stop, inverse removal order, and privileged runtime behavior.
- Privileged nftables/Docker network behavior remains Task 4 CI-only; the documentation calls root/sudo, rootful Docker, nftables, internal routing, and anti-spoofing production prerequisites rather than guarantees.
- No local production Pulumi, build, broad test, Engine, Provider, release, Docker, nft, SSH, sudo, or production operation was executed.

## Scope Discipline

Only Task 3 allowed CLI, documentation, specs, example configuration, tests, and this report were changed. Existing dirty Tasks 1-2 work was preserved. No files were staged, committed, pushed, reset, cleaned, or reverted.
