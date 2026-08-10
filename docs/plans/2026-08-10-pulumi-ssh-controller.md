# Pulumi SSH Controller Implementation Plan

**Source:** `docs/specs/001-pulumi-ssh-controller/context.md`, `tech-spec.md`, `test-spec.md`

**Goal:** 完整实现一个Environment Pulumi Stack、官方Cloudflare/Neon/Upstash Providers、每服务器唯一Host resource、系统OpenSSH和按需退出的`sub2api-host`，并保留现有运行资源和数据。

## Current State And Gaps

- 当前生产实现仍是每VPS一个本地Pulumi Stack，通过`command.local.Command`调用Shell/TypeScript。
- 当前未提交validate切片已经实现严格YAML、SOPS解密、引用/secret校验和OpenSSH alias预检，但拒绝maintenance空placement，并把server key和SSH alias混为同一值。
- 现有Compose、blue/green、preflight、state和code2 adoption测试提供行为证据，但目标Host Provider、远端runtime和Environment Program尚未实现。
- Cloudflare和Upstash已有官方Provider实现；Neon当前混合第三方alpha Provider与本地API command，目标官方Provider的无损迁移能力需独立验证。

## Shared Constraints

- Preserve：现有云physical IDs、URN映射、protect/retention、Host machine/ownership、Compose identity、active image/route、volume/bind data和业务数据。
- Exclude：业务数据迁移、真实公网/云可用性/最终用户路径冒烟测试、control ledger、approval PKI、effect registry和第二套plan/state引擎。
- Go固定`1.25.11`。
- 所有行为变更先写失败测试，观察RED后再实现GREEN。
- 共享checkout中每波writer独占路径，不stage、不commit；`go.mod/go.sum`一次只由一个任务拥有。
- 禁止本地构建：不得执行`go build`、`npm run build`、release bundle assembly或其他binary/发布产物构建命令。构建和产物验证只在CI运行；本地只运行单元测试、vet、格式/语法检查和不生成发布产物的rehearsal。

## Tasks

### Task 1: 收敛Environment配置和validate切片

**Owns:** `internal/environment/**`, `internal/sshcheck/**`, `cmd/sub2api-deploy/**`, 当前`go.mod`直接依赖调整

**Depends on:** none

**Produces:** 稳定server key与独立OpenSSH alias；允许仅在public access关闭时使用`apps.<app>.servers: []`；排序后的alias投影；现有strict YAML/SOPS/secret redaction保持。

**Acceptance:** focused RED/GREEN记录；`go test -count=1 ./internal/environment ./internal/sshcheck ./cmd/sub2api-deploy`和对应`go vet`通过。

### Task 2: Host语义合同和机器协议

**Owns:** `internal/hostcontract/**`, `internal/hostprotocol/**`

**Depends on:** Task 1，因为它消费稳定配置身份语义。

**Produces:** Host完整语义目标、最小secret投影、identity/data identity/approval subject、target revision、bounded single-frame request/response和错误分类。

**Acceptance:** malformed framing fail closed；secret rotation改变revision但不产生明文/裸secret digest oracle；纯合同测试不访问SSH/filesystem/cloud。

### Task 3: Provider框架与唯一Host schema checkpoint

**Owns:** `go.mod`, `go.sum`, `cmd/pulumi-resource-sub2api-host/**`, `internal/hostprovider/**`, `internal/hostresource/**`

**Depends on:** Task 2，因为Provider schema消费冻结的Host合同。

**Produces:** 与Go 1.25.11兼容的Provider依赖；只暴露一个Host token；unknown/secret/private state harness。

**Acceptance:** schema只有Host；Check/Diff transport为零；Provider binary由CI成功构建。

### Task 4: 系统OpenSSH transport

**Owns:** `internal/openssh/**`, `internal/artifact/**`; 接管并收敛`internal/sshcheck/**`

**Depends on:** Tasks 2-3，因为它消费protocol且不能与module checkpoint冲突。

**Produces:** 固定argv/stdin transport、host-key fail closed、hostile alias拒绝、process cleanup、pinned artifact安装。

**Acceptance:** recording process与临时OpenSSH loopback通过；不连接VPS或公网。

### Task 5: 远端Host state、inspect、lock和恢复核心

