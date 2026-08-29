# Test Spec: Pulumi SSH Controller

状态：`Draft for implementation review`
Owner：Feature Dev Test Spec producer
范围：Environment Pulumi Stack、唯一 `Host` resource、系统 OpenSSH、按需 `sub2api-host`、CLI/release 主链。迁移在独立附录，当前不实施；SingBox 不属于 001。

## 0. Product Source

| 项目 | 事实 |
| --- | --- |
| Product source of truth | [context.md](./context.md)，特别是目标、设计原则、Host lifecycle、云/Host 关系与测试原则。 |
| 规范性需求 | [tech-spec.md](./tech-spec.md) §10 Requirement Index 的 `TR-*`、`AC-*`。它优先于当前代码切片。 |
| 实施计划 | [docs/plans/2026-08-10-pulumi-ssh-controller.md](../../plans/2026-08-10-pulumi-ssh-controller.md)，用于任务/owner/harness 排序，不取代 source。 |
| 当前 slice | 001 补齐 Program、Provider、OpenSSH、Host process/runtime、public CLI、release 及其分层验证；迁移另行决策。 |
| 明确排除 | SingBox、真实云/VPS/公网/业务数据烟测、第二套状态/计划引擎。 |

当前源码和测试仅是 `candidate-unverified` / `implementation-present-evidence-pending`：没有精确远端 SHA 的 GHA run/artifact 时，任何源码测试都不是 `covered` 或 `passed`。`product-gap` 表示产品能力尚未提供；`harness-gap` 表示能力/局部机制存在但所需验证边界未闭合；`blocked-decision` 表示不能由当前 source 或字段自动推导的 owner 决策。

## 1. Test Architecture / Test Surface

| Module | Interface under test | Production seam | Candidate test adapter / path | Required integration boundary | Owner |
| --- | --- | --- | --- | --- | --- |
| Environment Program | config/secrets -> official resources + one Host/server | Pulumi language host | `internal/program/program_test.go` `pulumi.WithMocks` | planned real Engine/local backend and test-only publication Provider | Program/Integration |
| Host Provider | `Check/Diff/Create/Read/Update/Delete/Import` RPC | provider process -> system SSH | `internal/hostprovider/*_test.go` recording transport | planned Provider process -> scripted SSH subprocess | Provider/Integration |
| OpenSSH | fixed argv/stdin -> one response frame | system `ssh` | `internal/openssh/*_test.go` recording process + loopback | Provider uses it through scripted SSH subprocess; no VPS | Transport |
| Host process | stdin/stdout one-request process | `cmd/sub2api-host` | `cmd/sub2api-host/main_test.go` | process -> temp runtime/test-only command runner | Host-process/Runtime |
| Host runtime | inspect/reconcile/retire -> observation/result | filesystem + Docker/route/probe commands | `internal/hostruntime/*_test.go` tempFS + recordingRunner | Provider lifecycle connects through test-only seam | Runtime |
| Public CLI | user command/SOPS/approval -> Pulumi invocation | released `sub2api-deploy` | `cmd/sub2api-deploy/*_test.go` fake executable + PTY | public command must invoke attached Provider/Pulumi path | CLI |
| Release | release inputs -> verifiable bundle | GHA release assembly | `test/release-bundle.test.ts`; `cmd/sub2api-environment/main_test.go` fixture | target exact-SHA assembled artifact consumed separately | Release |

`WithMocks` proves registration/projection only, not Engine/backend transitions. Recording transport proves Provider calls only, not host-key semantics. Loopback proves OpenSSH behavior only. `recordingRunner` never proves Docker. No test seam may add a production public API, permanent two-Host root, or controller state.

### 1.1 Current Harness / Adapter Inventory

| Path | Candidate evidence, limited to actual assertion | Current status | Missing boundary / action |
| --- | --- | --- | --- |
| `internal/program/program_test.go` | two Host registrations, Cloudflare/Upstash inputs, protect, secret scope, deterministic map projection, maintenance suppression | candidate-unverified / implementation-present-evidence-pending | planned Engine/local-backend, official Neon, allowlist/failure-stop/publication Provider coverage |
| `internal/hostprovider/provider_test.go`, `lifecycle_test.go` | property class purity, lifecycle recording transport, Create checkpointing, Read ID preservation, approval/Delete local behavior | candidate-unverified / implementation-present-evidence-pending | Import success and Provider-process-to-runtime connection |
| `internal/openssh/openssh_test.go`, `process_linux_test.go`, `loopback_linux_test.go` | fixed argv/framed stdin, alias rejection, malformed response, host-key/terminator, cancellation | candidate-unverified / implementation-present-evidence-pending | scripted SSH subprocess connection from Provider |
| `cmd/sub2api-host/main_test.go` | one-frame process behavior, process exit, bootstrap/reconcile result, FD3 attestation | candidate-unverified / implementation-present-evidence-pending | CI-only invocation with temp runtime, no production root changes |
| `internal/hostruntime/runtime_test.go`, `reconcile_test.go` | tempFS/journal/lock, recovery, blue/green rollback, preserve-data/retire, recordingRunner containment | candidate-unverified / implementation-present-evidence-pending | Provider lifecycle connection; no two-Host production runtime requirement |
| `cmd/sub2api-deploy/*_test.go` | staged stack helpers, fake executable process handling, attached Provider helpers, PTY exact approval | candidate-unverified / implementation-present-evidence-pending | public CLI command wiring is a product gap |
| `cmd/sub2api-environment/main_test.go` | executable-relative manifest fixture and caller mock diagnostics | candidate-unverified / implementation-present-evidence-pending | target release artifact consumption |
| `test/release-bundle.test.ts` | fixture assembly, manifest/ELF/tamper/archive/workflow structural checks | candidate-unverified / implementation-present-evidence-pending | target supported release bundle is a product gap |

