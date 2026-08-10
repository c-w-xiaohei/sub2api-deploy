# Pulumi SSH Controller 一般测试规格

状态：`Draft for implementation review`

日期：2026-08-10

## 1. 文档定位

本文定义 [tech-spec.md](./tech-spec.md) 的验证方向、真实 seam、P0/P1 场景和迁移证据。
它验证公开合同与高风险负向行为，不冻结生产内部结构，也不声明实现已完成或可投产。

需求权威来源是 [context.md](./context.md)。
Tech requirements 与测试通过稳定的 `TR-*`、`AC-*` 和 `TS-*` ID 人工追踪，不生成额外 manifest。

## 2. 测试目标

测试必须共同证明以下控制链：

```text
Environment Pulumi Stack
-> official Cloudflare / Neon / Upstash Provider resources
-> exactly one Host resource per server
-> Host Provider through system OpenSSH
-> on-demand sub2api-host process
-> safe local reconcile, recovery and preserve-data retirement
```

优先验证：

- Program graph、引用、每服务器一个 Host、official Provider wiring 和 resource options。
- Pulumi protect、unknown、secret 与 dependency 语义。
- Check/Diff 纯度，Read/Import 只读及保留 ID。
- OpenSSH argv/stdin、host-key fail-closed、协议 framing 与进程清理。
- SSH unknown result 下同一 operation 的单副作用恢复。
- blue/green rollback、多 Host stable order 和 failure stop。
- data-link approval 的零副作用 guard。
- Delete preserve data 与 unowned objects。
- legacy/new single-writer 和 cloud/Host identity-preserving migration。

## 3. 不测试的内容

- 不测试 Cloudflare、Neon、Upstash 真实云 API 的可用性、SLA、配额、限流、区域或网络质量。
- 不做任何真实 public endpoint、云服务可用性或最终用户路径冒烟测试，也不把冒烟结果作为部署后验收或完成门槛。
- 不测试 PostgreSQL/Redis 业务数据复制、恢复、内容正确性或应用 schema 语义。
- 不穷举内部 helper、Compose name、path、slot、journal phase 或目录权限组合。
- 不使用 pairwise generator、covered-pair lockfile 或固定 seed 治理。
- 不要求 mutation score 100% 或 mutation governance 清单。
- 不建立所有内部 phase 与 failure point 的笛卡尔矩阵。
- 不建立 effect registry、production effect ID manifest 或 side-effect registry。
- 不建立 test selector grammar、selector registry 或 resolved-tests lock。
- 不以行覆盖率、测试总数或旧测试数量代替安全语义。
- 不在本地运行`go build`、`npm run build`、release bundle assembly或其他构建命令；binary、前端bundle和release产物只由CI构建和验证。

官方 Provider wiring 必须测试。
测试 oracle 是 Program 注册的官方 resource types、provider refs、inputs/outputs、dependencies、protect 和 Pulumi property 语义，不是真实云服务是否在线。
T2中的Host本机health/readiness检查只验证reconcile和blue/green的本机安全前置条件，不是冒烟测试；它不得连接真实公网endpoint或扩展成端到端用户路径验证。

## 4. 精简测试层级

| 层级 | 真实 seam 与运行方式 | 主要证据 | 不承担 |
| --- | --- | --- | --- |
| T0 Contract | Go table tests，必要处小范围 fuzz；覆盖 config/projection、Check/Diff、identity 和 protocol codec。 | canonical result、property class、stable error、transport call count 为零。 | 不模拟完整 Engine，不按 helper 结构写断言。 |
| T1 Provider/OpenSSH | Provider harness 加 recording process seam；少量真实 OpenSSH loopback。 | lifecycle ID/result、argv/stdin/frame、error class、operation key、host-key 和 process cleanup。 | fake transport 不冒充真实 host-key 证据。 |
| T2 Host Runtime | 真实 `sub2api-host` process 加临时 state/runtime tree 与 recording runtime commands。 | journal、action trace、route/active runtime、data/unowned sentinel、请求后无常驻进程。 | 不验证业务数据内容，不穷举 phase。 |
| T3 Program/Engine/Migration | Pulumi Go mocks 加少量 local-backend CLI rehearsal。 | official resources、graph edges、protect、unknown/secret、URN mapping/physical ID、preview operations 和 writer trace。 | 不使用真实云账号，不另建部署控制系统。 |

