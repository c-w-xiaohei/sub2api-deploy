# Task 4 Engine Graph Report

## Status

CI-ready / exact-SHA evidence pending. `MX-ALLOWLIST-01` Engine Graph coverage
is defined with the required symbols, but has not been executed. No local Engine
test, build, Pulumi, Docker, runtime, Provider, or release command was run.

## Delivered Coverage

- `TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop` uses the real
  local DIY backend, Pulumi Engine, `program.Register`, test Host provider, and
  Cloudflare provider with a dedicated Docker PostgreSQL/Redis data Host
  (`alpha`) and an App/publication Host (`bravo`). Its fixture provides internal
  addresses and explicit App `initialAdminEmail` configuration.
- The success path asserts the exact lifecycle order: Alpha data Host admission,
  Bravo App Host readiness, then Bravo Cloudflare DNS publication. It inspects
  real checkpoint Host targets for Docker data bindings, the remote data links,
  App placement, the Alpha-to-Bravo dependency, and secret-free trace events.
- The same test asserts data Host failure performs no Bravo Host write and no
  publication, and Bravo App Host failure after Alpha succeeds performs no
  publication.
- `TestEngineGraphCrossHostDataRemovalIsReverseStaged` applies the final
  canonical removal config through native Engine targets: publication delete,
  Bravo App detachment while Alpha retains the old source/checkpoint, then an
  untargeted Alpha data-policy contraction. It asserts exact per-stage traces,
  final checkpoint contents, and retry semantic equality with the immediately
  preceding completed stage before asserting retry trace no-op behavior.
- The existing artifact mechanism emits sanitized JSONL lifecycle evidence when
  `ENGINE_GRAPH_TRACE_DIR` is provided by CI. The artifact test asserts one
  deterministic file, strict expected JSONL records, no raw Host targets or
  inputs, and absence of every cross-Host fixture secret canary in both
  in-memory and persisted evidence. No provider test double models or inspects
  nftables internals; it observes only Host resource lifecycle calls.

## CI-Only RED

Before this change, both required symbols were absent:

```text
TestEngineGraphCrossHostDataAdmissionOrderingAndFailureStop
TestEngineGraphCrossHostDataRemovalIsReverseStaged
```

That absence was the expected RED condition for the Task 4 Engine Graph test
contract. Per the task boundary, it was not executed locally. CI must run the
Engine Graph package at the exact candidate SHA to establish execution evidence.

## Verification Limits

- The exact unexecuted boundary is the Engine Graph package: no Engine update,
  local DIY backend, Program registration, provider lifecycle, checkpoint, or
  JSONL artifact assertion was run locally. Exact-SHA CI execution is required
  before reporting passing evidence.
- `gofmt` and a scoped `git diff --check` are the only local verification
  commands permitted and performed for these changes.
- Source inspection verifies that staged removal uses Pulumi's existing
  `engine.UpdateOptions.Targets`; no custom planner, workflow, runtime, Program,
  Provider Runtime, release, workflow allowlist, or other documentation was
  changed.
- Engine scheduling, checkpoint behavior, and JSONL evidence remain CI-only
  until the exact-SHA Engine Graph gate executes.
- No files were staged, committed, pushed, reset, cleaned, or reverted.