## 2. Dependency Integration Environment

| Layer | Real behavior | Stubbed external behavior | Fixture / cleanup / isolation owner | Scope rule |
| --- | --- | --- | --- | --- |
| Engine graph | real Pulumi Engine/local backend and Program | test-only Host Provider plugin returns ready/fail by Host logical identity and records calls; official cloud/publication Providers record calls | Integration: unique backend dir/stack, removes state/artifacts | proves graph, partial failure stop and publication gating; never contacts cloud |
| Provider transport | real provider process and Provider lifecycle | scripted SSH executable returns protocol outcomes | Integration: unique PATH/socket/trace, reap subprocesses | proves Provider RPC/process -> fixed SSH request; no remote host needed |
| Provider/runtime | real lifecycle code + test helper subprocess executing the same `serve`/Runtime implementation with injected temp roots | recording Docker/route/probe runner | Runtime: planned `internal/hostruntime/testonly` CI-only helper starts subprocesses that call internal Runtime with A/B case temp roots; owns journal/lock/sentinels and reaps children | helper is test-only and is not labeled as the released `sub2api-host` binary; it does not change production public API or require a production fixed root/two instances |
| Host process | real `cmd/sub2api-host` stdio process | scripted command runner/attestation peer | Host-process: own pipe/FD3/temp root; close/reap | proves process framing/exit separately from Provider |
| CLI | real CLI process and PTY | fake `sops`, `pulumi`, Provider executable | CLI: unique PATH/PTY/env; restore/reap | helpers alone do not prove public command exposure |
| Release | target GHA assembled artifact | no cloud/VPS | Release: CI staging dir/artifact retention | independently proves artifact discovery/verification; does not require Engine chain |

禁止真实云、VPS、公网、生产 SOPS key、共享数据、本地 Docker/SSH/Pulumi/release 执行。每个失败场景必须断言相应 trace、journal、data/unowned sentinel 或 Engine state 无额外副作用。

### 2.1 Minimum Layered Offline Closure

不存在“唯一单体 full-chain”门槛。P0 使用下列最小分层闭环，分别提供可执行证据：

```text
L1 Engine graph: local backend -> Program -> test-only Host Provider plugin (logical Host identity -> ready/fail trace) -> test-only publication Provider
L2 Provider transport: provider process -> scripted SSH subprocess -> framed response
L3 Host lifecycle: Provider lifecycle seam -> planned `internal/hostruntime/testonly` CI-only helper subprocess -> same serve/Runtime implementation -> A/B tempFS + recordingRunner
L4 Release: exact-SHA assembled artifact -> isolated manifest/artifact consumer
```

核心 offline chain 只串 L2/L3 中可设计执行的部分；L1 和 L4 独立。若 L3 需要 alias router 或 temporary root，仅可由 CI-only helper/subprocess 将 alias 映射到每 case 独占 tempFS；不得要求生产 Host 固定 root 支持两实例，也不得增加 production public API。两机 Program graph 仍是 candidate，不等于两机运行证明。

## 3. Black-box Design

| Technique | Selected cases | Required MX |
| --- | --- | --- |
| EP | valid/invalid config reference; valid/hostile alias; valid/malformed frame; matching/nonmatching ownership | MX-PROG-01, MX-SSH-01, MX-READ-01 |
| BVA | configured server count `0/1/2` -> planned `TestEngineConfiguredServerCountZeroOneTwo`; App placement count `0` -> existing `TestRegisterMaintenancePlacementKeepsHostsAndSuppressesPublication`, `1/2` -> planned `TestEngineAppPlacementZeroOneTwo`; frame `max-1/max/max+1` -> planned `TestProtocolFrameBoundaries`; dangerous data-link change count `0/1/2` -> existing exact/multiple rejection symbols plus planned `TestDangerousDataLinkChangeCardinalityZeroOneTwo` | MX-PROG-01, MX-ORDER-01, MX-SSH-01, MX-DATA-01 |
| Decision | dangerous changes: none/single/multiple; approval: absent/exact/wrong; operation: same/different revision; result: received/lost; readiness: ready/fail | MX-DATA-01, MX-REC-01, MX-ORDER-01 |
| State | absent -> intent -> terminal/retry; old route -> candidate -> switched/rollback; Import reject/success | MX-REC-01, MX-RUNTIME-01, MX-IMPORT-01/02 |
| Pairwise | App placement `0/1/2` x revision `same/different` x readiness `ready/fail` | MX-ORDER-01, MX-REC-01 |
| Mandatory triples | `(allow-source, App ready, publication)`; `(same operation, response loss, retry)`; `(public off, servers: [], first ready)` | MX-ALLOWLIST-01, MX-REC-01, MX-ORDER-01 |
| Negative | unknown field, forged identity, corrupt state, missing artifact, unsupported feature | MX-PROVIDER-01, MX-IMPORT-01, MX-RELEASE-01, MX-MICROSOCKS-01 |

Pairwise selects cases; it is not a generator, lockfile, selector system, or internal phase Cartesian matrix.

下表中的六个 placement tuples 覆盖 `placement x revision x readiness` 的全部值对。Data-link admission 不采用 pairwise，因为 approval 只在单个危险 identity change 时合法；它改用穷举合法 Decision/State cases，避免制造“不变 identity 却携带 approval”等不可能状态。两类选例都不引入 generator 或 selector framework。