只有实现出现新的独立故障边界时才增加 fixture 或层级。
优先保持一个 Provider process seam 与一个 Host runtime command seam，避免为测试引入大量生产接口。

## 5. 通用 Oracle

高风险测试不能只断言 `error != nil`，必须至少带一个副作用、identity、顺序或 preserve 类负向 oracle。

| Oracle | 用法 |
| --- | --- |
| Call count | SSH、install、reconcile、Docker、route、data-link、retire 或 provider delete 为 0 或至多 1。 |
| Identity | Host/resource ID、old-to-target URN mapping、physical provider ID、machine identity、volume/bind identity、active image 前后比较。 |
| Sentinel | data file、route、remote state、journal 与 unowned inventory 前后摘要比较。 |
| Order trace | data allow-source、App readiness、publication 和 detach/delete 的先后关系。 |
| Protocol | stdout 恰好一个有界 frame，stderr 不解码，污染和截断 fail closed。 |
| Secret canary | argv、普通 outputs、logs/errors、stderr、journal 和非目标 Host 中零命中。 |
| Process | 操作结束或 cancel 后无常驻 `sub2api-host`、`ssh` 或 ProxyCommand 子进程。 |

## 6. P0 具名场景

P0 是实现安全主路径的阻断条件。
每个场景至少在最接近责任的层执行；涉及 Pulumi Engine 语义时增加窄 T3 证据。

