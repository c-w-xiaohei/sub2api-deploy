# Automation API Engine Isolation Plan

**Source:** User-authorized migration: use the official Pulumi SDK where possible, keep the Pulumi Engine in the official CLI process, preserve all existing Task 4 behavior and exact-SHA CI evidence, and do not run local Go builds/tests until CI demonstrates reduced resource use. Current evidence: `cmd/sub2api-deploy/attached_linux.go`, `cmd/sub2api-deploy/pulumi_stage*.go`, `internal/integration/enginegraph/**`, `internal/integration/providerimport/**`, and the pinned Pulumi v3.256.0 Automation API source.
**Goal:** Remove repository-owned direct `pulumi/pkg/v3` Engine/backend/deploy/deploytest use from deployment orchestration and Engine/Import integration tests. Drive the official external Pulumi CLI through Automation API while retaining the attached Host Provider, FD3 approval, private staged stack, semantic checkpoint assertions, and exact-SHA gates.

## Current State And Gaps
- `sub2api-deploy` already invokes an external bundled `pulumi` wrapper, but it directly uses Pulumi workspace/config internals to render and encrypt a private stack file.
- Engine Graph and Provider Import tests directly embed `pkg/v3` backend, Engine, deploy, and deploytest providers; this makes their test binaries heavy.
- `auto.PulumiCommand` allows the Automation API to run a caller-owned command implementation while the official CLI remains the Engine process. A custom command can preserve the existing attached Provider and approval socket lifecycle.
- `pulumi-go-provider v1.4.1` transitively imports `pkg/v3/codegen/schema`. This migration removes embedded Engine/backend/deploy execution, not every transitive `pkg/v3` module dependency.
- Existing CI captures pass/fail evidence but does not establish per-command peak memory or comparable build/test duration evidence for this migration.

## Shared Constraints
- Preserve the single global Pulumi graph/state engine, one custom Host resource per configured server, Host Provider FD3 approval, secret confinement, existing Task 4 test semantics, and exact-SHA candidate promotion.
- The external official Pulumi CLI is the only Engine implementation. Do not create an Engine wrapper, alternative scheduler, or replacement state engine.
- Use public Automation API and SDK interfaces where available. Keep direct subprocess management only in the `auto.PulumiCommand` adapter required to attach the custom Provider and its FD3 approval channel.
- Do not run local Go builds, tests, `go list`, vet, formatter, Pulumi, Docker, package manager, compiler, or linker commands. Static inspection, `git diff --check`, and remote GitHub Actions are permitted.
- CI is the sole behavior and resource verification environment. Lower paired cold-build cgroup memory peak and reviewed build/gate durations are necessary evidence, not automatic local authorization. Local builds and tests remain prohibited until the complete results are reviewed and an explicit user/project decision permits a bounded local command.
- Measure the same commands on a fixed GitHub runner image and Go/Pulumi version. The initial GNU time record is a warm-cache, maximum-single-process RSS observation, not aggregate build memory. A separate paired cold-cache compile/link comparison measures cgroup v2 memory peak (including accounted page cache), fixed concurrency, and duration for baseline and candidate on the same runner. Do not upload raw logs, environment, argv, frame, state, SQL, credentials, or runtime diagnostics.

## Tasks

### Task 1: Automation Workspace And Attached Command Seam
**Owns:** `cmd/sub2api-deploy/**`, narrowly scoped tests under `cmd/sub2api-deploy/**`, and an internal Automation integration helper if required.
**Depends on:** none.
**Consumes / Produces:** Consumes the existing private staged stack and attached Provider/FD3 behavior. Produces a small `auto.PulumiCommand` implementation and private LocalWorkspace construction that starts the official CLI with the existing attached Host Provider and returns Automation-compatible stdout, stderr, exit status, cancellation, and cleanup behavior.
**Preserve:** Source config is untouched; staged workspace/file modes and passphrase secrecy remain enforced; Provider FD3 is never passed to the Pulumi process; unsupported commands never start the Provider.
**Requirements:** Use a private local Program workspace rather than inline Program for the first migration. The staged stack must use the real selected stack name in the private workspace. Retain the existing CLI command interface while routing supported lifecycle operations through Automation API.
**Acceptance:** A focused source/behavior contract proves stack lifecycle calls use the Automation command seam, preserve output/cancellation/approval wiring, and invoke an external `pulumi` executable rather than importing Engine/backend/deploy packages.

### Task 2: CI External Engine Runtime Contract
**Owns:** `.github/workflows/**`, CI source-contract tests, and sanitized resource evidence handling needed to provision the external Engine test boundary.
**Depends on:** Task 1 because the runtime contract consumes the external CLI/Automation command decision.
**Consumes / Produces:** Consumes the pinned release/CLI version and existing Engine Graph job. Produces a pinned official CLI, isolated `PULUMI_HOME`, external test Provider discovery contract, and sanitized baseline resource record available to the external Engine harness.
**Preserve:** All eight required job names, no-skip behavior, existing candidate/release artifact inventory, and raw log/secret/state isolation.
**Requirements:** The CLI must be pinned and made available before test execution. The workflow must not expose raw command arguments, environment, provider logs, fixture inputs, exported state, or secret-bearing diagnostics. The sampler records only command label, elapsed milliseconds, and maximum RSS KiB. Establish a comparable baseline record before removing the embedded harness.
**Acceptance:** Exact-SHA CI emits a schema-validated, sanitized Engine Graph baseline resource record and has a reproducible external CLI/test-Provider runtime contract without changing required job names.

