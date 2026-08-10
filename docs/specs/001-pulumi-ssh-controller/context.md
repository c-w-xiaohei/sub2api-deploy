# Pulumi SSH Controller Spec Context

日期：2026-08-10
状态：Source requirement for spec drafting

## 1. 目标

为 Sub2API Deploy 起草一般性的技术规格和测试规格，指导后续完整实施：

```text
一个 Environment Pulumi Stack
-> 官方 Cloudflare / Neon / Upstash Providers
-> 每台服务器一个自定义 Host resource
-> Host Provider 使用系统 OpenSSH
-> 远端 sub2api-host 按请求执行后退出
-> 本机 Compose / Traefik / App / Docker PostgreSQL/Redis / MicroSocks / Tunnel connector
```

规格必须说明责任、接口、生命周期、故障边界、迁移和验收，但不提前发明实现框架或穷举全部内部类型。

## 2. 权威来源

按优先级：

1. 当前对话中用户确认的目标与原则。
2. `tmp/final-architecture-decisions-and-implementation-plan.md`。
3. `tmp/configuration-semantics-and-terminology-analysis.md`。
4. 当前 `deploy/` 源码、README、Pulumi配置和测试所证明的现状。

远端分支 `origin/agent/pulumi-ssh-controller-specs@694bb3c` 已决定弃用，不是权威来源。可参考它的目录结构、spec版式、风险清单和个别生命周期/测试想法；不得继承其`Ready for implementation`结论或未经重新论证的设计。

## 3. 设计原则

### 3.1 最少必要概念

- 如无必要，不增加概念实体。
- 用户只需要理解环境、服务器、App、PostgreSQL、Redis、公开访问和出站代理。
- 自定义Pulumi Provider只暴露一个深资源：`Host`。
- App、Traefik、Docker PostgreSQL/Redis、MicroSocks和Tunnel connector是Host内部配置，不是独立自定义资源。
- 内部模块、interface、adapter或状态字段必须对应真实责任、变化原因或测试seam，不能只为结构对称存在。

### 3.2 Pulumi是全局生命周期引擎

Pulumi拥有：

- Resource graph和依赖。
- State、preview、diff、refresh、import和update history。
- Protect、retain、aliases、secret tracking和Stack update lock。
- 官方云资源生命周期。
- Host资源生命周期和跨Host/云资源的顺序。

禁止另建：

- Plan engine。
- 全局resource graph。
- 控制机operation database。
- Cloud reconciler。
- 与Pulumi并行的长期desired/observed state系统。
- 通用事务协调器或saved-plan解释器。

### 3.3 Host是深模块

Host interface应只表达控制机需要知道的服务器身份、目标语义、最小secret子集和可观察结果。

Host内部派生并隐藏：

- Compose project和container names。
- Network、slot alias、route path和runtime paths。
- 本机文件布局和命令步骤。
- Journal phase和恢复细节。

控制机不得为了实现方便而把这些本机细节提升为用户配置或全局概念。

### 3.4 控制机不管理业务

- Sub2API拥有业务数据、账号、动态设置和应用schema。
- 控制机不执行PostgreSQL或Redis数据搬运、恢复或业务正确性验证。
- PostgreSQL/Redis链接变化必须普通apply默认阻止，目标数据由操作人员或外部平台准备。
- 控制机只安排流量、writers、连接切换、单副本启动和恢复服务的安全顺序。
- Sub2API第一方schema migration由应用启动行为触发；deploy观察结果但不实现另一套schema migrator。

### 3.5 离线不影响业务

- 控制机离线不得影响业务。
- 服务器不运行Pulumi。
- `sub2api-host`不常驻、不监听网络、不注册、不心跳。
- 不引入Kubernetes式控制平面、Agent服务发现或持续reconcile loop。

### 3.6 系统OpenSSH是transport

- 使用系统`ssh`和已有alias，保留OpenSSH config、Include、Match、ProxyJump/ProxyCommand、agent、certificate和known_hosts语义。
- 不使用`command.remote.Command`或`x/crypto/ssh`作为主transport。
- alias作为单独argv传入；目标数据和secret走stdin；不启动本地shell。
- Host key验证必须fail closed；Provider不修改SSH config、known_hosts或private keys。
- stdout只承载有界机器协议；stderr仅用于脱敏诊断。

### 3.7 失败恢复只放在真实责任处