| ID | 场景与追踪 | 层级 | 正向 oracle | 必须的负向 oracle |
| --- | --- | --- | --- | --- |
| TS-P0-PROG-01 | 一个 Environment 直接注册官方 Cloudflare/Neon/Upstash resources，并对每台 server 恰好一个 Host。追踪 TR-PROG-01/02。 | T3 | official tokens/provider refs、Host 数量和 per-Host projection 正确。 | 无 cloud wrapper；引用错误时注册数为零；Program/preview 不 SSH。 |
| TS-P0-PROG-02 | managed data、local data Host、App Hosts 和 public access dependencies。追踪 TR-PROG-04/05/06/07。 | T3 | allow-source -> App ready -> publication；删除顺序反向。 | Host 未 ready 时 publication mutation 为零；无无关 dependency。 |
| TS-P0-PROG-03 | managed Neon/Upstash 设置 protect。追踪 TR-PROG-03。 | T3 | resource options 明确 `protect=true`，适用 retention 独立记录。 | 删除 preview 被 Pulumi 阻断；不能只用 retention 冒充 protect。 |
| TS-P0-PROG-04 | Provider computed output 在 preview 为 unknown。追踪 TR-SEC-01/02。 | T0/T3 | Host nested input 保持 unknown，preview 可完成。 | unknown 不变为空值；Host/publication mutation 与 SSH 为零。 |
| TS-P0-PROG-05 | cloud 与 Host secrets 跨 projection 保持 secret。追踪 TR-SEC-01/04/05。 | T0/T3 | 目标 Host input 保留 secret bit。 | 普通 stack/Host outputs、非目标 Host 和诊断中无 canary。 |
| TS-P0-CHECK-01 | Check 覆盖 known、unknown、secret-known、secret-unknown、null 与 absent。追踪 TR-LC-CHECK-*。 | T0 | 合法 property class 保真，非法 known value 返回字段错误。 | transport/filesystem/cloud calls 均为零；unknown/secret bit 不丢失。 |
| TS-P0-DIFF-01 | Diff 区分 canonical no-op、ordinary in-place、dangerous data-link 和 unknown。追踪 TR-LC-DIFF-*。 | T0 | changes 和 changed paths 正确，普通变化不 replace。 | SSH/inspect/approval side effects 为零，unknown 不当作确定 diff。 |
| TS-P0-DIFF-02 | server key、OpenSSH alias 与 machine replacement lifecycle。追踪 TR-HOST-01..05、TR-LC-DIFF-05。 | T0/T1/T3 | 普通配置和 alias input 变化 preview 为 in-place；同机 rename 仅接受明确 alias/state move；物理替换显示 staged new/old Hosts。 | 普通配置不产生隐式 replace/delete；server key 直接变化在 preview fail；alias 变化后 machine mismatch 在 Update inspect fail closed，不自动 replacement。 |
| TS-P0-CREATE-01 | Create 从无 remote binary 完成 verified install 加完整 reconcile。追踪 TR-LC-CREATE-*。 | T1/T2 | install、identity bind、reconcile、readiness 和 outputs 完整完成。 | 不要求外部 bootstrap；不执行 `curl | sh`；冲突 runtime 不被接管。 |
| TS-P0-READ-01 | Read healthy/drift/pending，并覆盖 unreachable、missing binary、corrupt state 和 identity mismatch。追踪 TR-LC-READ-01..04。 | T1 | 只更新可信稳定 observation，所有 present/error case 保留 ID。 | reconcile/install/recover/retire 为零；错误不得 NotFound 或清空 ID。 |
| TS-P0-READ-02 | Read 检查 preserve-data retirement evidence。追踪 TR-LC-READ-05。 | T1/T2 | 只有 matching resource identity、machine identity 且格式合法的 managed evidence 报告 lifecycle ended。 | wrong resource、wrong machine 或 malformed evidence 均保留 ID 并 error；Read mutation 为零。 |
| TS-P0-IMPORT-01 | program-first read-only Import。追踪 TR-LC-IMPORT-*。 | T1/T3 | identity/ownership 可证明时构造 state，后续 preview no-op。 | install/reconcile/ownership write/runtime mutation 为零；证据不足不接管。 |
| TS-P0-SSH-01 | 系统 OpenSSH 以固定 argv 启动，安全 alias 是单参数并由 `--` 或已验证等价合同隔离，request 走 stdin。追踪 TR-SSH-01/04/06。 | T1 | 捕获的 executable、argv、alias 和 stdin framing 符合合同。 | 不启动本地 shell；secret 不在 argv/env；无 arbitrary command。 |
| TS-P0-SSH-02 | unknown/changed host key fail closed。追踪 TR-SSH-03。 | T1 loopback | 返回明确 host-key transport error。 | 不使用 `no`/`accept-new` 绕过；known_hosts 不变；远端 mutation 为零。 |
| TS-P0-SSH-03 | hostile alias 覆盖空值、前导 `-`、空白/control/DEL、metacharacter、`user@host`、`host:port`、URI 和多 destination 形式。追踪 TR-SSH-02/06/07。 | T0/T1 | 合法最小 grammar alias 保持单 token，并由系统 OpenSSH config 解析。 | hostile alias 在 process start 前失败；SSH 与 remote mutation 调用均为零；Provider 不解析 SSH config。 |
| TS-P0-PROTO-01 | 单帧、空、截断、双帧、banner/trailing、超限和版本不兼容。追踪 TR-PROTO-*。 | T0/T1 | 唯一合法有界 response 可解码，错误分类稳定。 | malformed response 不触发第二次 write；stderr 不参与解码。 |
| TS-P0-REC-01 | 请求可能到达后发生 EOF/255/timeout/cancel/半帧，随后相同 operation 重试。追踪 TR-REC-01/02/03/04。 | T1/T2 | resume 或返回原 terminal result，最终 revision 正确。 | operation 只有一个；bootstrap/route switch/retire 等非幂等 action 至多一次。 |
| TS-P0-REC-02 | pending 不同 revision 或 corrupt/contradictory journal。追踪 TR-REC-05/06。 | T1/T2 | conflict 或 recovery-required，保留 evidence。 | 不覆盖 journal、不启动 successor operation、不清理 data。 |
| TS-P0-BG-01 | inactive runtime 启动或 probe/route readiness 失败。追踪 TR-ORDER-06/07。 | T2 | 旧 active runtime 和 route 保持或恢复。 | 旧 runtime stop/remove 为零；新 active state 不提交。 |
| TS-P0-ORDER-01 | 多 Host image rolling 与本地 data Host 顺序。追踪 TR-ORDER-01/02/05。 | T0/T3 | stable server order；allow-source 先于 App，再 publication。 | map 重排不改变顺序；失败 Host 后续调用为零。 |
| TS-P0-BOOTSTRAP-01 | 首次多 Host App bootstrap，初始所有 Hosts 均未运行 App。追踪 TR-ORDER-04。 | T2/T3 | 仅稳定第一台 Host 启动；其 ready 后才按序启动其他 Hosts。 | 第一台失败或 not ready 时后续 Host 启动调用为零，publication mutation 为零。 |
| TS-P0-MAINT-01 | public access 关闭后 App 使用 `servers: []`，再按 first/remaining/public 恢复；不兼容 image update 复用同一流程。追踪 TR-MAINT-01..03。 | T2/T3 | App 定义和 data links 保留；原 Hosts 停 owned runtime/writers 并 preserve data；恢复顺序正确。 | 空 placement 不发布；不增加 maintenance resource/transaction；image-only flow 不伪造 data identity change 或 data approval。 |
| TS-P0-DATA-01 | data identity 改变但批准缺失、错误、已消费或 target revision 不匹配。追踪 TR-DATA-01/02。 | T0/T1/T2 | 返回 approval-required/conflict。 | SSH write、render、restart、route、journal mutation 全为零。 |
| TS-P0-DATA-02 | 有效批准仅绑定 exact old/new identity 与 revision，unknown result 后恢复。追踪 TR-DATA-02/03/04。 | T1/T2 | exact operation 可继续并 resume。 | proof 不可跨 Host/revision 复用；无 dump/restore/copy/schema tool 调用。 |
| TS-P0-DELETE-01 | approved `retire --preserve-data`。追踪 TR-LC-DELETE-*、TR-RETIRE-01。 | T1/T2 | 只清理 deploy-owned runtime shell，并写 retirement evidence。 | volume、bind/data、业务 sentinel、unowned objects 和恢复 evidence 不变。 |
| TS-P0-DELETE-02 | Delete 不可达、identity mismatch、corrupt state 或缺批准。追踪 TR-LC-DELETE-03/04。 | T1/T3 | operation 失败并保留 Pulumi failure/state。 | Host ID 不伪装 absent；Host/runtime/data mutation 为零。 |
| TS-P0-MIG-01 | legacy writer freeze、adoption、program-first Import、writer cutover。追踪 TR-MIG-01/02。 | T2/T3 | 任一时刻恰有一个 writer，切换 trace 可审。 | old/new mutation trace 不交叉；旧入口每次 mutation 前被阻断。 |
| TS-P0-MIG-02 | cloud resource move/import 保留 physical identity、resource continuity 与 protection。追踪 TR-MIG-CLOUD-*。 | T3 | physical provider ID、`old URN -> expected target URN`、provider closure、protect/retention/secret taint 与 inventory 一致；仅同 Stack/project 要求 URN 字面相等。 | 跨 Stack 不误要求 URN 相等；目标 preview `0 create, 0 delete, 0 replace`；不调用 source Delete。 |
| TS-P0-SEC-01 | 各类 canary secret 贯穿 Program、Provider、SSH 和 Host。追踪 TR-SEC-04、TR-OBS-03。 | T0-T3 | 只在 Pulumi secret property 和明确 runtime secret artifact 中可见。 | argv、普通 output、logs/errors、stderr、journal、snapshot 零非法命中。 |