### Task 3: External Engine Test Harness
**Owns:** new bounded test helper packages/executables and shared test-only checkpoint/event/trace adapters under `internal/integration/**`.
**Depends on:** Task 2 because tests consume the pinned CLI, provider discovery, and resource evidence contract.
**Consumes / Produces:** Consumes Automation lifecycle/event/export interfaces and existing fixture topology. Produces external test Provider processes, a sanitized trace IPC mechanism, normalized checkpoint assertions, and a command resource sampler usable by Engine Graph and Provider Import.
**Preserve:** Test provider behavior remains limited to the existing fake semantics; it never becomes production code. Event/checkpoint adapters preserve dependency, ordering, protected/state, import, secret, and retry assertions without importing Engine/backend/deploy types.
**Requirements:** The Engine runs in the official CLI process. Test providers expose the SDK Provider RPC interface, either from a separate helper process or from a test-owned loopback gRPC server attached to that external Engine. Do not add a helper executable merely to separate a fake provider from the test harness. Their traces use fixed safe event grammar and never contain targets, inputs, secrets, state, raw provider messages, or arbitrary error text. The initial sampler records only command label, elapsed milliseconds, and peak RSS KiB; paired build measurements distinguish cgroup memory from RSS.
**Acceptance:** One converted Engine Graph scenario proves real external CLI Engine scheduling and exports equivalent normalized checkpoint/event/trace evidence. The CI artifact contains an allowlisted resource record for the command.

### Task 4: Migrate Engine Graph And Provider Import
**Owns:** `internal/integration/enginegraph/**`, `internal/integration/providerimport/**`, their fixtures, and direct tests.
**Depends on:** Task 3 because these suites consume the stable external Provider/checkpoint/event/trace harness.
**Consumes / Produces:** Consumes external Engine test harness. Produces equivalent Engine Graph ordering/removal/sanitization and Provider Import preview assertions driven through Automation API and the official external CLI.
**Preserve:** Required symbols, all existing semantic cases, exact data admission/removal ordering, retry no-op semantics, secret canaries, state protection, and resource-level import behavior. `Stack.ImportResources` must not replace the existing resource `pulumi.Import(id)` test.
**Requirements:** Remove repository direct imports of `pkg/v3/backend`, `engine`, `resource/deploy`, and `deploytest` from these suites. Do not claim removal of transitive `pkg/v3` through `pulumi-go-provider`.
**Acceptance:** The migrated suites run through external CLI in CI, retain all required symbols and assertion coverage, and source inspection shows no direct Engine/backend/deploy/deploytest imports outside explicitly excluded dependency modules.

### Task 5: CI Resource Evidence And Closure
**Owns:** `.github/workflows/**`, CI contract tests, release metadata/evidence handling, and migration documentation.
**Depends on:** Task 4 because the measured commands must be the final external-Engine gates.
**Consumes / Produces:** Consumes final migrated gate commands and resource records. Produces exact-SHA sanitized duration/RSS evidence, before/after comparison metadata, and an explicit decision on whether local build/test remains prohibited.
**Preserve:** All eight required job names, no-skip behavior, candidate inputs, target release same-byte consumption, safe evidence schema, and existing finalizer cleanup.
**Requirements:** Compare identical cold compile/link targets for fixed baseline and candidate revisions on the same runner. Report lower cgroup memory peak separately from the warm-cache single-process RSS observation. Review duration and all required functional gates before a local safety decision. Do not enable local builds merely because CI is green.
**Acceptance:** An exact-SHA CI run passes all required gates and the complete Engine Graph/Provider Import suites through the external CLI. Source inspection establishes actual public Automation Stack calls; CI inspects test and binary dependency closures for Engine/backend/deploy. Paired measurements report cgroup memory peak and duration of identical cold compilation targets. Full gate durations are reviewed separately; a compile-only result cannot claim lower total CI duration. The final report names observed values, comparison method, residual transitive dependency, and the continuing local-build prohibition unless explicitly changed.

## Verification And Closure
- Static checks: source import inventory, workflow/schema contracts, documentation consistency, and `git diff --check`.
- Remote-only checks: exact-SHA required CI including normal/race Provider Runtime, migrated external Engine Graph/Provider Import, release candidate verification, and resource evidence comparison.
- Closure claim: Engine execution is external to repository test/CLI binaries, while Program/Provider SDK usage remains. This does not claim Nix adoption, NixOS conversion, immutable image migration, production deployment, or removal of `pkg/v3` as a transitive Provider-framework dependency.