| Selected tuple | Bound symbol | Expected observation |
| --- | --- | --- |
| `(placement=0, revision=same, ready)` | `TestRegisterMaintenancePlacementKeepsHostsAndSuppressesPublication`; planned: `TestEngineAppPlacementZeroOneTwo` | configured Host resources remain; App publication is absent. |
| `(placement=0, revision=different, fail)` | planned: `TestEngineMaintenanceAndRollingFailureStop` | no App runtime or publication starts while placement is empty. |
| `(placement=1, revision=same, fail)` | planned: `TestEngineAppPlacementZeroOneTwo` | one target Host is attempted; publication remains blocked on failure. |
| `(placement=1, revision=different, ready)` | planned: `TestEngineAppPlacementZeroOneTwo` | one target Host becomes ready before publication. |
| `(placement=2, revision=same, ready)` | `TestRegisterFoundationGraph`; planned: `TestEngineMaintenanceAndRollingFailureStop` | two Host graph edges remain stable and publication follows both readiness results. |
| `(placement=2, revision=different, fail)` | planned: `TestEngineMaintenanceAndRollingFailureStop` | predecessor failure prevents the later Host and publication. |
| `(frame=max-1/max/max+1)` | planned: `TestProtocolFrameBoundaries` | first two accepted only if complete/compatible; oversized frame fails before retry. |
| `(dangerousChanges=0, approval=absent)` | planned: `TestDangerousDataLinkChangeCardinalityZeroOneTwo` | no identity change creates no approval request; unrelated ordinary target changes may still reconcile normally. |
| `(dangerousChanges=1, approval=absent)` | planned: `TestDangerousDataLinkChangeCardinalityZeroOneTwo` | missing approval prevents the write and creates no pending operation. |
| `(dangerousChanges=1, approval=wrong)` | `TestLifecycleUpdateApprovalFailuresAndMultipleChangesDoNotWrite` | mismatched subject or revision is rejected before write. |
| `(dangerousChanges=1, approval=exact, result=received)` | `TestLifecycleUpdateRequestsOnlyExactSingleDataLinkApproval` | one matching subject permits the intended write. |
| `(dangerousChanges=1, approval=exact, result=lost, retryRevision=same)` | planned: `TestApprovalRetryRevisionBinding` | retry resumes the admitted operation without consuming another approval. |
| `(dangerousChanges=1, approval=exact, result=lost, retryRevision=different)` | planned: `TestApprovalRetryRevisionBinding` | a different revision cannot reuse admission and requires a new exact subject. |
| `(dangerousChanges=2, approval=not-consulted)` | `TestLifecycleUpdateApprovalFailuresAndMultipleChangesDoNotWrite`; planned: `TestDangerousDataLinkChangeCardinalityZeroOneTwo` | multiple simultaneous dangerous changes fail closed before consulting approval or writing. |

## 4. Executable Matrices

All `GHA gate` values are **planned**. A `planned GHA@REMOTE_SHA:<gate>` is not a current workflow claim. Test symbols without `planned:` exist but remain candidate-unverified; `planned:` symbols do not exist. Only a matching exact 40-hex remote SHA, run URL, named result and artifact can change a row to `passed`.

### 4.1 Program / Engine / Product Gaps

| MX ID | source | technique | prestate | stimulus | observable | no-side-effect | real/mock | owner | test symbol | GHA gate | status |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| MX-PROG-01 (`TS-P0-PROG-01`) | TR-PROG-01/02 | EP+BVA | 0/1/2 server fixtures | Program register | existing two-server fixture asserts CF/Upstash and two Host registrations; planned Engine boundary covers exact configured server counts | invalid config pre-registration | `WithMocks`; planned Engine fixture | Program | `TestRegisterFoundationGraph`, `TestRegisterRejectsBeforeRegistration`; planned: `TestEngineConfiguredServerCountZeroOneTwo` | planned `GHA@REMOTE_SHA:program-graph` | candidate-unverified / implementation-present-evidence-pending + harness-gap for 0/1 counts |
| MX-PROG-02 (`TS-P0-PROG-02/03`) | TR-PROG-03/05/07/08 | Decision | protected managed data, ready/fail Host | Engine update/failure | Engine graph and test publication trace prove protect, failure stop, publication after readiness | failed Host => publication mutation 0 | planned local backend + test-only publication Provider | Integration | planned: `TestEngineGraphFailureStopsPublication` | planned `GHA@REMOTE_SHA:engine-graph` | harness-gap |
| MX-PROG-03 (`TS-P0-PROG-04/05`) | TR-SEC-01..05 | EP+Negative | computed/secret output canaries | Program projection | existing test asserts unknown/secret projection | ordinary inputs do not receive canary | `WithMocks` | Program | `TestRegisterPreservesComputedUpstashOutputs`, `TestRegisterFoundationGraph` | planned `GHA@REMOTE_SHA:program-properties` | candidate-unverified / implementation-present-evidence-pending |
| MX-PROG-04 (`TS-P1-PROG-01/02`) | TR-PROG-01, TR-ORDER-01 | EP | bad refs/duplicate IDs/map reorder | Program register | existing rejection/deterministic snapshot | no resources/calls on rejection | `WithMocks` | Program | `TestRegisterRejectsBeforeRegistration`, `TestRegisterIsDeterministicAcrossYAMLMapOrder` | planned `GHA@REMOTE_SHA:program-graph` | candidate-unverified / implementation-present-evidence-pending |
| MX-ORDER-01 (`TS-P0-ORDER-01/BOOTSTRAP-01/MAINT-01`, `TS-P1-MAINT-01`) | TR-ORDER-01/04/05, TR-MAINT-* | Pairwise+State | App placement 0/1/2; same/different release; first ready/fail; public on/off | Program/Engine update | existing Program tests narrowly assert deterministic projection and maintenance publication suppression; planned Engine tests cover placement boundaries and first/remaining/publication stop order | predecessor fail/not-ready => later Host/publication 0; no image-only data approval | `WithMocks`; planned local backend + test publication Provider | Program/Integration | `TestRegisterMaintenanceKeepsManagedAndLocalData`, `TestRegisterIsDeterministicAcrossYAMLMapOrder`; planned: `TestEngineAppPlacementZeroOneTwo`; planned: `TestEngineMaintenanceAndRollingFailureStop` | planned `GHA@REMOTE_SHA:engine-graph` | harness-gap |
| MX-NEON-01 (new) | TR-PROG-02..04 | Decision | official Provider resource schema, create-output and projection model chosen | register/project managed Neon | official type, protect, unknown/secret projection | no local API fallback | planned Program/Engine fixture | Program | planned: `TestRegisterOfficialNeonResourceAndProjection` | planned `GHA@REMOTE_SHA:engine-graph` | product-gap + blocked-decision |
| MX-ALLOWLIST-01 (new) | TR-PROG-06, TR-ORDER-02/03 | Decision+Pairwise | cross-Host data Host/App Host; allow absent/present; ready/fail | staged update/delete graph | allow-source -> App readiness -> publication and inverse detach order | absent allowlist/failure => App/publication 0 | planned Engine graph + test-only publication/allowlist Provider | Program/Infra | planned: `TestCrossHostAllowlistOrderingAndFailureStop` | planned `GHA@REMOTE_SHA:engine-graph` | product-gap + blocked-decision |
| MX-MICROSOCKS-01 (new) | TR-INV-03, TR-SEC-04/05 | EP+Decision | selected server/client Hosts and credentials | project/reconcile/retire MicroSocks | ownership, credential scope, local/network behavior and retire result | unrelated Host/secret/runtime untouched | planned Program + runtime seams | Product/Infra | planned: `TestMicroSocksProjectionAndRuntimeLifecycle` | planned `GHA@REMOTE_SHA:runtime-features` | product-gap + blocked-decision |
| MX-TUNNEL-01 (new) | TR-INV-03, TR-SEC-04/05, TR-PROG-05 | EP+Decision | connector placement, token, publication relation | project/reconcile/retire connector | ownership, token scope, readiness/publication relation and retire result | unrelated Host/secret/runtime untouched | planned Program + runtime seams | Product/Infra | planned: `TestTunnelConnectorProjectionAndRuntimeLifecycle` | planned `GHA@REMOTE_SHA:runtime-features` | product-gap + blocked-decision |