## 7. P1 具名场景

P1 补充兼容、诊断与迁移边界。
与 migration、Delete 和 single-writer 相关的 P1 必须在生产切换前完成。

| ID | 场景与追踪 | 层级 | 关键 oracle |
| --- | --- | --- | --- |
| TS-P1-PROG-01 | 缺失 server 引用、重复 identity 或依赖环。追踪配置合同、TR-ORDER-01。 | T0 | 任何 resource 注册前 fail，错误稳定指出引用或环。 |
| TS-P1-PROG-02 | 两个 Host 的 secret 与 dependencies 隔离。追踪 TR-SEC-01/04。 | T0/T3 | 每个 Host 只收到自己的 secret 和 graph edges。 |
| TS-P1-HOST-01 | Create/Update no-op、image/config/secret rotation 与 owned drift repair。追踪 TR-LC-UPDATE-*。 | T1/T2 | 仅目标 Host in-place reconcile，不 replacement、不波及其他 Host。 |
| TS-P1-HOST-02 | unowned 同名 container/route 与 malformed future state。追踪 TR-STATE-03。 | T2 | fail closed，不修复或删除 unowned object。 |
| TS-P1-SSH-01 | OpenSSH Include、Match、ProxyJump/ProxyCommand、agent 和 certificate。追踪 TR-SSH-07。 | T1 loopback | 由系统 OpenSSH 解释；Provider 不重写配置语义。 |
| TS-P1-SSH-02 | timeout/cancel 清理 local process group。追踪 TR-SSH-05、TR-OBS-05。 | T1 loopback | deadline 内返回，无遗留 ssh/ProxyCommand；结果按 unknown 合同处理。 |
| TS-P1-RUNTIME-01 | Docker PostgreSQL/Redis、MicroSocks、Tunnel connector 的 local projection。追踪 Host 深模块合同。 | T2 | 只触碰 owned runtime，preserve data，不调用云 API。 |
| TS-P1-READ-01 | timestamp、container ID、restart count 等瞬时变化。追踪 TR-HOST-06、TR-OBS-01。 | T0/T1 | 可更新诊断，但不制造 managed diff/reconcile。 |
| TS-P1-DELETE-01 | retire 完成但 response 丢失，Provider/Pulumi 重启后 retry。追踪 TR-REC-04、TR-LC-DELETE-02。 | T1/T2 | 返回原 result，stop/remove 至多一次，preserved inventory 不变。 |
| TS-P1-MIG-01 | Import 后 inputs 与 remote applied revision 有非危险差异。追踪 TR-LC-IMPORT-05。 | T1/T3 | Import 零 mutation，preview 明确显示 in-place diff，不声称已收敛。 |
| TS-P1-MIG-02 | 新 Host write 前回退 old writer。追踪 TR-ROLLBACK-01。 | T2/T3 | lock 内 observation 未变才允许回退，始终 single-writer。 |
| TS-P1-MIG-03 | 新 Host write 后尝试盲目恢复 old writer。追踪 TR-ROLLBACK-02。 | T2/T3 | 被阻断；必须先恢复 remote operation 到明确 observation。 |
| TS-P1-MIG-04 | cloud state move/import 中断。追踪 TR-ROLLBACK-03。 | T3 | 两侧 apply 停止，physical ID 对账后继续，无双 writer。 |
| TS-P1-MAINT-01 | `servers: []` stop-all、first Host、remaining Hosts、publication 任一 update 失败后恢复。追踪 TR-MAINT-*。 | T2/T3 | 每步 preview/checkpoint 可见；失败保持 public off，未轮到的 Host 启动调用为零，并可由下一次普通 update 继续。 |