- Pulumi保留Stack update和失败资源事实。
- 远端Host journal只解决单台Host副作用和SSH未知结果。
- 相同Host resource、相同目标revision的重试必须resume或返回原结果，不能盲目重复副作用。
- 不为远端journal建立控制机镜像数据库。
- 规格只冻结必要的恢复不变量，不提前冻结大而全phase框架、GC系统、transition index或effect registry。

### 3.8 安全确认是防误操作门禁

- PostgreSQL/Redis connection identity变化和Host退役需要一次性明确批准。
- 批准绑定变更对象、old/new identity或目标revision，并且不能变成长期配置开关。
- 第一版不引入独立PKI、Host clock协议、签发者体系或通用审批平台，除非先有明确威胁模型证明必要性。

### 3.9 数据保护优先

- 官方Neon/Upstash等数据资源使用Pulumi Protect；迁移时保留身份。
- Host Update不得隐式删除、更换或重新初始化持久volume/data path。
- Host Delete只解除deploy-owned运行外壳，默认保留volume、bind data、业务数据和手工对象。
- Unreachable、协议错误、Host state损坏或Agent缺失不能被Read解释为resource absent。

### 3.10 规格应可实施而不过度冻结

- 一般spec固定接口承诺、顺序、不变量和验收，不固定未验证的package patch版本、任意容量、保留天数、目录权限矩阵或内部helper数量。
- 只在安全、兼容、持久身份或跨进程恢复需要时固定格式。
- 测试跟随真实风险与接口，不先建立自定义test governance framework。
- 没有实现和执行证据时，状态只能是Draft或Ready for implementation review，不能宣称已通过不存在的freeze gate。

## 4. 当前仓库事实

基线仓库：`deploy/`，当前HEAD为`07ffde4`，本地有未提交Go validate切片，规格工作不得覆盖或重写这些变化。

当前生产架构：

- 一个VPS一个Pulumi Stack。
- Pulumi在目标VPS本地运行。
- Go Program通过`command.local.Command`调用Shell/TypeScript和本地Docker Compose。
- 每Host包含共享Edge和多个隔离Site。
- Cloudflare、Neon和Upstash与Host Stack耦合。
- 现有blue/green、preflight、adoption和state脚本是迁移行为证据。

当前具体证据：

- `deploy/README.md`描述one-Host-per-Stack与本地执行。
- `deploy/infra/commands.go`使用`command.local.Command`。
- `deploy/scripts/reconcile-site.sh`管理本机data/App/route和probe。
- `deploy/scripts/application-release.sh`与`switch-slot.sh`管理image blue/green。
- `deploy/scripts/host-preflight.ts`、`deployment-preflight.ts`和state writers提供adoption/ownership证据。
- `deploy/infra/cloudflare.go`、`database.go`、`redis.go`包含现有云资源身份和生命周期。
- Go版本固定`1.25.11`，不得升级。
- 当前环境实际有Go 1.25.11和Docker 29.3.0；不能以“本地没有Go/Docker”为spec前提。

## 5. 目标运行实体

只保留：

```text
sub2api-deploy
  薄CLI：环境选择、SOPS、标准Pulumi命令、一次性危险操作确认

Environment Go Program
  从config/secrets注册官方云资源和每服务器一个Host

pulumi-resource-sub2api-host
  一个自定义Provider，只暴露Host resource

sub2api-host
  远端按需执行binary，inspect/reconcile本机状态
```

“Agent”如在内部讨论中出现，只能表示这个按需binary，不能暗示常驻Agent或控制平面。

## 6. 必须定义的Host生命周期

- `Check`：纯输入校验和canonicalization；不得SSH。
- `Diff`：比较Pulumi输入/状态，普通变化in-place；不得SSH；危险链接变化可显示diff但Update必须要求批准。
- `Create`：Host资源拥有完整生命周期，包括通过系统OpenSSH安装/升级`sub2api-host`、校验machine identity并reconcile；不得要求用户先运行另一个日常bootstrap生命周期。
- `Read`：SSH执行inspect；更新稳定observation和drift；unreachable/corrupt/missing-agent保留resource ID并报错。
- `Update`：reconcile完整Host目标；使用稳定resource identity和desired revision恢复SSH未知结果。
- `Delete`：仅移除deploy-owned运行外壳，preserve data；不得通过普通delete隐式销毁业务数据。
- `Import`：只读、program-first；无法证明identity/ownership时失败，不通过apply猜测接管。

## 7. State边界

只允许两类持久state：

