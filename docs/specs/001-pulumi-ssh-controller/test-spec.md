# 001 — Pulumi SSH Controller Test Spec

状态：Ready for implementation（Terra freeze gate passed）
日期：2026-08-09
对应技术规格：[tech-spec.md](tech-spec.md)
需求追踪：[requirements.yaml](requirements.yaml)
副作用清单：[side-effects.yaml](side-effects.yaml)
旧路径门禁：[legacy-bridge.yaml](legacy-bridge.yaml)
测试对象：Environment Host projection、Native Host Provider、System OpenSSH transport、`sub2api-host`、migration/retire controller

## 1. 测试结论

本规格只验证 Pulumi SSH Controller。Cloudflare、Neon、Upstash 等 serverless provider 的 CRUD、Import、Refresh、API schema、配额、区域、费用和服务可用性全部排除。它们在测试中表现为 opaque upstream resource/output；测试只关心这些输出是否进入正确 Host 的 desired projection、secret taint 是否保留、Host dependency edge 是否正确。

当前 Work Mode 没有 Go、Docker，且限制 `tsx` Unix socket，因此完整 Controller 测试运行在 GitHub-hosted Ubuntu runner。核心逻辑必须保持 hermetic：不需要 VPS、云账号或真实 Sub2API 服务即可验证 lifecycle、SSH 协议、journal 恢复、蓝绿行为、数据保护和迁移门禁。真实生产 Host 只执行 shadow/read-only 检查，不能代替自动化测试。

测试完成度按照“技术规格 MUST/禁止项 -> test ID -> test layer -> CI evidence”计算，不使用测试总数作为覆盖证明。当前仓库测试需要逐项登记为保留、被 Go contract 替代、或与目标无关；旧测试数量变化不会降低任何安全语义。

## 2. 范围

### 2.1 必测

- config decode、canonicalization、reference validation、dependency sorting 和 per-Host projection；
- Host resource Check、Diff、Create、Read、Update、Delete、Import；
- Pulumi unknown/secret/null property 处理与 state secret taint；
- 系统 OpenSSH argv、安全配置、stdin/stdout framing、timeout、cancel 和 unknown result；
- Host RPC version、schema、size、error taxonomy 和 redaction；
- operation ID 跨 Provider/Pulumi 进程重启的稳定恢复；
- Host Agent inspect/apply/recover/retire、writer lock、journal、atomic state；
- Compose/Traefik/App/data/proxy/firewall runtime plan 与失败回退；
- PostgreSQL/Redis connection identity 和一次性批准门禁；
- legacy state/adoption、ownership epoch、writer freeze、Import 只读；
- retire tombstone、preserve-data 和重新 Create/Import 保护；
- CLI 参数、SOPS/approval 临时文件、Pulumi argv、bundle 和 release compatibility。

### 2.2 明确排除

以下项目不进入 Controller 通过率，也不为它们建立 mock CRUD 测试：

- Cloudflare DNS、Tunnel、Load Balancer 的真实或模拟 CRUD；
- Neon Project/Branch/Endpoint/Database/Role 的真实或模拟 CRUD/import；
- Upstash Redis 的真实或模拟 CRUD/import；
- 云厂商网络故障、限流、配额、账单和区域可用性；
- serverless public endpoint 冒烟；
- PostgreSQL/Redis 业务数据复制和恢复正确性。

Environment test 可以注入 `OpaqueResourceOutput{id, endpoint, secret}`，验证 Host projection 和 `dependsOn`。该测试名称和报告必须标为 controller projection，不能标为 Cloudflare/Neon/Upstash integration。

## 3. 测试层级

| 层级 | 名称 | 运行方式 | 主要 oracle |
| --- | --- | --- | --- |
| L0 | Pure model | `go test` table/property/fuzz | canonical value、stable error、hash、零 side effect |
| L1 | Agent state machine | Go test + temp filesystem + recording runtime | phase、call trace、state/journal、sentinel |
| L2 | Binary contract | 启动真实三个二进制 | JSON frame、exit、signal、file mode、redaction |
| L3 | Provider harness | `pulumi-go-provider` integration harness | lifecycle response、diff、state、transport call count |
| L4 | Provider process restart | 两个独立 plugin/Pulumi process + persistent fake Agent | journal count、operation ID、external action count |
| L5 | OpenSSH loopback | 临时 sshd、临时 SSH config/keys、真实 `ssh` | resolved config、argv、stdin、process tree、known_hosts digest |
| L6 | Runtime contract | recording runtime + GitHub runner Docker/Compose fixture | blue/green trace、route、health、volume/data sentinel |
| L7 | Engine/migration rehearsal | 真实Pulumi CLI/local backend/native plugin + 多UID legacy/Agent进程 | protect/import、ownership epoch、zero mutation、preview/refresh state |

L0-L7 全部属于 Controller CI。没有任何一层要求 serverless provider credentials。

## 4. 测试性设计要求

Provider 和 Agent 状态机只依赖 tech spec 定义的 `HostTransport`、`ProxyRegistry`、`RuntimeInspector`、`RuntimeSteps`、`FileStore`、`CommandRunner`、`Prober`、`Clock` 和 Agent-owned `IDSource`。测试实现提供：

- `RecordingRuntime`：记录每个外部动作，支持在动作前、返回后、checkpoint 前注入失败；
- `FaultFileStore`：在 write/fsync/rename/directory fsync/chmod/chown 注入 ENOSPC、EIO 和中断；
- `PersistentFakeAgent`：跨 Provider process 保存 journal、tombstone 和 action counters；
- `FakeSSH`：捕获 argv、stdin、env、stdout/stderr/exit/signal；
- `FixedClock` 与 `MonotonicClock`：验证 expiry 边界和墙钟跳变；
- `CanarySecrets`：每个 secret 位置使用唯一可扫描值；
- `OpaqueUpstream`：只表达 Host dependency/output，不实现任何云 API。

测试不允许通过真实 sleep 等待状态；所有 timeout、drain、retry 使用 fake clock 或 bounded process deadline。所有临时目录、端口、Compose project 和 operation store 使用 test-specific 唯一值，支持并行与 `go test -race`。

## 5. 等价类分析方法

每个输入维度分为有效类、无效类、边界类和状态相关类。一个代表值只能覆盖同一控制路径和同一 oracle 的输入；只要 expected side effect、error taxonomy、secret scope、recovery path 或 state transition 不同，就属于不同等价类。