## 8. Environment Program 测试方向

Program graph fixture 至少包含一个 Cloudflare 公开入口、一个 managed Neon、一个 managed Upstash、一个本地 data Host 和两个 App Hosts。

T3 mocks 必须记录：

- resource type token、logical name、parent 和 official provider reference。
- Host 数量、每 Host projected inputs 和 secrets。
- explicit/implicit dependencies 和 registration order trace。
- `protect`、retention、aliases/import 相关 resource options。
- known、computed/unknown、secret-known 与 secret-unknown outputs。
- App `servers: []` 时 definition/data-link continuity、原 Host stop projection 和 public access rejection。

Mocks 用于 graph 大多数场景。
仅 `protect` 删除阻断、Import/refresh/preview、state identity 和迁移 `0/0/0` 使用少量 local-backend CLI rehearsal。
所有场景使用 fake official Provider outputs，不连接真实云账号。

## 9. Lifecycle 纯度测试

Provider harness 为每个 lifecycle request 使用同一个 recording transport，并按调用类别统一断言：

| Lifecycle | 允许 | 禁止 |
| --- | --- | --- |
| Check | property decode/canonicalization | 全部 transport、filesystem、cloud calls |
| Diff | prior/input property comparison | 全部 SSH、inspect、approval mutation |
| Create | fixed install、identity inspect、reconcile、final inspect | arbitrary shell、unknown artifact、猜测 adoption |
| Read | inspect | install、reconcile、recover、retire、state migration |
| Update | inspect、approval check、begin/resume reconcile、final inspect | duplicate operation、implicit data replacement |
| Delete | inspect、approved preserve-data retire | volume/data/unowned deletion |
| Import | inspect、Pulumi state construction | install、ownership write、reconcile、runtime mutation |

unknown preview 必须额外证明所有 transport call 为零。
guard failure 除明确允许的只读 inspect 外，所有 runtime mutation 为零。

## 10. OpenSSH 与协议测试

Recording process seam 负责精确检查 executable、argv、stdin、stdout、stderr、exit、timeout 与 cancellation。
真实 OpenSSH loopback 只覆盖 fake 不能证明的系统行为：