SingBox has no MX row: it is explicitly excluded from 001, not a skipped implementation requirement.

### 4.2 Provider / OpenSSH / Host Process

| MX ID | source | technique | prestate | stimulus | observable | no-side-effect | real/mock | owner | test symbol | GHA gate | status |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| MX-PROVIDER-01 (`TS-P0-CHECK-01`, `TS-P0-DIFF-01/02`) | TR-INV-05, TR-LC-CHECK-*, TR-LC-DIFF-* | EP+Decision | known/unknown/secret/null old/new inputs | Check/Diff | existing tests assert property preservation and conservative diff | transport/filesystem/cloud 0 | Provider + recording transport | Provider | `TestConfigAndHostCheckPreservePropertyClassesWithoutEffects`, `TestDiffIsConservativeForUnknownAndPreviewHasNoFakeID` | planned `GHA@REMOTE_SHA:provider-lifecycle` | candidate-unverified / implementation-present-evidence-pending |
| MX-SSH-01 (`TS-P0-SSH-01/03`, `TS-P0-PROTO-01`) | TR-SSH-01/02/04/06/07, TR-PROTO-01..05 | EP+BVA+Negative | valid/hostile alias; frame max-1/max/max+1 | transport run | existing tests assert fixed argv/framed stdin and malformed response rejection; planned boundary test covers exact frame limits | hostile alias starts no process; malformed/oversized response triggers no retry | recording process | Transport | `TestRunUsesFixedSSHArgvAndFramedStdin`, `TestRunRejectsHostileAliasBeforeStartingProcess`, `TestRunRejectsMalformedResponsesAndBoundsStderr`; planned: `TestProtocolFrameBoundaries` | planned `GHA@REMOTE_SHA:ssh-contract` | candidate-unverified / implementation-present-evidence-pending + harness-gap for exact boundaries |
| MX-SSH-02 (`TS-P0-SSH-02`, `TS-P1-SSH-01/02`) | TR-SSH-03/05/07, TR-OBS-05 | Decision+Negative | temporary known/changed key; cancel | loopback SSH | existing tests assert strict known-host/terminator and process-group cancellation | rejected key mutates neither known_hosts nor remote | real loopback | Transport | `TestLoopbackStrictKnownHostAndOptionTerminator`, `TestSystemStartCancellationKillsProcessGroup` | planned `GHA@REMOTE_SHA:ssh-loopback` | candidate-unverified / implementation-present-evidence-pending |
| MX-PROVIDER-SSH-01 (new) | TR-LC-CREATE-*, TR-SSH-*, TR-PROTO-* | State+Negative | Provider process, scripted SSH executable, success/lost response | Create/Read/Update via RPC | Provider process writes fixed SSH request and receives framed result | wrong frame/lost result starts no unsafely new operation | planned provider process + scripted SSH subprocess | Integration | planned: `TestProviderProcessUsesScriptedSSHTransport` | planned `GHA@REMOTE_SHA:provider-ssh` | harness-gap |
| MX-HOST-PROCESS-01 (new) | TR-INV-04, TR-PROTO-01..04 | EP+State+Negative | inspect/reconcile/bootstrap frame; second frame | invoke in-process serve/bootstrapServe or spawn test binary | existing `TestStdioServesOneInspectFrameAndRejectsWrites` / `TestBootstrapStdioServesOneReconcileFrameAndReturnsAppliedResult` cover in-process serve/bootstrapServe; `TestStdioProcessExitsAfterOneFrameAndRejectsTwo`, `TestBootstrapStdioProcessExitsAfterOneRejectedFrame`, `TestInstallAttestProcessUsesOnlyFD3` cover real test subprocess behavior | second/write-invalid frame rejected; subprocess exits; no persistent process | in-process pipes versus real test subprocess + pipe/FD3, not remote SSH | Host-process | named existing symbols; planned: `TestHostProcessWithTestOnlyRuntimeHelper` | planned `GHA@REMOTE_SHA:host-process` | candidate-unverified / implementation-present-evidence-pending |