**Owns:** `cmd/sub2api-host/**`, `internal/hostruntime/**`中的state/inspect/lock/journal

**Depends on:** Task 2，因为runtime实现同一protocol。

**Produces:** 非驻留process、只读inspect、machine/ownership evidence、单Host writer lock、unknown-result resume。

**Acceptance:** corrupt state只读失败；相同operation重试不重复副作用；请求后无常驻进程。

### Task 6: 本机reconcile、blue/green和preserve-data retire

**Owns:** 接管`internal/hostruntime/**`并按需修改`compose/**`, `traefik/**`和目标runtime测试

**Depends on:** Task 5，因为reconcile依赖持久恢复合同。

**Produces:** Host内部派生布局；本机readiness；App/data/proxy/connector收敛；blue/green rollback；`servers: []`停writers；preserve-data retire。

**Acceptance:** route切换未知结果至多一次；失败保留旧runtime/route；Delete后data/unowned sentinel不变；不做冒烟测试。

### Task 7: Host Provider完整生命周期

**Owns:** 接管`cmd/pulumi-resource-sub2api-host/**`, `internal/hostprovider/**`

**Depends on:** Tasks 3-5；集成Task 6的真实runtime合同。

**Produces:** Check/Diff/Create/Read/Update/Delete/Import；Create完整install+reconcile；Read保留ID；Import只读；Delete preserve data。

**Acceptance:** test spec中Provider P0通过；local Provider process integration通过且不连接真实Host。

### Task 8: Environment Program和官方Provider图

**Owns:** `cmd/sub2api-environment/**`, `internal/program/**`

**Depends on:** Tasks 1, 3和已验证的官方Neon资源模型。

**Produces:** 每server一个Host；官方Cloudflare/Neon/Upstash资源；protect、unknown、secret和依赖；无`command.local.Command`。

**Acceptance:** Pulumi mocks与local backend证明resource graph；不连接真实云账号。

### Task 9: CLI、SOPS与一次性批准

**Owns:** 接管`cmd/sub2api-deploy/**`，必要时`internal/cli/**`

**Depends on:** Tasks 1, 3, 8。

**Produces:** validate、标准Pulumi命令薄包装、process-scoped批准channel；不保存plan或事务状态。

**Acceptance:** fake SOPS/Pulumi/Provider下批准精确绑定、清理和secret redaction通过。

### Task 10: Single-writer和identity-preserving迁移

**Owns:** `internal/migration/**`, sanitized fixtures, migration CLI/docs/tests

**Depends on:** Tasks 6-9和生产inventory/官方Neon import证据。

**Produces:** bounded inventory、shadow inspect、writer freeze、adoption、program-first Import、cloud state rehearsal和cutover。

**Acceptance:** Host Import零mutation；old/new writer不交叉；cloud目标preview为0 create/delete/replace；不移动业务数据。

### Task 11: Release、CI、旧实现退出和完整集成

**Owns:** 最终接管`infra/**`, `Pulumi.yaml`, release/build scripts, workflows, README和旧测试迁移；最后收敛`go.mod/go.sum`

**Depends on:** Tasks 6-10全部接受。

**Produces:** 发布包包含Environment Program、Host Provider、`sub2api-host`和薄CLI；旧writer退出。

**Acceptance:** 全部P0无skip；迁移/Delete/single-writer相关P1通过；完整仓库verify通过；无公网或云可用性冒烟命令。

## Scheduling Waves

1. Wave 1：Task 1；并行只读收敛Neon、machine identity、revision、approval和Cloudflare blockers。
2. Wave 2：Task 2。
3. Wave 3：Task 3。
4. Wave 4：Tasks 4、5、8中不依赖未决Neon的基础部分，独占路径并行。
5. Wave 5：Tasks 6、7、9并行，集成后独立review。
6. Wave 6：Task 10串行。
7. Wave 7：Task 11串行集成和最终review。

## Verification And Closure

- 每波记录RED、GREEN、相关suite与独立review。
- 每个接受finding由原writer修复、原reviewer定向复查。
- 本地验证命令不得包含构建；CI负责所有binary、前端bundle和release产物构建证据。
- 最终按`test-spec.md`逐项映射P0/P1证据和未决外部能力。
- 不因外部Provider/生产inventory blocker伪造完成；可独立完成的实现继续推进。