覆盖策略：

- 一维校验使用全等价类 + 边界值；
- Host projection 高维组合使用 all-pairs，并列出必须完整覆盖的三阶组合；
- Provider lifecycle 使用状态 × RPC × transport result 的显式矩阵；
- journal 每个 phase 自动生成相同 fault-point suite；
- 具有非幂等风险的动作执行全故障点覆盖，不依赖 pairwise；
- 所有禁止行为使用负向 call-count、文件 sentinel 和 secret scan 作为 oracle。

Tech spec外部contract limit使用测试侧独立golden literal，不导入生产常量。固定覆盖：config 4 MiB、servers 64、apps 128、per-Host apps 64、RPC request 8 MiB、response 1 MiB、stderr 256 KiB、journal 2 MiB、approval 64 KiB、evidence 4 MiB、bundle 512 MiB、operation directory 512 MiB；每项都测试limit-1、limit、limit+1。修改生产常量而未同步规格会让contract test失败。

All-pairs输入固定在[pairwise-model.yaml](pairwise-model.yaml)：factor、level、illegal-combination constraint、generator version和seed都进入版本控制。CI输出生成case、covered/uncovered pair清单，`uncovered_pairs`必须为0；第18节高阶组合继续单独执行，不由pairwise替代。

## 6. Config 与 per-Host projection 等价类

### 6.1 配置模式

| EC ID | 等价类 | 代表输入 | 预期 |
| --- | --- | --- | --- |
| CFG-EC-01 | 仅新配置且完整 | version + servers/apps/data | canonical environment |
| CFG-EC-02 | legacy 四对象完整 | edge/sites/edgeSecrets/siteSecrets | 只允许 legacy adapter/render |
| CFG-EC-03 | 新旧混用 | servers + sites | 注册资源前失败，零 SSH |
| CFG-EC-04 | legacy 缺任一对象 | sites 缺 siteSecrets | fail-closed |
| CFG-EC-05 | 空配置 | `{}` | 明确错误，不产生零事实源 Stack |
| CFG-EC-06 | YAML duplicate key | 两个 `servers:` | parse 失败 |
| CFG-EC-07 | YAML anchor/merge 改写关键字段 | merge 覆盖 server ID | 按冻结策略拒绝关键对象 merge |
| CFG-EC-08 | nil/empty map/list | `apps: {}`、`servers: []` | 使用明确业务规则，不与 absent 混淆 |

### 6.2 Server ID 与 SSH alias

合法 ID 规格为 DNS-label 风格、ASCII、长度 1..63、首尾字母数字、内部允许 `-`。测试边界：0、1、2、62、63、64 字符；首/尾 `-`；连续 `-`；大写、Unicode、空格、tab、CR/LF、`/`、`..`、`-oProxyCommand=...`、`--`、前导 option。

所有无效类必须在创建 `exec.Cmd` 前失败。合法 alias 在 argv 中恰好占一个元素，不能进入 shell command string、路径拼接或环境变量名。

### 6.3 Host 数量与角色

| EC ID | Host 角色 | 必查输出 |
| --- | --- | --- |
| PRJ-EC-01 | 单 Host、单 App leader | 完整 App desired + bootstrap secret |
| PRJ-EC-02 | 单 Host、已有 App | AUTO_SETUP=false，无 bootstrap secret |
| PRJ-EC-03 | 多 Host follower | 只含 follower desired，等待 leader readiness |
| PRJ-EC-04 | 同 Host 多 App，角色混合 | App 级 secret/proxy/route 隔离 |
| PRJ-EC-05 | 仅 Docker data Host | 不含 App/route/bootstrap secret |
| PRJ-EC-06 | 仅 proxy/connector Host | 不含其他 App/data secret |
| PRJ-EC-07 | 声明 Host 无 workload | 允许bootstrap-only，所有runtime mutation=0 |
| PRJ-EC-08 | 一个 App 三 Host且 leader 失败 | 两个 follower 的 transport 调用数均为零 |

数量边界使用测试侧独立literal覆盖0、1、2、servers 64/apps 128/per-Host apps 64和各上限+1，不能导入生产常量。

`PRJ-DESIRED-PURITY-*`对生成的HostDesired做schema/golden检查：role只允许由server list顺序得到leader/follower；不存在InstallationID、ObservedOwner、OwnershipEpoch、existing、NeedsSetup或AutoSetup字段。首次bootstrap、matching import、epoch提升与旧checkpoint重放使用同一config求值得到相同desired revision，Program SSH call=0；Provider runtime request再依据Inspect选择AUTO_SETUP。

### 6.4 引用与顺序

| EC ID | 输入 | 预期 |
| --- | --- | --- |
| PRJ-EC-09 | map key 重排 | canonical hash 不变 |
| PRJ-EC-10 | App server list 重排 | leader/更新顺序变化，hash 改变 |
| PRJ-EC-11 | server list 重复 | validation 失败 |
| PRJ-EC-12 | 引用未知 server/data/proxy | validation 失败且给出稳定 path |
| PRJ-EC-13 | A→B→C | 稳定拓扑顺序 |
| PRJ-EC-14 | A→B→A | validation 输出稳定 cycle path |
| PRJ-EC-15 | 两个独立 Host | 排序稳定，无多余依赖 |
| PRJ-EC-16 | shared data Host | 两个 App Host 都依赖 data Host |

删除 Host 时，`apps.servers`、`publicAccess.servers`、`outboundProxy.servers`、`postgres.server`、`redis.server` 五种单独引用分别建立 P0 用例，任一存在都在 Pulumi update 开始前阻断。

### 6.5 Secret scope

对 remote/control/bootstrap/app/data/proxy secret 分别覆盖 absent、单 Host、双 Host、同 Host 双 App、两 Host 同 App。断言：

- `RemoteSecrets` 只进入目标 Host request；
- `ControlSecrets` 从不进入 Agent request；
- `initialAdminPassword` 只进入首次 leader；
- follower、新增副本、重试已有 App均不收到 bootstrap secret；
- App A 的 secret 不出现在 App B rendered plan、hash debug 或 evidence；
- Pulumi unknown secret 在 Check/Diff 保持 unknown 和 secret taint。

### 6.6 Canonical hash 与 connection identity