### 4.3 Runtime / Import / Recovery / Data

| MX ID | source | technique | prestate | stimulus | observable | no-side-effect | real/mock | owner | test symbol | GHA gate | status |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| MX-LC-01 (`TS-P0-CREATE-01`) | TR-LC-CREATE-* | State | clean runtime and staged artifact | Provider Create | existing lifecycle test asserts bootstrap/inspect/checkpoint ordering | invalid artifact/ownership stops before transport | Provider recording transport | Provider | `TestLifecycleCreateBootstrapsThenInspectsAndCheckpoints`, `TestLifecycleCreateArtifactAndConfigurationFailuresPrecedeTransport` | planned `GHA@REMOTE_SHA:provider-lifecycle` | candidate-unverified / implementation-present-evidence-pending |
| MX-READ-01 (`TS-P0-READ-01/02`, `TS-P1-READ-01`) | TR-LC-READ-*, TR-HOST-06 | EP+Negative | healthy/drift/pending/unreachable/corrupt/mismatch | Provider Read | existing tests assert trusted observation and retained checkpoint/ID | no install/reconcile/retire; error is not NotFound | Provider recording transport | Provider | `TestLifecycleReadRefreshesTrustedObservationWithOneInspect`, `TestLifecycleReadFailuresPreserveCheckpointAndDoNotClaimNotFound`, `TestLifecycleReadRetirementEvidenceMustMatchManagedCheckpoint` | planned `GHA@REMOTE_SHA:provider-lifecycle` | candidate-unverified / implementation-present-evidence-pending |
| MX-IMPORT-01 (`TS-P0-IMPORT-01`, negative) | TR-LC-IMPORT-02/03 | Negative | empty import-style Read request or insufficient proof | invoke import-style Read | existing test rejects before transport | install/reconcile/ownership/runtime mutation 0 | Provider module | Provider | `TestReadRejectsImportStyleEmptyRequestBeforeTransport` | planned `GHA@REMOTE_SHA:provider-import` | candidate-unverified / implementation-present-evidence-pending |
| MX-IMPORT-02 (`TS-P0-IMPORT-01`, success) | TR-INV-06, TR-LC-IMPORT-01..05 | Decision+State | Program registered complete Host; matching machine/ownership/runtime/path/data proof | Import then preview | program-first state construction then no-op/accepted safe diff | inspect only; no install/reconcile/ownership write/render/runtime mutation | planned module seam + planned Engine local backend | Provider/Integration | planned: `TestImportBuildsReadOnlyStateFromVerifiedObservation`; planned: `TestEngineImportPreviewIsNoOpOrAcceptedDiff` | planned `GHA@REMOTE_SHA:provider-import` | product-gap |
| MX-REC-01 (`TS-P0-REC-01/02`) | TR-INV-07, TR-REC-* | State+Decision | same/different revision; response lost/received | retry reconcile | existing tests assert lock and same-operation resume | different/corrupt evidence produces no successor | tempFS + recordingRunner | Runtime | `TestRunOperationHoldsLockAcrossEffectAndResponseLossRetry`, `TestPendingResumeConflictAndCompletionAdvancesState` | planned `GHA@REMOTE_SHA:host-runtime` | candidate-unverified / implementation-present-evidence-pending |
| MX-RUNTIME-01 (`TS-P0-BG-01`, `TS-P1-RUNTIME-01`) | TR-ORDER-06/07 | State+Negative | old route; candidate start/probe/route failure | reconcile | existing tests assert rollback/candidate cleanup | old route/runtime and data sentinel preserved | tempFS + recordingRunner | Runtime | `TestProxyProbeFailureRestoresOldRouteAndCleansCandidate`, `TestReconcileBlueGreenOwnedOnlyAndTerminalReplay` | planned `GHA@REMOTE_SHA:host-runtime` | candidate-unverified / implementation-present-evidence-pending |
| MX-DATA-01 (`TS-P0-DATA-01/02`) | TR-INV-09, TR-DATA-* | BVA+Decision | dangerous changes 0/1/2; approval absent/exact/wrong; same/different retry revision; result received/lost | Update/retry | existing module tests assert exact single approval, multiple/wrong rejection and local resume; planned tests complete legal cardinality/result-loss cases | absent/wrong/multiple admission => no journal/runtime mutation | Provider/runtime separate seams | Provider/Runtime | `TestLifecycleUpdateRequestsOnlyExactSingleDataLinkApproval`, `TestLifecycleUpdateApprovalFailuresAndMultipleChangesDoNotWrite`, `TestReconcileDataApprovalAndUnownedAdmissionHaveNoJournalOrMutation`; planned: `TestDangerousDataLinkChangeCardinalityZeroOneTwo`; planned: `TestApprovalRetryRevisionBinding` | planned `GHA@REMOTE_SHA:data-approval` | candidate-unverified / implementation-present-evidence-pending + harness-gap for selected cases |
| MX-DELETE-01 (`TS-P0-DELETE-01/02`) | TR-INV-08, TR-LC-DELETE-* | State+Negative | detached target; exact/missing approval; response loss | Delete/retry | existing tests assert exact retirement/retry and data preservation | unowned/data/recovery evidence unchanged | Provider/runtime separate seams | Runtime | `TestLifecycleDeleteResponseLossRetriesTheSameRetirementOperation`, `TestRetirePreservesDataACMEAndRemovesAllSecretArtifactsInOrder` | planned `GHA@REMOTE_SHA:provider-runtime` | candidate-unverified / implementation-present-evidence-pending |
| MX-PROVIDER-RUNTIME-01 (new) | TR-LC-CREATE-*, TR-LC-UPDATE-*, TR-LC-DELETE-* | State | lifecycle request to test helper subprocess with tempFS | Provider lifecycle through CI-only helper executing the same `serve`/Runtime implementation | one Host reconcile/inspect/retire trace matches lifecycle result; this is not released Host-binary evidence | no duplicate action; helper exits; sentinels preserved | planned CI-only helper subprocess/temp root + recordingRunner | Integration | planned: `TestProviderLifecycleWithHostProcessTempRuntime` | planned `GHA@REMOTE_SHA:provider-runtime` | harness-gap |

