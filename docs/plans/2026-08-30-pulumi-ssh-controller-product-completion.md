# Pulumi SSH Controller Product Completion Plan

**Source:** `docs/specs/001-pulumi-ssh-controller/context.md`, `tech-spec.md`, `test-spec.md`

**Goal:** 补齐 001 产品本体，使薄 CLI、Environment Program、唯一 `Host` Provider、系统 OpenSSH、按需 `sub2api-host`、目标 release 和分层测试证据形成受支持链路。现有实例与旧云资源迁移另行处理。

## Current State And Gaps

- Host contract、Provider lifecycle、OpenSSH、远端 state/journal、blue/green、本地 data/proxy、approval 和 preserve-data 已有较完整实现，但当前源码测试尚无本轮精确远端 SHA 的动态证据。
- Public CLI 只暴露 `validate`；已有 Pulumi runner、SOPS staging、Provider attach 和 TTY approval 尚未接入。
- `Pulumi.yaml` 与 release 仍指向 legacy `./infra` Program，目标 `cmd/sub2api-environment` 尚未成为发布入口。
- Program 已注册 Host、Cloudflare DNS 和 Upstash，但当前 publication dependency、真实 Engine evidence 和目标 release evidence未闭合。
- 成功、只读、program-first Host Import 是产品 gap；现有代码只证明 import-style Read fail closed。
- Neon、cross-Host allowlist、MicroSocks、Tunnel Connector 的最小执行合同尚未冻结，保持 `BLOCKED`，不得从已有字段猜测实现。

## Shared Constraints

- Preserve: 保留 Host identity、revision、approval、secret、OpenSSH、journal、unknown-result、blue/green、Read 保留 ID 和 preserve-data Delete 合同。
- Preserve: `infra/**` 与现有 legacy writer 保持不变；当前产品实现不切换现有生产实例、不移动 state、不触碰 cloud physical IDs。
- Exclude: 迁移、production cutover、真实云/VPS/公网、业务数据正确性、SingBox、第二套 engine/graph/state、常驻 Agent、controller service、operation DB 和 test selector registry。
- Verification: 本地仅允许读取、编辑、搜索、`git diff --check` 等轻量静态检查。所有测试、build、vet、race、Node、Pulumi、SSH、Docker 和 release assembly 只在精确远端 SHA 的 GitHub Actions 中执行。
- TDD: 每个行为任务先发布 test-only RED SHA；只有确认预期行为失败后才发布最小实现 GREEN SHA。
- Scheduling: 并行任务写路径不得重叠；`.github/workflows/**`、`go.mod`、`go.sum` 和 release shared artifacts 串行接管。

## Tasks

### Task 1: Exact-SHA CI Evidence Foundation

**Owns:** `.github/workflows/ci.yml`, `test/controller-ci.test.ts`

**Depends on:** none

**Produces:** 精确 SHA checkout/核验、首批固定 candidate symbols 的 JSON event、统一 trace artifact intake 与 SHA-bound evidence artifacts；并准备/核验后续 required OpenSSH loopback 所需的离线工具环境，但不在本 Task 强制 loopback symbol。不新增 selector manifest。后续 Task 只向该通用 evidence contract 增加自己的 symbols/traces，最终 no-skip 收敛由 Task 10 接管。

**Requirements:** 首批只覆盖不依赖新增 harness 的现有 candidate symbols，排除 loopback、Engine、Provider-process 和 runtime-helper planned cases；planned 产品/harness tests 不得伪装成 existing gates。Workflow 固定 package commands并保留完整 JSON stream，核对 checkout HEAD 与 40 位 evidence SHA；当前已枚举 required candidate skip/not-run 必须失败。离线 prerequisites 只安装/核验固定 OpenSSH loopback 工具，并向 Tasks 5-9 提供统一 trace artifact 上传约定，不提前把后续 symbols设为required。

**Acceptance:** 精确远端 SHA 的 GHA run 证明 CI contract 本身工作，且 claim 仅限 evidence foundation。

### Task 2: Public Pulumi CLI

**Owns:** `cmd/sub2api-deploy/main.go`, `cmd/sub2api-deploy/main_test.go`

**Depends on:** Task 1 的 evidence contract

**Consumes / Produces:** 复用 `parsePulumiPlan`, `runPulumiPlan`, staged Stack, attached Provider 和 `terminalApproval`；公开 `pulumi ENV preview|up|refresh|destroy|import`。

**Preserve:** `validate ENV` 行为；CLI 不保存 plan/state/ledger；成功 Import 由 Task 8 负责。