| 维度 | 相同等价类 | 必须不同的等价类 |
| --- | --- | --- |
| map/order | map 重排、YAML CRLF/LF | leader list 顺序变化 |
| endpoint | 默认端口显式/隐式按规格归一 | host、port、IPv4/IPv6 地址变化 |
| PostgreSQL | 等价 percent-encoding | database、TLS server identity变化 |
| Redis | `DB 0` 的显式/默认规则 | DB number、TLS identity变化 |
| secret | 密码/token rotation | 非敏感连接 identity变化 |
| nil/empty | 规格声明等价时相同 | absent 与 explicit empty 语义不同处必须不同 |

序列化使用 length-delimited canonical encoding，测试构造字段拼接碰撞，例如 `ab|c` 与 `a|bc`，两者 ID 必须不同。普通 SHA 不得包含低熵 secret。

## 7. Provider lifecycle 等价类与状态矩阵

### 7.1 Pulumi property classes

Check/Diff 对每个字段覆盖 known、unknown、secret-known、secret-unknown、null、absent、type mismatch。Preview 中的 unknown 不触发 SSH，不被当作空字符串校验，也不丢 secret bit；只有引擎要求最终 known 的 Create/Update 才返回明确 failure。

### 7.2 Diff classes

| Case ID | 输入变化 | 预期 |
| --- | --- | --- |
| PVD-DIFF-001 | canonical no-op | changes=false，transport=0 |
| PVD-DIFF-002 | desired change | in-place update，replaces=[] |
| PVD-DIFF-003 | bundle SHA change | in-place update |
| PVD-DIFF-004 | remote secret rotation | in-place update，connectionId 不变 |
| PVD-DIFF-004A | control secret rotation | in-place update，Agent Apply=0，只允许ProxyRegistry read/upsert |
| PVD-DIFF-005 | managed observed drift | in-place repair update |
| PVD-DIFF-006 | transient health only | 只更新 health observation，不触发 repair diff |
| PVD-DIFF-007 | server ID/alias/environment change | Check failure，不能 replacement |
| PVD-DIFF-008 | timestamp/container ID/latency变化 | observed hash 不变 |
| PVD-DIFF-009 | unknown desired property | 合法 unknown diff，transport=0 |
| PVD-DIFF-010 | registry desired != applied desired | in-place registry upsert，Agent Apply=0 |
| PVD-DIFF-011 | registry observed != committed observed | RegistryDrifted=true，in-place repair |
| PVD-DIFF-012 | registry disabled sentinel | no-op，Registry call=0 |

所有 Check/Diff case 都断言 HostTransport 方法调用总数为零。

### 7.3 Create classes

| 远端状态 | 无 adoption intent | 有匹配 intent | 预期 |
| --- | --- | --- | --- |
| empty/bootstrap-only | allow | allow | apply |
| matching managed + desired complete | idempotent | idempotent | 不重复副作用 |
| matching managed + drift | fail | 仅显式adoption intent允许 | 普通Create永远拒绝已有runtime |
| verified legacy | fail | allow | adoption apply |
| different Host/environment owner | fail | fail | identity conflict |
| retire tombstone exists | fail | 仅显式 re-adopt | 不能当空 Host |
| corrupt/unknown state | fail | fail | recovery-required |
| unreachable/agent missing | fail | fail | 保留诊断，不猜 empty |

### 7.4 Read classes

| Case ID |  observation | Pulumi ID | 预期 |
| --- | --- | --- | --- |
| PVD-READ-001 | healthy/no drift | 保留 | outputs 更新 |
| PVD-READ-002 | managed config drift | 保留 | Drifted=true |
| PVD-READ-003 | health degraded only | 保留 | health 更新，Drifted=false |
| PVD-READ-004 | operation running | 保留 | pending diagnostic |
| PVD-READ-005 | recovery-required | 保留旧checkpoint | Read返回结构化error，零mutation，不提交半更新state |
| PVD-READ-006 | SSH unreachable/timeout/255 | 保留 | transport error，绝不 NotFound |
| PVD-READ-007 | agent missing | 保留 | recovery-required |
| PVD-READ-008 | marker/state 手工删除 | 保留 | corruption/missing-owned-state |
| PVD-READ-009 | identity mismatch | 保留 | conflict |
| PVD-READ-010 | malformed/future protocol | 保留 | incompatible |
| PVD-READ-011 | Provider-owned completed retire tombstone | 可删除 | confirmed absent |

Read 的所有 case 断言 Apply/Retire/Recover call count 为零。Read 不获取 writer lock执行修复。

Registry Read具体类：disabled使用四字段sentinel且Registry call=0；enabled matching为ready/no-op；desired enabled但never-applied absent由desired/applied diff触发repair且RegistryDrifted=false；曾applied后absent或stale使observed/committed不同且RegistryDrifted=true；unowned collision保留ID并fail；unreachable保留ID并error。每类覆盖no-op、input diff、observed drift、Import和unknown control property。

`PVD-REGISTRY-DISABLE-*`覆盖enabled→disabled：ReadOwned matching/unowned/unreachable，Delete success/unknown/response loss/Provider restart。只有matching owned record可删；read-after-unknown确认absent后才提交四字段disabled sentinel。Agent Apply=0、retire approval=0、delete action至多一次；随后refresh Registry call=0。

### 7.5 Update classes

覆盖 desired no-op、image、bundle、remote secret、route config、proxy config、managed drift、connection change、pending same operation、pending different operation、failed rollback-complete、recovery-required、initial inspect timeout、apply unknown、final inspect timeout、context cancel。

每个 case 必须给出：operation ID、transport call sequence、Agent action count、最终 state、journal 状态、是否允许 retry。任何 write-bearing SSH 已启动后出现 EOF/255/timeout/cancel，都进入 unknown-result recovery，不能生成新 ID。

### 7.6 Delete classes

批准维度覆盖 absent、wrong environment、wrong Host、wrong desired fingerprint、过期前 1ns、恰好 expiry、过期后 1ns、已消费、并发消费、有效。前九类在全局 plan gate 处终止，所有 provider/SSH 调用数为零；有效类进入 publication-ready precondition 和 `retire --preserve-data`。

Delete 还覆盖远端 retire 完成但响应丢失、Provider process 崩溃、重试 destroy、retire tombstone 已存在。所有路径的 stop/remove action 至多一次，preserved asset identity 不变。