- known host 成功，unknown/changed host key 失败且 known_hosts 不变。
- Include、Match、ProxyJump/ProxyCommand、agent 或 certificate 由系统 OpenSSH 解释。
- 目标 OpenSSH 对 `--` terminator 的支持，或所选等价固定 argv 合同不会把合法 alias 解释为 option/额外 destination。
- cancel/timeout 后本地 process group 无遗留。
- stdout framing 污染或截断被 Provider fail closed。

loopback 不扩展为 VPS 集群，也不测试云厂商网络可用性。

## 11. Host Runtime 与 fault injection

Host runtime fixture 使用临时 remote state/runtime tree、data sentinel、unowned sentinel 和 recording Docker/Compose/route/probe commands。
运行真实 `sub2api-host` 入口，并检查进程在 response 后退出。

fault injection 只跟随真实非幂等窗口增量增加：

| 增量 | 具名切点 | 主要 oracle |
| --- | --- | --- |
| A Provider/SSH | request 前失败、request 后 EOF、side effect 后 journal 前、journal 后 response 丢失 | 一个 operation，关键 action 至多一次。 |
| B blue/green | inactive start 后、probe 失败、route write 后、old stop 前 | 旧 route/runtime 保留，失败 runtime 不成为 active。 |
| C data-link | approval check 前后、render 前、restart 后 response 丢失 | 无批准零 mutation，批准不跨 operation 复用。 |
| D Delete/migration | detach 后 retire 前、retire 后 response 丢失、old writer preflight 后 mutation 前 | preserve inventory 与 single-writer。 |

只有新增不可约非幂等 action 或真实缺陷暴露新 unknown window 时，才增加新的具名 fault case。
不得从内部步骤自动生成全 phase 笛卡尔矩阵。

## 12. 迁移与 preserve rehearsal

测试使用脱敏 inventory，不保存 credential 或业务数据：

- Pulumi identity：old URN、expected target URN、type/name、完整 provider closure、physical provider ID、protect、retention、secret taint 和 alias/import 关系。
- Host identity：Environment/server/resource ID、machine identity、ownership 和 writer 状态。
- Runtime：Compose project、owned labels、paths、active image/route 摘要。
- Data：volume identity 或 bind path/inode 与仅测试 sentinel 摘要。
- Trace：old writer、new Provider、SSH 与 runtime mutator 的调用顺序。

rehearsal 必须证明：

- cloud resources 迁入 Environment Stack 保持 physical provider ID 与 resource continuity，目标 preview 为 `0 create, 0 delete, 0 replace`；跨 Stack 使用已审 URN 映射而非字面相等。
- Host Import 与 refresh 为零 runtime mutation。
- preview no-op 或只有明确接受的非危险 in-place diff。
- writer cutover 期间任何时刻只有一条 mutation trace。
- Delete 后 data identity/sentinel、unowned inventory 和 recovery evidence 不变。

inventory 是测试 evidence，不进入生产 Provider 的持久 state。

## 13. 现有测试迁移

现有测试按风险语义迁移，不按文件数量一一复制：

| 当前证据 | 目标用途 |
| --- | --- |
| Go config/projection tests | T0 strict config、references、ordering、secret isolation。 |
| `infra/resources_test.go` Pulumi mocks | T3 official Provider graph、Host count、dependencies、protect、unknown/secret。 |
| SSH check 与 CLI tests | T1 argv/stdin、host-key、redaction、process cleanup。 |
| blue/green 与 slot tests | T2 active runtime、route rollback 和跨 App 隔离。 |
| preflight/deployment mode tests | malformed state、data-link guard 和零副作用。 |
| legacy adoption tests | journal recovery、preserve、single-writer 和 migration rehearsal。 |
| render/Compose/layout tests | 实现 parity 前保留 runtime artifact、ownership 和 data path 风险证据。 |

执行顺序为 `新增公开合同测试 -> 同一风险双跑 -> 新实现通过 -> 删除旧实现细节断言`。
不维护永久 old-test selector manifest。

## 14. 测试数据与安全

- 每类 secret 使用不同 canary，失败信息和 snapshots 也参与扫描。
- fixtures 不含生产凭据、真实业务数据或可访问的云 resource IDs。
- machine、resource、data identities 使用稳定测试值，避免 wall-clock 参与语义。
- 一次性批准使用普通测试 evidence 和 process-scoped 消费状态，不引入 PKI 或 FixedClock 体系。
- 只有实现真正冻结 expiry 语义时才增加 clock boundary fixture。
- 真实 `ssh` loopback 的 key、config 和 known_hosts 全部临时生成并在测试后清理。