### 4.4 Public CLI / Release / Secret Boundaries

| MX ID | source | technique | prestate | stimulus | observable | no-side-effect | real/mock | owner | test symbol | GHA gate | status |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| MX-CLI-01 (new, helper) | TR-SEC-04/05, TR-DATA-01 | EP+Decision | fake PATH, staged input, PTY | internal CLI helper | existing tests assert private stack staging/passphrase separation and exact PTY challenge mechanics | helper-specific cleanup/redaction only; it makes no public command dispatch claim | fake executables + PTY | CLI | `TestRunPulumiPlanStagesPrivateStackAndKeepsPassphraseOutOfPulumi`, `TestTerminalApprovalAdapterRequiresTerminalAndAcceptsExactPTYChallenge` | planned `GHA@REMOTE_SHA:cli-helper` | candidate-unverified / implementation-present-evidence-pending |
| MX-CLI-02 (new, public) | TR-STATE-01, TR-SEC-04/05, TR-DATA-01..03 | Decision+Negative | released CLI and ordinary user command | invoke public `pulumi` command | supported public command stages SOPS, starts attached Provider/Pulumi with exact approval channel | invalid/input failure starts neither process; declined approval may occur after both start but creates no SSH write, journal intent or runtime mutation, and reaps both processes/fd | planned CLI subprocess + fake executables/PTY | CLI | planned: `TestPublicCLIWiresPulumiProviderAndApproval` | planned `GHA@REMOTE_SHA:public-cli` | product-gap |
| MX-RELEASE-01 (new, fixture) | TR-LC-CREATE-02 | Negative | fixture components/manifest | fixture assemble/verify | existing test asserts manifest, ELF metadata, tamper rejection | invalid archive/artifact never reaches verifier consumer | release fixture | Release | `test/release-bundle.test.ts: assembles the control plane and a strict, verifiable Host artifact manifest` | planned `GHA@REMOTE_SHA:release-fixture` | candidate-unverified / implementation-present-evidence-pending |
| MX-RELEASE-02 (new, target) | TR-LC-CREATE-02, AC-12 | Decision | target exact-SHA supported release input | assemble candidate then consume it in isolation | candidate contains CLI、Program、Provider 和双架构 Host artifacts with verified manifest，active inventory不含 legacy execution surface | malformed/missing target artifact cannot reach Create | planned GHA target bundle + isolated consumer | Release | planned: `TestTargetSupportedReleaseBundleIsConsumableByProviderCreate` | planned `GHA@REMOTE_SHA:target-release` | product-gap；publish promotion由最终全部适用P0 exact-SHA/no-skip gates和candidate consumer通过后的workflow owner执行 |
| MX-SEC-01 (`TS-P0-SEC-01`) | TR-SEC-04/05, TR-OBS-03 | Negative | distinct per-layer canaries | Program/SSH/runtime execution | existing symbols only cover their named local diagnostic/property boundary | planned cross-layer scan must find zero in argv/output/log/stderr/journal/non-target Host | module seams; planned layered artifacts | Security | `TestRunDoesNotExposeStderrCanary`, `TestBootstrapRejectsInvalidMachineBeforeStateOrMutationWithoutSecretLeak`; planned: `TestLayeredSecretCanaryContainment` | planned `GHA@REMOTE_SHA:secret-boundaries` | candidate-unverified / implementation-present-evidence-pending |

## 5. Planned Exact Remote SHA GHA Gates

所有 gate 都是**planned**，当前不主张已有对应 workflow/job。动态证据只能来自 GitHub Actions 对精确 40 位远端 SHA 的 run；本地仅允许静态阅读、搜索和本文档编辑。

| Planned gate | Required future evidence | Cannot be substituted by |
| --- | --- | --- |
| `planned GHA@REMOTE_SHA:program-graph`, `:program-properties`, `:engine-graph` | exact SHA/run URL, named tests, registration/property or local-backend trace | branch/latest/cache/local claim or `WithMocks` alone |
| `planned GHA@REMOTE_SHA:provider-lifecycle`, `:provider-ssh`, `:provider-runtime`, `:provider-import`, `:data-approval` | provider/process/runtime trace and no-side-effect assertions | recording transport alone |
| `planned GHA@REMOTE_SHA:ssh-contract`, `:ssh-loopback`, `:host-process`, `:host-runtime` | temporary config/process/tempFS cleanup artifacts | VPS/public endpoint |
| `planned GHA@REMOTE_SHA:cli-helper`, `:public-cli`, `:release-fixture`, `:target-release`, `:secret-boundaries`, `:runtime-features` | CLI/release/canary or decided feature artifacts | helper/fixture-only source tests |
| `planned GHA@REMOTE_SHA:migration-rehearsal` | authorized appendix-only sanitized migration rehearsal | ordinary 001 product evidence |