CLI全局gate和Provider纵深gate分别取证。L3直接调用Provider Delete，逐项传入缺失、wrong environment、wrong Host、wrong fingerprint、expired和consumed approval，断言Retire=0并返回稳定failure；有效approval才允许preserve-data retire。L7再使用真实Pulumi local backend绕过CLI执行raw destroy、普通config删除和未unprotect计划，断言Engine checkpoint、opaque upstream delete和Host Delete调用全部为零。

### 7.7 Import classes

匹配 Host ID/owner/protocol 允许 import；environment/server/owner 不匹配、state corrupt、pending operation、tombstone、unreachable 全部失败。Import 永远断言 Apply/Retire/Recover=0。

Import无法读取remote/control secret原文。测试要求Program提供的secret inputs保持Pulumi secret taint，Read只返回`SecretRevision`。matching import完成后refresh + preview no-op；program desired与远端AppliedDesiredRevision不同时，Import固定`Drifted=false`且`ObservedRevision==CommittedObservedRevision`，后续preview只因`DesiredRevision != AppliedDesiredRevision`显示in-place update。无法验证AppliedDesiredRevision的fixture必须Import失败并要求adoption baseline。所有类保持零mutation、resource ID语义明确、stack export无明文。bootstrap/enrollment是独立显式mutation，不能混入Import test。

ProxyRegistry import另分三类：owned record与desired matching则no-op；owned absent/stale时Import成功且后续只因`RegistryDesiredRevision != AppliedRegistryDesiredRevision`显示repair；unowned collision直接Import失败。三类都执行真实Engine export/refresh/preview并断言Registry write=0。

L7必须使用真实Environment Program、native plugin、Pulumi CLI和本地passphrase backend执行program-first import、refresh、stack export和preview；Provider harness结果不足以证明Engine会保留nested secret inputs。整个流程只允许Inspect transport call。

## 8. Operation identity 跨进程恢复

这是 P0 强制套件。`PersistentFakeAgent` 位于两个 Provider process 之外，保存 journal、current transition、tombstone 和外部动作计数。

### PVD-RESTART-001：Update complete 后回包丢失

1. Pulumi 用旧 checkpoint 调用 Provider process A；
2. A 发送写请求，Agent 在side effect前原子创建current transition和operation ID，随后完成route/commit并fsync complete；
3. transport在任何response byte前EOF；另一变体只返回半帧；
4. kill Provider process A 和 Pulumi process；
5. 从同一old checkpoint/new inputs启动process B，B不持有operation ID或Agent fingerprint；
6. B重发相同intent body，Agent begin-or-resume必须命中同一fingerprint和operation ID并收敛成功。

Oracle：journal只有一个；Apply action count=1；route switch=1；operation ID相同；最终state的AppliedDesiredRevision与CommittedObservedRevision正确。

同一套件复制到 Create、data-link Update、proxy registration 和 Delete/retire。对于副作用尚未完成的 checkpoint，process B 只能恢复同一 transition。

### PVD-RESTART-002：secret rotation

Provider A/B使用相同old state和secret input；Agent对同一approval subject得到相同BaseIntentFingerprint。A绑定approvalId后删除proof，B先inspect current transition取得operation ID与已绑定approvalId，再recover；BoundOperationFingerprint匹配且不能开始successor。不同nonce同subject具有相同base但不同bound fingerprint。日志、journal和diagnostics不得出现raw nonce/signature/secret。

### PVD-RESTART-003：不同 intent

process B使用不同desired或secret且旧transition仍非终态时，Agent必须拒绝并保持journal count=1，不创建或持久化第二operation ID。用户先处理旧operation，不能覆盖current transition。

## 9. System OpenSSH 等价类

### 9.1 最终配置安全

测试使用真实 `ssh -G` fixture覆盖：StrictHostKeyChecking=yes/ask/no/accept-new、UpdateHostKeys、PermitLocalCommand、LocalCommand、SendEnv/SetEnv、ControlMaster/ControlPersist、IdentityFile、ProxyJump、Match、Include、hashed known_hosts 和 host certificate。

策略不是二选一：`StrictHostKeyChecking=no/accept-new`、任意`SetEnv`和`LocalCommand`必须拒绝；yes/ask、IdentityFile和ProxyJump允许。执行argv必须强制`StrictHostKeyChecking=yes`、`UpdateHostKeys=no`、`CheckHostIP=no`、`PermitLocalCommand=no`、`ClearAllForwardings=yes`、`ForwardAgent=no`、`ForwardX11=no`、`Tunnel=no`、`ControlMaster=no/ControlPath=none/ControlPersist=no`和`SendEnv=-*`。测试直接调用Provider transport而不先经过CLI validate，证明不能绕过。

`SSH_AUTH_SOCK`覆盖absent、合法Unix socket、非socket、owner不符、group/other writable、dangling；只有合法类进入子进程。active/stale master、changed key和运行中config替换不能绕过最终argv。SSH子进程不继承Pulumi passphrase、SOPS key、cloud token、Admin API key或其他secret；用户config、known_hosts和private key前后摘要保持不变。Match exec与ProxyCommand/ProxyJump作为操作者可信代码边界单独记录，不宣称sandbox。

### 9.2 连接与 agent classes

| Case ID | 条件 | 预期分类 |
| --- | --- | --- |
| SSH-CONN-001 | `ssh -G` 和连接成功 | 进入 RPC |
| SSH-CONN-002 | alias 不存在/解析失败 | preflight failure |
| SSH-CONN-003 | unknown host key | host-key failure，无 apply |
| SSH-CONN-004 | changed host key | host-key failure，无 apply |
| SSH-CONN-005 | auth failure | unreachable/auth |
| SSH-CONN-006 | ProxyJump/DNS hang | overall deadline，process group清理 |
| SSH-CONN-007 | 连接成功但 agent 不存在 | agent-missing |
| SSH-CONN-008 | stale ControlMaster socket | 重新诊断，不绕过身份检查 |
| SSH-CONN-009 | `sudo -n`拒绝 | `sudo-denied`，Read保留ID，Update零side effect |
| SSH-CONN-010 | installation UID/GID不符 | `identity-permission-mismatch` |
| SSH-CONN-011 | docker socket拒绝 | `docker-permission-denied` |
| SSH-CONN-012 | root helper/sudoers拒绝 | `root-helper-denied` |

真实 OpenSSH test验证本地option terminator位于destination前：`ssh ... -- <alias> <fixed-remote-command>`。fake与真实loopback都断言相同argv contract。

### 9.3 Frame 与 exit classes