**Acceptance:** Public subprocess test 证明 dispatch；invalid command、解析/SOPS/staging failure 时 Provider/Pulumi zero-start；declined approval 时允许 Provider/Pulumi 已启动，但必须在 SSH remote write、journal intent 和 runtime mutation 前失败并清理全部进程/fd；精确 SHA RED/GREEN。

### Task 3: Target Pulumi Project And Program Build

**Owns:** `Pulumi.yaml`, `scripts/build-pulumi-release.sh`, `cmd/sub2api-environment/main_test.go`, target Program contract tests

**Depends on:** Task 1

**Produces:** project name `sub2api-environment`，`pulumi-program` 由 `./cmd/sub2api-environment` 构建，并继续从 executable-relative bundle 加载 Host artifacts。

**Preserve:** 不删除或修改 `infra/**` 实现，不增加 legacy fallback。

**Acceptance:** 精确 SHA GHA 证明 project identity 与 target Program build source。

### Task 4: Current Publication Dependency Graph

**Owns:** `internal/program/program.go`, `internal/program/program_test.go`

**Depends on:** Task 1

**Produces:** 当前已支持 DNS publication 的每个 resource 直接依赖该 App `publicAccess.servers` 对应的全部 Host URNs；因为 validation 要求它是 `app.servers` 子集，App placement 的 stable serial edges继续提供传递依赖。多 Host ordering 和 maintenance suppression 保持确定。

**Requirements:** Program graph tests必须断言直接 dependency URN 集合、App placement `0/1/2`、public subset、maintenance、前序 failure stop 和删除时 publication 先于 Host removal 的逆序语义；不新增 barrier/scheduler，不实施 Neon、cross-Host data、MicroSocks、Connector 或非 DNS Cloudflare。

**Acceptance:** Program graph RED/GREEN 证明全部 target Host dependencies、stable ordering、maintenance、protect 和 secret/unknown 不回归。

### Task 5: L1 Pulumi Engine Graph Harness

**Owns:** `internal/integration/enginegraph/enginegraph_linux_test.go`, `internal/integration/enginegraph/providers_linux_test.go`, `internal/integration/enginegraph/testdata/**`; 若当前 Pulumi dependencies不足，本 Task 串行独占 `go.mod`, `go.sum`

**Depends on:** Tasks 3 and 4

**Produces:** real Pulumi local backend + test-only Host/cloud Provider plugins，验证 Engine scheduling、failure stop、publication gating、partial checkpoint 和 property semantics。

**Preserve:** 不连接真实云，不增加 production Provider 或 public API。

**Acceptance:** Test Spec 的 configured server、placement、ready/fail 和 publication cases 在精确 SHA GHA 通过。

### Task 6: L2 Provider Process And SSH Harness

**Owns:** `internal/integration/providerssh/providerssh_linux_test.go`, `internal/integration/providerssh/testdata/**`, `internal/hostprotocol/protocol_test.go`, `internal/openssh/openssh_test.go`, `internal/openssh/loopback_linux_test.go`, `internal/openssh/process_linux_test.go`

**Depends on:** Task 1 的 evidence contract和离线前置环境

**Produces:** real Provider process -> scripted `ssh` subprocess，以及 required OpenSSH loopback；补 frame `max-1/max/max+1`。

**Acceptance:** fixed argv/stdin、host key、frame、cancel、response-loss 和 zero unsafe retry 在精确 SHA GHA 无 skip。

### Task 7: L3 Provider Lifecycle And Temporary Runtime Harness

**Owns:** `internal/hostruntime/testonly/**`, `internal/integration/providerruntime/providerruntime_linux_test.go`, `internal/integration/providerruntime/testdata/**`

**Depends on:** Task 1

**Produces:** test helper subprocess 执行相同 `serve`/Runtime implementation，使用独立 temp roots、recording runner、journal 和 sentinels；不冒充 released Host binary。

**Acceptance:** Create/Read/Update/Delete、response loss、legal approval decision cases、secret isolation 和 preserve-data 在精确 SHA GHA 通过。

### Task 8: Read-only Program-first Host Import

**Owns:** `internal/hostprovider/provider.go`, `internal/hostprovider/lifecycle.go`, `internal/hostprovider/provider_test.go`, `internal/hostprovider/lifecycle_test.go`, `internal/integration/providerimport/providerimport_linux_test.go`, `internal/integration/providerimport/testdata/**`

**Depends on:** Tasks 5、6、7；分别消费 local-backend Engine harness、Provider-process/scripted transport fixture、read-only temporary Runtime helper