Planned P0 enforcement contract: this Test Spec is the coverage inventory; CI MUST NOT create a selector registry or auxiliary manifest. Each planned workflow job owns fixed commands for the test symbols listed in its MX rows and emits `go test -json` or the runner's equivalent JSON event stream. The job parses those events directly and fails when any required named test has no terminal `pass`, emits `skip`, is absent/not-run, belongs to a different SHA, or lacks the trace artifact required by its matrix row. Current CI does not claim these planned jobs exist. Final P0 acceptance requires every applicable P0 MX to have exact-SHA `passed` evidence; local-only, mock-only, branch-only or cached-only results are not substitutes.

## 6. Requirement -> MX -> Symbol -> Planned Gate Mapping

| Requirement | MX | Existing narrow symbol / planned symbol | Planned GHA |
| --- | --- | --- | --- |
| TR-INV-01..03 | MX-PROG-01, MX-PROVIDER-01, MX-NEON-01 | `TestRegisterFoundationGraph`; planned `TestRegisterOfficialNeonResourceAndProjection` | `program-graph`, `engine-graph` |
| TR-INV-04 | MX-HOST-PROCESS-01 | `TestStdioProcessExitsAfterOneFrameAndRejectsTwo`; planned `TestHostProcessWithTestOnlyRuntimeHelper` | `host-process` |
| TR-INV-05 | MX-PROVIDER-01 | `TestConfigAndHostCheckPreservePropertyClassesWithoutEffects` | `provider-lifecycle` |
| TR-INV-06 | MX-READ-01, MX-IMPORT-01/02 | `TestLifecycleReadFailuresPreserveCheckpointAndDoNotClaimNotFound`; planned Import success symbols | `provider-lifecycle`, `provider-import` |
| TR-INV-07 | MX-REC-01, MX-PROVIDER-RUNTIME-01 | `TestRunOperationHoldsLockAcrossEffectAndResponseLossRetry`; planned lifecycle seam symbol | `host-runtime`, `provider-runtime` |
| TR-INV-08 | MX-DELETE-01 | `TestRetirePreservesDataACMEAndRemovesAllSecretArtifactsInOrder` | `provider-runtime` |
| TR-INV-09 | MX-DATA-01 | `TestLifecycleUpdateRequestsOnlyExactSingleDataLinkApproval`; planned cardinality symbols | `data-approval` |
| TR-INV-10, TR-MIG-*, TR-MIG-CLOUD-*, TR-ROLLBACK-* | MX-MIG-01/02 | planned appendix symbols | `migration-rehearsal` |
| TR-SEC-01..05 | MX-PROG-03, MX-SEC-01, MX-MICROSOCKS-01, MX-TUNNEL-01 | `TestRegisterPreservesComputedUpstashOutputs`; planned `TestLayeredSecretCanaryContainment` | `program-properties`, `secret-boundaries`, `runtime-features` |
| TR-PROG-01..08 | MX-PROG-01/02, MX-ORDER-01, MX-NEON-01, MX-ALLOWLIST-01 | existing Program symbols; planned Engine/Neon/allowlist symbols | `program-graph`, `engine-graph` |
| TR-HOST-01..06 | MX-PROVIDER-01, MX-READ-01, MX-IMPORT-02, MX-PROVIDER-RUNTIME-01 | existing Check/Diff/Read symbols; planned Import/runtime symbols | `provider-lifecycle`, `provider-import`, `provider-runtime` |
| TR-LC-CHECK-01..03, TR-LC-DIFF-01..05 | MX-PROVIDER-01, MX-DATA-01 | existing Check/Diff symbols; planned approval cardinality symbols | `provider-lifecycle`, `data-approval` |
| TR-LC-CREATE-01..04 | MX-LC-01, MX-PROVIDER-SSH-01, MX-PROVIDER-RUNTIME-01, MX-RELEASE-02 | existing Create symbols; planned transport/runtime/release symbols | `provider-lifecycle`, `provider-ssh`, `provider-runtime`, `target-release` |
| TR-LC-READ-01..05 | MX-READ-01 | existing Read symbols | `provider-lifecycle` |
| TR-LC-UPDATE-01..05 | MX-REC-01, MX-DATA-01, MX-RUNTIME-01, MX-PROVIDER-RUNTIME-01 | existing runtime/approval symbols; planned lifecycle seam symbol | `host-runtime`, `data-approval`, `provider-runtime` |
| TR-LC-DELETE-01..05 | MX-DELETE-01, MX-ALLOWLIST-01 | existing Delete symbols; planned allowlist order symbol | `provider-runtime`, `engine-graph` |
| TR-LC-IMPORT-01..05 | MX-IMPORT-01/02 | `TestReadRejectsImportStyleEmptyRequestBeforeTransport`; planned Import success symbols | `provider-import` |
| TR-SSH-01..07, TR-PROTO-01..05 | MX-SSH-01/02, MX-PROVIDER-SSH-01, MX-HOST-PROCESS-01 | existing OpenSSH/process symbols; planned Provider connection symbol | `ssh-contract`, `ssh-loopback`, `provider-ssh`, `host-process` |
| TR-REC-01..06, TR-STATE-01..04 | MX-REC-01, MX-PROVIDER-RUNTIME-01, MX-CLI-02 | existing recovery symbols; planned lifecycle/public-CLI symbols | `host-runtime`, `provider-runtime`, `public-cli` |
| TR-RETIRE-01..02 | MX-DELETE-01 | `TestLifecycleDeleteResponseLossRetriesTheSameRetirementOperation`; planned lifecycle seam symbol | `provider-runtime` |
| TR-DATA-01..04 | MX-DATA-01 | existing exact-approval symbol; planned cardinality/retry symbols | `data-approval` |
| TR-MAINT-01..03, TR-ORDER-01..07 | MX-ORDER-01, MX-RUNTIME-01, MX-ALLOWLIST-01 | existing maintenance/rollback symbols; planned Engine ordering symbols | `engine-graph`, `host-runtime` |
| TR-OBS-01..05 | MX-READ-01, MX-SSH-02, MX-SEC-01 | existing Read/loopback symbols; planned canary symbol | `provider-lifecycle`, `ssh-loopback`, `secret-boundaries` |
| AC-01 | MX-PROG-01/02/03, MX-ORDER-01, MX-NEON-01, MX-ALLOWLIST-01 | existing Program symbols; planned Engine/Neon/allowlist symbols | `program-graph`, `program-properties`, `engine-graph` |
| AC-02 | MX-LC-01, MX-READ-01, MX-IMPORT-01/02, MX-PROVIDER-RUNTIME-01 | existing lifecycle/Read symbols; planned Import/runtime symbols | `provider-lifecycle`, `provider-import`, `provider-runtime` |
| AC-03 | MX-SSH-01/02, MX-HOST-PROCESS-01 | existing OpenSSH/Host-process symbols | `ssh-contract`, `ssh-loopback`, `host-process` |
| AC-04 | MX-REC-01 | existing retry symbol | `host-runtime` |
| AC-05 | MX-RUNTIME-01, MX-ORDER-01, MX-ALLOWLIST-01 | existing rollback symbol; planned ordering/allowlist symbols | `host-runtime`, `engine-graph` |
| AC-06 | MX-DATA-01 | existing approval symbol; planned cardinality symbols | `data-approval` |
| AC-07 | MX-DELETE-01 | existing retire symbol | `provider-runtime` |
| AC-08 | MX-MIG-01/02 | planned appendix symbols | `migration-rehearsal` |
| AC-09 | MX-PROG-03, MX-PROVIDER-01, MX-PROG-02 | existing property symbols; planned Engine graph symbol | `program-properties`, `provider-lifecycle`, `engine-graph` |
| AC-10 | all applicable P0 MX | matrix-listed fixed test commands with JSON skip/absence enforcement | all applicable planned gates |
| AC-11 | L1-L4 and all MX | planned offline fixtures and no-cloud/VPS/public-network assertions | all applicable planned gates |
| AC-12 | MX-RELEASE-01/02 | fixture symbol; planned target supported release symbol | `release-fixture`, `target-release` |