## 15. 执行与完成判定

实现阶段应使用仓库现有 Go、Vitest/TypeScript、Shell syntax 和 Compose validation 入口，并加入最少的 Pulumi local-backend 与 OpenSSH loopback rehearsal。
具体命令随实现目录确定，本一般规格不提前冻结 package paths 或 CI job names。

进入实现评审所需测试设计条件：

- 所有 P0 场景已有明确 seam、fixture、正向 oracle 和负向 oracle。
- Environment Program 测试直接覆盖 official Provider wiring、protect、unknown、secret 与 dependencies。
- Check/Diff 零 transport，Read/Import 只读，Read error 保留 ID。
- OpenSSH host-key fail closed、argv/stdin 和单帧协议有 fake 加 loopback 证据。
- unknown result 不产生第二个 operation 或第二次非幂等副作用。
- 首次 bootstrap、`servers: []` maintenance、blue/green、data-link、preserve-data 和 single-writer 场景有可执行方向。

后续宣称实现通过时必须满足：

- 所有 TS-P0 场景在当前 revision 无 skip 通过。
- migration、Delete、single-writer 相关 TS-P1 在生产切换前通过。
- cloud rehearsal 为 `0 create, 0 delete, 0 replace`，且不连接真实云账号。
- canary 扫描未发现 secret 泄漏。
- 现有关键风险测试在删除前已有新合同证据替代。
- 没有执行或要求真实公网endpoint、云服务可用性或最终用户路径冒烟测试。
- 本地没有执行构建命令；所有构建与发布产物证据来自CI。

“所有 P1 已实现”、真实云可用性、完整 Docker 平台矩阵、pairwise、mutation 100%、全 phase 矩阵、effect registry 或 selector 治理均不是第一轮实现评审门槛。

## 16. 需求追踪索引

| Tech area | 主要测试场景 |
| --- | --- |
| TR-PROG-*、TR-SEC-* | TS-P0-PROG-01..05、TS-P1-PROG-01..02、TS-P0-SEC-01 |
| TR-HOST-*、TR-LC-CHECK-*、TR-LC-DIFF-* | TS-P0-CHECK-01、TS-P0-DIFF-01..02、TS-P1-READ-01 |
| TR-LC-CREATE/READ/UPDATE/IMPORT-* | TS-P0-CREATE-01、TS-P0-READ-01..02、TS-P0-IMPORT-01、TS-P1-HOST-01 |
| TR-SSH-*、TR-PROTO-* | TS-P0-SSH-01..03、TS-P0-PROTO-01、TS-P1-SSH-01..02 |
| TR-REC-* | TS-P0-REC-01..02、TS-P1-DELETE-01 |
| TR-DATA-*、TR-MAINT-*、TR-ORDER-* | TS-P0-DATA-01..02、TS-P0-BG-01、TS-P0-ORDER-01、TS-P0-BOOTSTRAP-01、TS-P0-MAINT-01、TS-P1-MAINT-01 |
| TR-LC-DELETE-*、TR-RETIRE-* | TS-P0-DELETE-01..02、TS-P1-DELETE-01 |
| TR-MIG-*、TR-MIG-CLOUD-*、TR-ROLLBACK-* | TS-P0-MIG-01..02、TS-P1-MIG-01..04 |

该索引用于人工评审，不生成独立 requirements、effects、pairwise、legacy 或 selector manifest。

## 17. 测试 Open Questions

1. 目标 Host Provider framework 对 unknown/secret/private state 的测试 harness 能力是什么？
2. 哪个固定 Pulumi CLI/backend 组合用于 sanitized cross-Stack state move rehearsal？
3. 官方 Neon Provider 的 import schema 应使用哪些脱敏 fixture 才能证明无 replace？
4. OpenSSH loopback 是否需要覆盖一个真实 ProxyJump，还是 ProxyCommand process cleanup 已足以覆盖当前风险？
5. Host-local readiness 的最终接口确定后，哪些 observation 属于稳定 output，哪些仅属于诊断？
6. 生产 legacy writer 无共享 guard 时，测试如何用可审 trace 证明全部 mutation entrypoint 已禁用？

这些问题需要在对应实现 seam 确定后收敛，不得借此增加测试治理框架或冻结任意 phase、容量与保留策略。