**Produces:** 由完整 Program inputs 与只读 remote evidence 构造 state；普通 Read 不回归。

**Preserve:** Import 只 inspect，不 install/reconcile/ownership write/render/runtime mutation；不读取 legacy inventory、不切 writer。

**Acceptance:** module 和 local-backend Import preview 精确 SHA GREEN，trace 只有 inspect。

### Task 9: Target Supported Release Candidate

**Owns:** `scripts/release-bundle.sh`, `scripts/release-bundle-files.txt`, `scripts/verify-release.sh`, `scripts/pulumi-go-shim.sh`, `test/release-bundle.test.ts`, `test/pulumi-runtime.test.ts`, `Pulumi.production.example.yaml`, `README.md`

**Depends on:** Tasks 2、3、4、8；只消费 public CLI、target Program build、accepted Program graph 和 Import-capable Provider binary

**Produces:** 包含 CLI、Environment Program、Host Provider 和双架构 Host artifacts 的 target candidate bundle；active execution surface 不再依赖 legacy Program。

**Preserve:** legacy source 留在仓库；本 Task 不修改 publish-capable `.github/workflows/release.yml`，只产出 candidate artifact contract；BLOCKED 产品合同未关闭前不宣称完整产品 release。Target bundle active inventory不得包含 `infra/**`、legacy `command.local.Command`、Compose/SingBox orchestration；README/example不得暗示已迁移或支持 blocked能力。

**Acceptance:** exact-SHA assembled artifact 与 isolated consumer 通过；manifest、hash、size、ELF arch、Program/Provider/artifact discovery 均可审计。

### Task 10: L1-L4 Gate Convergence

**Owns:** `.github/workflows/ci.yml`（从 Task 1 accepted foundation 接管）, `.github/workflows/release.yml`

**Depends on:** Task 1 和 Tasks 2-9；消费 foundation workflow、全部 accepted symbols/traces 与 target candidate artifact

**Produces:** Test Spec 当前适用 P0 的 exact-SHA、no-skip、SHA-bound evidence，并且 publish job 只有这些 gates和target candidate consumer都通过后才可运行；不运行 migration rehearsal。

**Acceptance:** TR/AC -> MX -> test -> gate -> SHA/run/artifact 可逆；BLOCKED rows 仍明确未通过，不越界声称完整 001 完成。

## Blocked Product Work

| Area | Missing contract | Reopen condition |
| --- | --- | --- |
| Official Neon | official package/resource schema、create outputs、protect、secret/unknown projection；`tech-spec.md` §11、`test-spec.md` `MX-NEON-01`，当前 `internal/program/program.go` preflight拒绝 | owner 冻结产品 create 合同；不得等待迁移 import |
| Cross-Host PostgreSQL/Redis | allow-source legality、ownership、readiness、add/remove order；`MX-ALLOWLIST-01`，当前 Program preflight拒绝 | owner 冻结合同 |
| MicroSocks | server/client runtime、credential scope、network、retire/rollback；`MX-MICROSOCKS-01`，当前 Program/runtime拒绝 | owner 冻结合同 |
| Tunnel Connector | placement、token scope、publication/readiness、ownership、retire；`MX-TUNNEL-01`，当前 Program/runtime拒绝 | owner 冻结合同 |
| Production readiness | machine identity 实机适用性、最小 `sudo -n` 权限；`tech-spec.md` §11 | production owner 提供证据，不阻止离线产品链实施 |

## Waves

1. Task 1。
2. Tasks 2、3、4、6、7 并行，分别独立 review；路径不重叠。
3. Task 5 在 Tasks 3、4 accepted 后执行，可与尚未结束的 Tasks 2、6、7 并行；若需 `go.mod/go.sum`，此时独占。
4. Task 8 在 Tasks 5、6、7 后执行。
5. Task 9 在 Tasks 2、3、4、8 后执行，只生成 candidate，不发布。
6. Task 10 最后接管 CI/release workflows，安装最终 gates和publish promotion，并做 integration review。

## Verification And Closure

- 每个 Task 的 RED 与 GREEN 必须是不同精确远端 SHA 的 GHA evidence；环境/fixture/workflow failure 不算行为 RED。
- 每个 reviewer 直接读取 `context.md`、approved Tech/Test Specs、本计划、当前 net diff 和 evidence；finding 回到 owning Task 修复。
- 本计划可关闭未被 owner decision 阻塞的产品主链。Blocked Product Work 未关闭前，不宣称完整 001 产品实现完成。
- 计划完成不会自动迁移现有实例、停用旧 writer、移动 cloud state 或执行 production cutover。