```text
Pulumi state
  resource inputs/outputs、provider IDs、dependencies、protect、aliases、secrets、history

Remote Host state
  machine identity、ownership evidence、applied desired revision、稳定runtime observation、operation journal
```

Remote state不是第二份环境配置；控制机本地临时批准或命令执行文件不是新的长期state系统。

## 8. 云资源与Host资源关系

- 官方Cloudflare/Neon/Upstash Provider直接注册资源，不增加包装Provider。
- Environment Program必须保留unknown和secret语义。
- Managed data资源使用protect，并在迁移中保留URN/provider ID或使用aliases/import/state move。
- 公开入口只有在目标Host readiness满足后加入；删除Host前先从公开入口摘除。
- 新compute访问本地data Host时，先更新data Host允许来源，再创建/更新App Host，再发布公开入口。
- App image在多Host上按稳定顺序滚动；本机blue/green由`sub2api-host`完成。

## 9. 迁移约束

- 现有云资源和Host运行时不能被重新创建或隐式删除。
- 迁移前记录当前URN、provider ID、protect/retain、Compose project、labels、paths、active slot/image和data identity。
- 新旧writer不得同时修改同一Host资源。
- 允许先部署只读inspect能力，再program-first import Host。
- Import后preview必须no-op或只显示已明确接受的差异，才能切换writer。
- 云资源迁入新Environment Stack时目标为0 create、0 delete、0 replace。
- Legacy bridge应是有界迁移工具，不成为长期运行架构；不要为每个旧脚本发明永久effect registry。

## 10. 测试原则

优先证明以下行为：

- 配置严格、引用正确、每服务器恰好一个Host。
- Check/Diff不SSH，Read/Import只读。
- Read不可达不返回NotFound。
- SSH response丢失后相同目标不产生第二次非幂等操作。
- Blue/green失败保留旧slot和route。
- 数据链接变化无批准时零runtime副作用。
- Delete保留volume/data并不碰unowned对象。
- System OpenSSH argv、host key和协议framing安全。
- Secret不进入日志、argv、普通outputs或journal正文。
- 旧writer与新writer不双写。
- Environment Program正确注册官方Provider资源、依赖、protect、unknown和secret传播。

测试可以分层，但层数应服务于真实seam；第一版不要求自定义pairwise generator、mutation score 100%、所有内部步骤笛卡尔故障矩阵或测试selector治理系统。

不做冒烟测试。真实公网endpoint、云服务可用性和最终用户路径探测不属于自动化测试、部署后验收或完成门槛。`sub2api-host`在reconcile期间执行的Host本机health/readiness检查是安全切换和依赖排序的运行时前置条件，不是冒烟测试；它不得扩展为公网或端到端探测。

禁止本地构建。不得在开发机执行`go build`、`npm run build`、release bundle assembly或其他二进制/发布产物构建命令；所有构建与发布产物验证必须由CI环境执行并提供证据。本地只运行单元测试、静态检查、格式/语法检查和不生成发布产物的rehearsal。

## 11. 远端弃用分支的可参考内容

可参考：

- `docs/specs/<NNN>-<topic>/tech-spec.md`和`test-spec.md`目录结构。
- Host Provider lifecycle分类。
- Read不可达保留ID、Import只读、Check/Diff纯度。
- OpenSSH单帧协议和unknown-result风险。
- Operation journal、blue/green、data preserve和single-writer测试想法。

必须丢弃或重新论证：

- Control ledger、environment lease和saved-plan successor engine。
- 独立Ed25519 approval PKI与Host clock体系。
- Host master key/HKDF作为一般spec前提。
- Agent必须预bootstrap后Host Create才能运行。
- Docker group加root helper所宣称的伪隔离。
- 固定12/7 phase、90天/128/512等任意策略。
- Side-effect registry、legacy caller graph manifest、pairwise generator和test selector framework。
- 完全排除Environment Program官方Provider wiring测试。
- 未执行即标记Ready/freeze passed。

## 12. 本轮交付

在`tmp/specs/001-pulumi-ssh-controller/`生成：

```text
tech-spec.md
test-spec.md
```

可按远端分支版式组织，但应更短、更深、更一般。两份规格状态为`Draft for implementation review`。

不修改`deploy/`源码，不合并或修改弃用分支，不生成requirements/effects/pairwise/legacy manifests。只有后续实现证明需要时，再从可执行测试或真实迁移工具生成辅助清单。