stdout 覆盖：一帧+EOF、一帧无换行、CRLF、空、两帧、前置 banner、尾随文本、截断 JSON、duplicate key、unknown field、invalid UTF-8、响应上限-1/上限/上限+1。stderr 覆盖空、普通诊断、secret canary、上限+1。exit 覆盖 0、业务非零、255、remote signal、本地 cancel。

组合 oracle：exit 0 + malformed JSON 是 protocol error；exit nonzero + succeeded JSON 仍按失败/unknown taxonomy处理；任何 write-bearing request 已开始后 exit 255 都按 unknown result查询同一 operation。

### 9.4 传输切点

注入 request 发送 0 byte、半帧、完整帧；Agent 读取后未执行、执行中、已 commit；response 发送 0 byte、半帧、完整帧。每种 write case都断言不会生成新 operation。Context cancel/timeout 后扫描 process tree，不能残留 ssh/ProxyCommand 子进程；远端 operation 仍通过 journal观察，不将“本地进程已结束”当成远端未执行。

## 10. Host RPC 等价类

| 维度 | 有效类 | 无效/边界类 |
| --- | --- | --- |
| protocolVersion | 1 | 0、负数、future、absent、string |
| kind | version/inspect/stage-bundle/apply/recover/retire | unknown、大小写变体、空 |
| operation ID | 首次begin为空；恢复为`op_<26-char lowercase base32>`随机128-bit ID | 非begin空值、长度错、路径字符、Unicode、大写、另一Host ID |
| host ID | matching canonical | environment/server mismatch、路径逃逸 |
| Agent fingerprint | 首次begin absent；response/recover中matching opaque HMAC | Provider伪造、不匹配、长度错；Agent始终重算 |
| JSON | 单值+EOF、UTF-8、无 duplicate | duplicate key、unknown field、trailing value、超限 |
| status/error | schema 对应组合 | succeeded+error、failed 无 code、running+complete phase |

RPC decoder 使用显式 duplicate-key detection；标准 `encoding/json` 单独不足以拒绝重复 key。错误响应只能回显安全 field path 和稳定 code，不能包含原 request/frame。

Compatibility固定验证Provider/Agent `minProtocol=1,maxProtocol=1`、bundle format v1：1↔1允许，任一端只支持0或2、范围不相交、bundle format 0/2都在mutation前失败。unknown/duplicate v1 field拒绝；Version negotiation只读。升级测试证明旧进程完成当前operation、symlink仅影响下一RPC，无handoff或downgrade。

## 11. Journal 状态、故障点与文件系统

### 11.1 合法状态迁移

正常 phase 只能按 tech spec 单调前进。允许的 terminal 为 complete、failed-with-rollback-complete、recovery-required。禁止跳 phase、倒退、complete 后修改 fingerprint或重开相同 ID。

每个 phase自动生成以下 fault points：

| Fault ID | 注入位置 |
| --- | --- |
| JRN-F01 | phase intent 写入前 |
| JRN-F02 | intent 已 fsync，外部动作前 |
| JRN-F03 | 外部动作已完成，verify observation 前 |
| JRN-F04 | observation 已确认，phase complete 写入前 |
| JRN-F05 | phase complete 已 fsync，下一 phase 前 |
| JRN-F06 | commit/complete 已 fsync，response 前 |
| JRN-F07 | recover observation 与 checkpoint 冲突 |

Apply全部12个phase与Retire全部7个phase分别乘F01..F07形成自动生成套件。没有外部动作的phase仍验证文件恢复；包含外部副作用的phase必须额外断言动作计数。Provider侧ProxyRegistry effect执行独立同构fault matrix。

### 11.2 非幂等子动作

Agent的`reconcile-app`和`reconcile-proxy-local`内部细分为可恢复step：intent -> observe -> act -> verify -> complete。管理员bootstrap/隐式schema migration、route switch、old slot stop、ownership marker切换和retire tombstone提交执行完整fault matrix。控制机`ProxyRegistry`的record create/update/delete使用固定名称幂等upsert和read-after-unknown，由Provider harness单独执行相同unknown-result矩阵，不写入远端Agent journal。

恢复总是 observe-before-act。相同 operation ID只能证明同一 intent，不能直接证明外部动作未发生。

[side-effects.yaml](side-effects.yaml)是可执行副作用清单。CI从生产`RuntimeSteps`/Provider adapters收集effect ID，要求每个production effect恰好映射一个manifest entry，每个entry至少有observe-before-act、verify、recovery和fault test；缺项或孤儿entry都失败。

每个leaf substep必须显式声明phase和非空test selector；phase只能属于Apply 12 phase、Retire 7 phase、bootstrap phases或§9.6 control transaction state。root selector不自动算作child coverage。lease acquire/release、unprotect/reprotect等相隔phase的动作使用各自leaf/effect，CI输出effect→phase→resolved test的双向表。

### 11.3 文件系统 classes

对 journal/state/tombstone/Host master key及domain derivation覆盖：

- write short/ENOSPC/EIO；
- file fsync失败；
- rename失败；
- directory fsync失败；
- chmod/chown失败；
- 残留 temp file；
- truncated/oversized/duplicate-key JSON；
- symlink/hardlink/path traversal；
- operation directory权限不符；
- journal quota达到边界；
- master key missing/rotated/permission错误或domain label错误。

未知 durability结果进入 recovery-required。非终态 journal永不被 GC。目录打开使用 no-follow/containment语义，operation ID不能直接作为未经验证的路径片段。

GC边界使用fixed clock覆盖terminal age 89d、90d、90d+1ns与terminal count 127、128、129；只允许同时满足“超过90天且不在最近128个”时清理。retire关联operation与所有非终态operation永久不GC。清理前必须把摘要原子写入容量512的transition index；index 511/512/513、write/fsync/rename失败、corrupt entry、index miss和已GC complete journal的延迟重放分别测试。淘汰最旧非retire摘要后，超晚retry必须recovery-required，不能创建新operation。

GC与inspect/begin/resume并发时使用同一GC/writer coordination，reader只能看到清理前或清理后+完整index的snapshot。多轮retire/retry生成永久evidence逼近operations目录512MiB上限时，Controller在新operation前返回quota recovery-required，绝不GC retire/nonterminal evidence或绕过容量门禁。

连续transition套件覆盖A→B→C、running same/different、complete same/different、failed+rollback same/different、explicit retry successor、complete journal已GC后successor，以及pointer/index transaction各fault point。每个case断言predecessor chain、current pointer、journal count与side-effect trace唯一。