## 7. Skipped / Blocked

| Area | Status | Reason | Required action / owner |
| --- | --- | --- |
| P0 final acceptance | no-skip at final acceptance | candidate source evidence and planned gates do not close P0 | controller collects exact-SHA `passed` evidence per applicable row |
| Current planned gates | CI-skippable now | no workflow existence is asserted | workflow owner creates gates before final acceptance |
| Real cloud/VPS/public endpoint | skipped by product scope | source excludes smoke/availability testing | never replace offline layer evidence |
| Business data/Docker daemon correctness | skipped by responsibility | tests prove owned ordering/preservation with sentinels | Runtime owner keeps scripted runner |
| official Neon | product-gap + blocked-decision | official model/projection decision unresolved | Program/Product owner decides |
| cross-Host allowlist | product-gap + blocked-decision | legality/ownership/sequence not frozen | Product/Infra owner decides |
| MicroSocks | product-gap + blocked-decision | runtime currently rejects; contract not frozen | Product/Infra owner decides |
| Tunnel Connector | product-gap + blocked-decision | runtime currently rejects; contract not frozen | Product/Infra owner decides |
| successful Import | product-gap | only negative rejection candidate exists | Provider/Integration implements module and Engine success tests |
| public CLI wiring | product-gap | helpers exist; CLI only exposes validate | CLI owner implements public command |
| target supported release | product-gap | fixture assembly is not the target supported release | Release owner assembles/validates candidate；CI/release workflow owner只在最终全部适用P0 gates和candidate consumer通过后promotion/publish |
| Engine graph, Provider-SSH, Provider-runtime | harness-gap | existing local seams are not connected | Integration owner adds CI-only helpers/subprocesses without production API changes |
| Frontend CDD | N/A | no visible frontend scope | reopen with feature-embedded CDD if scope changes |

## Appendix A. Migration Matrix (Not Current Implementation)

| MX ID | source | technique | prestate | stimulus | observable | no-side-effect | real/mock | owner | test symbol | GHA gate | status |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| MX-MIG-01 (`TS-P0-MIG-01`, `TS-P1-MIG-01..03`) | TR-MIG-01/02, TR-ROLLBACK-01/02 | State+Decision | sanitized old inventory and frozen old writer | shadow inspect/import/cutover/rollback | one writer trace and read-only Import | no remote Delete/data loss | future offline rehearsal | Migration | planned: `TestMigrationSingleWriterImportAndRollback` | planned `GHA@REMOTE_SHA:migration-rehearsal` | not-current-implementation |
| MX-MIG-02 (`TS-P0-MIG-02`, `TS-P1-MIG-04`) | TR-MIG-CLOUD-*, TR-ROLLBACK-03 | Decision+Negative | sanitized provider IDs/state interruption | target local-backend rehearsal | continuity/protect; target `0 create, 0 delete, 0 replace` | no source Delete/dual writer | future CI fixture | Migration | planned: `TestCloudMigrationIdentityAndZeroOperationPreview` | planned `GHA@REMOTE_SHA:migration-rehearsal` | not-current-implementation |

迁移不作为 001 当前实现或 P0 缺口的替代项；未获授权前不得引入 legacy bridge、ledger、effect registry 或额外持久 state。