### 11.4 并发 classes

| Case ID | 并发输入 | 预期 |
| --- | --- | --- |
| JRN-CON-001 | 相同 ID/相同 fingerprint 两 writer | 一个执行，一个读取同结果 |
| JRN-CON-002 | 相同 ID/不同 fingerprint | 一个执行，另一个拒绝 |
| JRN-CON-003 | 不同 ID 同 Host | 只有 current transition 可写 |
| JRN-CON-004 | inspect 与 apply | inspect 只看到完整 checkpoint |
| JRN-CON-005 | refresh 与 recover | refresh 只读，不修复 |
| JRN-CON-006 | cancel 后立即第二次 up | 第二次绑定原 transition |
| JRN-CON-007 | 两个 control Stack/alias 指向同 Host | ownership epoch 不匹配者零 side effect |
| JRN-CON-008 | old writer 已过 preflight，新 writer 接管 | common mutation lock/epoch保证单 writer |

锁相关测试使用 `go test -race` 并重复至少 100 次；无锁但存在非终态 journal时，新 operation 仍必须拒绝或恢复原 transition。

## 12. Runtime reconcile 等价类

### 12.1 Inspect

覆盖 empty、bootstrap-only、正常单 App、双 App、local data、external data、legacy flat layout、pending adoption、owned drift、unowned collision、state missing、state corrupt、route missing、container missing、master/domain key异常。Inspect 必须只读，测试对 filesystem tree、Docker action log 和 route digest做前后比较。

Observed hash只包含稳定 owned projection。时间戳、container ID、健康延迟、restart count和瞬时 probe结果不参与 hash；owned route/config/image/label/connection identity变化必须改变 hash。无 ownership label 的同名对象返回 collision，不被自动删除或接管。

### 12.2 Blue/green

| 当前状态 | 目标 | 预期 |
| --- | --- | --- |
| 无 slot | image A | bootstrap blue |
| blue A active | image B | start green B -> probe -> route -> drain blue |
| green B active | image C | start blue C -> probe -> route -> drain green |
| inactive残留目标 image且 healthy | 同目标 | observe/reuse，不重复创建 |
| inactive残留错误 image | 新目标 | 受管清理后重建，不碰 active |
| previous route存在 | health fail | 恢复 previous route |
| 首次 route不存在 | health fail | 移除新 route，保持未发布 |
| unowned同名 container/route | 任意 | collision failure，零清理 |

每个外部动作 start、internal probe、route stage、route commit、direct probe、drain、stop old、state commit 前后注入失败。Oracle 包括 active route、slot action count、state、journal 和 data sentinel。

现有runtime parity另覆盖：更新前停止仅属于本App的inactive slot；从active slot复制非敏感`config.yaml`和`.installed`marker；secret文件不复制而是重建。inactive stop、marker staging/verify/atomic install每一步执行fault matrix；失败时active slot marker/inode/hash和route保持不变，不重复AUTO_SETUP或schema migration。

### 12.3 Data runtime

Docker PostgreSQL/Redis 各覆盖 absent/create、healthy no-op、stopped restart、owned config drift、unowned collision、local volume present/missing、shared network、retire。external connection只参与 render/identity，不触发 cloud API。

Controller 不测试业务数据搬运；测试证明 link 变化门禁和 volume保留。local data Host 与 App Host分离、PG/Redis分居、同 Host共享多个 App 都进入 pairwise组合。

### 12.4 Proxy/firewall

覆盖 disabled、optional、required；本地 MicroSocks start失败、probe失败、registration失败、registration成功但response丢失、existing owned record、unowned/manual record、credential rotation、firewall owned table和unowned table。只允许管理固定名称前缀和明确 ownership metadata。

Traefik edge runtime与Tunnel connector分别覆盖absent/create、matching no-op、image/config/route change、stopped、health fail、owned drift和unowned collision。connector只测试本机container/secret/route projection，不调用Cloudflare provider API或public endpoint。

required proxy失败时 Host 不到 ready-for-publication；optional策略按 config contract输出 degraded或继续。该套件不调用任何 Cloudflare provider。

## 13. Approval 等价类

Ed25519 approval field分别覆盖正确、错误、缺失：issuer key ID、signature、environment、Host、App/resource、old/new identity、action、128-bit/32-hex nonce、issuedAt、expiresAt。approvalId的versioned length-delimited encoding使用golden和拼接碰撞反例验证。时间边界覆盖`issuedAt-skew-1ns`、`issuedAt-skew`、expiry-1ns、expiry、expiry+1ns；skew固定30秒，clock倒退30秒及30秒+1ns分别验证allow/fail-closed。

批准文件测试：0600、owner正确、regular file、no-follow、64KiB边界、父CLI保留到Pulumi退出、正常/异常退出清理和symlink拒绝。普通data-link由Agent在writer lock内绑定approvalId；retire另要求control ledger在unprotect/publication前先绑定相同approvalId，Agent稍后核对并绑定同一ID。Provider读取、transport 0/半帧、验签前失败均不消费。同subject两个proof并发时只有一个新operation/retire transaction绑定；已绑定operation在proof删除和expiry后仍可inspect-by-base再recover，successor/另一Host/另一intent复用相同approvalId拒绝。

## 14. Secret 与隐私测试

Canary corpus为以下位置生成不同 secret：Pulumi passphrase、Host master key及三个派生domain key、SSH相关环境变量、remote App secret、DB password、Redis token、MicroSocks credential、Admin API key、bootstrap password、approval内容。

测试结束扫描：

```text
Pulumi checkpoint/export fixture
Provider logs and errors
CLI stdout/stderr
SSH argv/env/stdout/stderr
Agent journal/state/tombstone
operation evidence
generated non-secret config
每个Agent/root-helper/prober/Compose CommandRunner argv/env/error
Docker inspect Config.Env allowlist surface
test failure diagnostics and golden files
```

明文只允许出现在Pulumi secret加密payload、远端0600 secret文件和明确登记的运行中受管container `Config.Env`；container删除后该surface必须消失。所有`CommandRunner`逐次断言secret不在argv、通用env、process error或raw stderr，Compose argv只带env-file路径，root helper/prober只从stdin/受限文件读取。SSH子进程env使用显式白名单；协议污染错误不回显原frame。master key丢失、rotation、domain derivation或权限错误返回recovery-required，不能自动覆盖remote secret。

关键guard逐项做独立mutation：分别禁用redaction、SSH最小环境、Agent最小环境、nested secret taint、protocol error不回显和Docker inspect过滤；每个mutation都必须至少被一项测试杀死，critical mutation score固定100%。

## 15. Writer ownership 与迁移

Host 保存稳定 owner identity和递增 ownership epoch；common mutation lock由旧脚本和新 Agent共同遵守。旧程序每次 mutation 前检查 takeover marker/epoch，不能只在最初 preflight检查一次。

迁移等价类：

| Case ID | old writer | new owner | 预期 |
| --- | --- | --- | --- |
| MIG-OWN-001 | idle/frozen | matching | import允许 |
| MIG-OWN-002 | still writable | matching | new update阻断 |
| MIG-OWN-003 | 已过 preflight后阻塞 | takeover开始 | 等待/拒绝，不能交叉 side effect |
| MIG-OWN-004 | 两个 Environment/Stack | 同 Host | epoch不匹配者失败 |
| MIG-OWN-005 | 两个 alias | 同 Host identity | 只允许 owner alias |
| MIG-OWN-006 | ledger/state不一致 | 任意 | fail-closed |

Import rehearsal 前后断言 old/new action log、runtime tree和route digest不变。legacy fixture逐个映射当前 host-state、deploy-state、slot、mode、route、adoption journal；unknown field、malformed或unsafe path进入 recovery-required。

L7不是fake writer单测：GitHub Ubuntu runner创建真实`legacy-deploy`与`sub2api-deploy`两个UID/group，按[legacy-bridge.yaml](legacy-bridge.yaml)运行全部真实entrypoint caller graph和Agent binary，共同争抢同一lock inode；Docker命令由recording shim替代。覆盖nested继承FD/token不自锁、直接内部调用自行锁、旧脚本已过preflight、bridge未安装/attestation错、epoch提升、lock inode替换、parent/child kill和两个alias/Stack。断言mode/owner/sudo矩阵、每个effect前epoch重读、FD持有范围和side-effect trace唯一。

## 16. Retire 与 preserve-data

Retire 前建立preserved asset inventory：volume driver/name/ID/options、bind path/inode、PG/Redis/data sentinel、old release、operation evidence、shared network、installation/ownership/master key/tombstone。成功后逐项比较；运行时dotenv/credential和受管container `Config.Env`必须消失，ControlSecrets从未远端落盘。

必测组合：单 App、双 App、local PG、local Redis、PG+Redis、仅 external data、shared network、symlink邻近路径、unowned container/route/nft table、retire重试、response丢失、Provider restart。

静态扫描和 recording runtime共同禁止：

```text
docker compose down -v
docker volume rm
recursive remove of data root
cloud data delete
business table mutation
cleanup of unowned object
```

retire 成功后保留不可变 tombstone，包含 Host identity、owner epoch、preserved inventory hash、approval fingerprint和last operation。Create 将 tombstone视为 retired-existing，不视为空 Host；Topic 001没有re-adopt/rekey命令，相关请求固定fail-closed。

无 retire approval 的 destroy 必须在全局 update gate 前失败，opaque upstream resource和SSH调用数都为零，避免 Pulumi 先删除 publication依赖后才在 Host.Delete报错。

另有真实Pulumi CLI + local passphrase backend + native/recording provider fixture绕过CLI，分别执行raw destroy、普通config删除和手工未unprotect update；断言Host与所有先行publication resource的protect让Engine在provider delete调用前阻断，checkpoint不变、opaque upstream delete和SSH计数均为零。

获批路径使用真实Engine验证完整control事务：唯一config已删除server；从旧checkpoint恢复identity；lease/checkpoint export；在任何unprotect前durably bind control approval；unprotect后从新checkpoint保存plan；按`URN + delete|update + changed paths`过滤；`up --plan`detach publication、DeleteOwned record、Agent retire；最后reprotect/read-back。shared publication只允许去除目标Host binding的update，不验证真实cloud schema/API。

在lease、export、control bind、unprotect、plan save、publication partial checkpoint、registry delete、Agent bind/retire和reprotect前后SIGKILL CLI。新进程首先恢复ledger/reprotect；checkpoint未变才可继续原plan，已改变则refresh并生成带predecessor的新受限successor plan。backend version外部变化立即fail-closed。每个状态断言approval只被该control transaction和同一Agent operation使用、错误URN/额外op/replace/data delete/property path/plan digest均零mutation。

## 17. CLI 与 bundle

CLI 等价类覆盖 env存在/缺失、config valid/invalid、SSH preflight success/failure、Pulumi executable missing、Provider plugin missing/wrong version、pending journal、writer conflict、approval valid/invalid、child process cancel和exit code传播。

每条 wrapper测试断言实际 Pulumi argv、cwd、最小 env、`--parallel 1`、`--refresh`显式性和脱敏输出。CLI 不隐藏底层非零退出，也不把 post-update verification失败报告为部署成功。

Bundle覆盖amd64/arm64、unknown arch、checksum match/mismatch、相同版本no-op、content-addressed inbox partial/resume、同digest不同内容、symlink target、512MiB边界、atomic install中断和protocol/bundle format不兼容。真实多UID loopback令admin primary group不同、supplementary加入`sub2api-deploy`，验证dropbox root:group 2730、本地artifact 0640、strict `scp -p`后partial owner=admin/group=sub2api-deploy/mode0640，再调用stage-bundle验证/rename。StageBundle可重复且runtime mutation=0。初始bootstrap另覆盖Agent不存在、0700 remote temp、32-hex grammar、self/parent owner-mode-SHA、strict scp/SSH argv、sudo拒绝、每个bootstrap leaf SIGKILL与重连恢复。

## 18. 强制高阶组合

除 all-pairs 外，以下组合逐个建立具名用例：

1. `PRJ-COMB-001`：首次 leader × local Docker PG+Redis × required proxy × bootstrap secret；
2. `PRJ-COMB-002`：首次 follower × remote data × required proxy：无 bootstrap secret；
3. `PRJ-COMB-003`：已有 App新增 follower × secret rotation：不重新 bootstrap；
4. `PRJ-COMB-004`：同一 Host双 App × shared data × 不同 proxy/secret：严格隔离；
5. `PRJ-COMB-005`：一个 App三 Host × leader failure：两个 follower零调用；
6. `PRJ-COMB-006`：两 App形成 Host cycle × shared data：稳定 cycle错误；
7. `PVD-COMB-007`：image update × SSH complete-response EOF × Provider restart：单次 route切换；
8. `APR-COMB-008`：data-link approval consumed × Agent kill × CLI restart：原 operation继续，新 operation拒绝；
9. `RET-COMB-009`：retire × local PG+Redis × response EOF × destroy重试：tombstone证明成功，数据不变；
10. `MIG-COMB-010`：old writer已过 preflight × new takeover × second control Stack：任何时刻一条 side-effect trace；
11. `SEC-COMB-011`：secret rotation × stderr canary × malformed stdout：无泄漏、同 operation恢复；
12. `PVD-COMB-012`：refresh health degraded × owned config no drift × pending old journal absent：只更新 health，不触发写。

## 19. 不可发生行为断言

每次 Controller integration test共享以下全局 assertions：

1. Check、Diff、Read、Import、preview 不调用 mutating Agent command；
2. EOF、255、timeout、cancel和 Provider restart后不生成第二个 operation；
3. 任意失败路径不执行 volume/data/cloud data/business table删除；
4. identity、owner epoch或ownership label不匹配时不接管、不清理；
5. raw request/response/stderr/env/DSN不进入诊断、journal和evidence；
6. 普通 input diff不触发 Host replacement；
7. 无全局 retire gate时不启动 Pulumi update，不先删除上游依赖；
8. pending operation期间旧 writer、另一 Stack或新 operation不写；
9. Import/Create不把保留数据或 retire tombstone Host当成空 Host；
10. Host master/domain key异常时不自动重写 remote secret；
11. inspect/refresh不执行隐式 repair；
12. unowned同名 runtime对象不被删除或覆盖。

## 20. 需求—测试追踪

机器可检查的权威manifest为[requirements.yaml](requirements.yaml)。每条记录都使用稳定ID并包含priority、规范原文定位、test ID、layer、negative oracle、evidence和serverless exclusion。例如：

```yaml
- requirement: TECH-9.4-READ-UNREACHABLE-KEEPS-ID
  priority: P0
  tests: [PVD-READ-006]
  layers: [L3, L5]
  evidence: provider-lifecycle.json
```

CI validator要求：

- 每个 MUST、必须、不得、不允许至少映射一个 test ID；
- P0 negative behavior至少一个 call-count/file-state oracle；
- 每个 operation phase映射自动生成 fault suite；
- 每个旧测试标记 `retain | replace-by | unrelated`；
- serverless provider测试不得映射到 Controller requirement；
- CI 汇总未映射 requirement、未执行 test和skip原因，任一 P0/P1缺失即失败。
- test binary必须输出实际执行test ID；仅在YAML中声明但没有运行证据等同未覆盖；
- production effect ID与[side-effects.yaml](side-effects.yaml)双向一一对应。

Test selector grammar只允许完整ID或单个尾缀`-*`；CI从binary `--list-test-ids`与generator resolved lock展开selector，zero-match、重叠到不同canonical ID或未执行都失败。生成case canonical ID固定为`<suite>-<phase-or-factor>-<fault-or-index>`，例如`JRN-PHASE-render-F03`；裸`JRN-F01`只作为fault dimension，不算可执行test。CI提交`resolved-tests.lock.json`，记录generator version/seed、selector到具体ID、执行结果和evidence digest。

## 21. 现有测试迁移

现有测试先通过脚本生成 lexical inventory，再人工确认语义归属。初步分类：

- `infra/spec_test.go`、`config_test.go`、`layout_test.go`、`triggers_test.go` 中 Host config/projection语义迁入 L0；
- `test/host-preflight.test.ts`、`deployment-preflight.test.ts`、`deployment-mode.test.ts` 迁入 ownership/data-mode guard；
- `test/slot-state.test.ts`、`site-orchestration.test.ts`、`legacy-adoption.test.ts` 作为 Agent/Runtime parity oracle；
- `render-runtime-env`、`site-layout`、`compose` 的 Host runtime contract保留到 Go parity完成；
- Neon endpoint、Cloudflare/Upstash resource CRUD/schema相关测试归为 serverless unrelated，不计入 Controller覆盖；
- `infra/resources_test.go` 中 command.local graph测试在新 Host graph snapshot通过后删除，不能算作 Provider lifecycle覆盖。

迁移报告逐条列测试名称和 replacement test ID，不使用“旧 117 tests”这类易失数字作为完成判定。

## 22. CI 组织

建议命令：

```text
verify-model          # L0
verify-agent          # L1/L2/L6
verify-provider       # L3
verify-restart        # L4
verify-ssh            # L5
verify-migration      # L7
verify-controller     # 全部 Controller tests + race + secret scan + traceability
```

GitHub CI每次工作分支 push和PR运行 `verify-controller`。任务拆分可以并行，最终 required job只聚合真实结果。任何 P0/P1 skip、flaky retry后通过、race、泄漏或未映射 requirement都算失败。

固定质量门槛：

- `go test -race ./...`；
- journal phase × fault-point suite全绿；
- Provider跨进程 restart suite全绿；
- loopback OpenSSH contract全绿；
- operation lock和approval并发用例重复100次无 flake；
- 三个 binary amd64/arm64 build和bundle verify；
- secret canary scan零非法命中；
- 每个critical guard mutation都被测试杀死，mutation score=100%；
- legacy behavior mapping无未处理项；
- serverless excluded清单保持零误报。

## 23. 完成判定

Controller测试完成需要同时满足：

1. tech spec中所有 MUST/禁止项都进入traceability manifest；
2. 本规格所有等价类、边界值和12个强制高阶组合均有可执行 test ID；
3. lifecycle状态矩阵没有未定义的 error/NotFound/unknown-result分支；
4. 所有 journal phase和非幂等子动作通过故障注入；
5. Provider/Pulumi跨进程恢复证明 operation和副作用总数均为一；
6. Read不可达保留resource ID、Import只读、Check/Diff纯度、Delete全局门禁有负向证据；
7. retire前后preserved asset inventory一致，tombstone阻止误Create；
8. 用户SSH配置、known_hosts、private key摘要不变，子进程无secret继承；
9. 当前仓库每个旧测试完成 retain/replace/unrelated登记；
10. GitHub Controller CI全绿，无 P0/P1 skip；
11. 报告明确写明serverless provider行为未测试，不把opaque output测试描述成云集成通过。
