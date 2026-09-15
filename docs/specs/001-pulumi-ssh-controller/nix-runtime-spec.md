# Nix Runtime Environment Spec

日期：2026-09-14。状态：已确认方案，实施中；不是上线验收结论。

## 当前修订：声明环境，而非包装安装脚本

本节优先于下面旧稿中要求所有环境预先打成单一完整包、禁止任何
materialization builder 的限制。用户确认：Go/现有 CI 编译程序，Nix
只消费预构建软件；在本机生成静态配置、创建链接和组合环境不属于源码
编译。允许受限的、只执行这些文件操作的 Nix derivation，禁止 stdenv、
编译器、源码包或 cache-miss 源码回退。组合器所需 shell/coreutils 同样
必须来自锁定的预构建工具，而不是借用不受声明控制的 builder PATH。

Nix 声明层负责组件选择、Docker unit、daemon config 和安装清单；
activation 只消费已生成文件，验证所有权并显式应用。不得在 Shell 里
继续拼装 unit 或决定业务参数。现有 journal/权限/失败恢复属于系统接入
必要机制，不能为缩短脚本而删除；App/data/切流仍由现有 Go runtime 管。

发布流程由 Go CI 产出二进制，取得真实摘要后自动生成伴随 release 的
Nix 描述/锁。描述不嵌入其自身所描述的 archive；基本发布链不强制依赖
Bot PR。没有真实组件产物时继续 fail closed，不能假造可用版本。

测试必须调用生产 Nix 环境组合模块的成功路径，使用测试专用固定组件
代替 GitHub 下载；生产入口不能接受任意 file URL 或绕过校验。验证
生成 unit/manifest 和激活器消费关系、profile/app 安装执行、无源码
builder，以及配置参数变化产生对应产物。单独测试 opaque store path
安装不足以证明生产模块可用。真实软件的 ELF/plugin/Ubuntu VM 激活
仍是独立验收，不能用 fake payload 冒充。

## 1. 原始需求与决定

用户要求：彻底移除部署仓库中的 Node 基建；只使用 Nix 而非 NixOS，
统一每台机器的软件运行环境，收敛零散运维脚本。构建与运行分离：
项目由 Go 和现有 CI 构建，运行时只从 GitHub 获取已构建完毕的产物。
Nix 不负责项目源码构建，不得编译 Pulumi CLI/Engine。

本规格 supersede 先前讨论中的 Nix Go builders、开发 shell、NixOS
modules、自动源码构建和 Nix 构建整个 CI 的建议。

## 2. 责任与系统范围

| Owner | 负责 | 不负责 |
| --- | --- | --- |
| Go / GitHub Actions | 编译、测试、exact-SHA candidate、GitHub Release | 在操作机临时编译 |
| Nix | 固定 GitHub artifact、摘要校验、软件环境、启动入口、静态系统接入配置 | Go/Engine 源码构建、生产资源变更 |
| Host activation | 普通 Linux 上显式应用已生成的系统配置 | App reconcile、数据库变更、无提示接管已有服务 |
| Pulumi | graph、checkpoint、cloud resources、依赖顺序、import | Host 操作系统包管理 |
| Host Provider/Runtime | Host binary 安装升级、Docker 对象、动态 nft table、journal、数据与切流 | 与 Nix 争用底座服务 |
| SOPS / CLI | store 外密钥、运行时私有解密与 secret projection | 将明文写入 derivation/store |

首版底座为 Ubuntu 24.04 + systemd，支持的平台必须逐个取得证据。
首版 Controller 仅发布 `x86_64-linux`；Host 环境发布 `x86_64-linux` 和
`aarch64-linux`。arm64 Controller 在取得独立 exact-SHA candidate 证据后再开放。
机器需先具备 Linux、root/已有 SSH 通路、宿主 CA 与已安装 Nix。
Nix 的首次安装是外部 bootstrap 前提，不在运行时偷偷安装或执行远程脚本。
目标机器不转换为 NixOS，不替换现有 SSH daemon，也不改其 host keys。

## 3. 两套环境

### 3.1 Controller

安装入口为 `nix profile install .#controller`，运行入口为
`nix run .#deploy -- validate production`，其他现有 CLI 参数不变。

包含同一项目 release 的 CLI、Program、Provider、双架构 Host artifacts，
以及完整官方 Pulumi 3.256.0 运行组件（至少验证 Go language host）、
SOPS、SSH client、必要工具及锁定 Provider plugins。

启动必须绑定固定依赖路径，保持 cwd、HOME、SSH config 和相对 backend
语义。配置、Pulumi home/state、解密文件均在 store 外。不得 `cd` 到 store。
只读 plugin payload 不等于可写 PULUMI_HOME；不得覆盖用户 credentials。
缺失 plugin 时不得运行下载器或源码编译器。实际 plugin discovery 必须
通过真实 CLI 测试证明，不能只检测文件存在。

Controller payload 必须保留现有 bundle 内 `bin/sub2api-deploy`、`bin/pulumi`、
`bin/pulumi-program`、`bin/pulumi-resource-sub2api-host` 和
`artifacts/sub2api-host/manifest.json` 的相对布局。仅设置 PATH 无法满足
cwd 中 `Pulumi.yaml` 的 `./bin/pulumi-program` 合同。必须提供
`nix run .#workspace-init -- <directory>` 显式入口：在指定可写工作目录
创建公开 Pulumi.yaml 和指向不可变 bundle bin 的链接；完整预检后才写入。
同版本重复调用无变化；已有文件/链接不匹配时拒绝，不能在 preview/up
中隐式覆盖。未初始化 workspace 的 Pulumi 操作必须失败并提示此入口。
plugins 固定在 payload 的 `plugins/` 下，由发布的 Pulumi launcher 将其
绑定到可写且权限受控的运行目录；必须验证实际 discovery。
不能把真实 config/secrets 复制进 store。workspace 升级须显式进行。

### 3.2 Host

安装入口为 `nix profile install .#host-environment`，显式系统接入入口为
`sudo <installed-path>/bin/sub2api-host-activate`。activation 不自动运行。

环境提供 Docker/dockerd、containerd/runc/辅助程序、nftables、必要网络与
文件工具。必须绑定 daemon 子进程、非交互 SSH、sudo 下的 PATH；交互 shell
能找到工具不足以验收。发布物必须包含实际运行所需库，不能仅复制动态 ELF。

Provider 独占 `/usr/local/libexec/sub2api-host` 及 stage binary 的安装升级。
Nix 不包装或修改该 binary 字节，否则会破坏 remote probe 的摘要合同。
运行时依赖路径如需增加显式 transport 接口，必须单独验证固定 argv/secret
  边界；不得靠修改全系统 PATH 或开放任意 root shell 绕过该问题。

现有 transport 在 bootstrap 使用 root，而 stdio 直接以 SSH 身份执行；
bootstrap 产生 root-owned 状态及 0700 binary，不能据此承诺非 root deploy
用户已可用。首版 Nix 底座不创建 deploy 用户或 sudoers；远端特权入口的
统一、固定 argv 和 UID 合同须另行闭合。当前实现不得修改该 Go transport
并把问题隐藏在 activation 中。NR-06 未完成前不声明全链路从零可用。

## 4. 预构建产物合同

发布描述内的 `nix/runtime-release.json` 是该环境的软件来源锁，按 role/system
记录 version、GitHub release asset URL、摘要和不可变根目录布局。它由
发布工具从真实资产生成，不要求操作员手工填写，也不强制 Bot PR 更新。
禁止 latest、分支 URL、伪造 hash、从本地源码 fallback。

全部运行依赖必须来自已审核 GitHub 发布包；不能将普通 nixpkgs package
当作“必定命中缓存”。实现可以采用 Nix 内置 fixed-output fetching，或
下载已发布 package closure，但必须证明没有依赖源码构建回退。
不得用 Nix builders 重新编译项目以生产运行闭包。

下载、校验、解包/链接与源码编译不同；仅前者允许在安装时发生。
包管理入口必须严格验证 role、系统、固定版本、路径和摘要。错误平台、
缺失 URL/hash/文件、下载失败必须 fail closed。禁止运行 archive 内安装脚本。
静态运行包或完整预构建闭包的格式，须以真实依赖可执行性验证后确定。

具体禁止引用任何源码编译 derivation、stdenv 编译环境、远程源码 builder
或 cache-miss 源码回退。生成文件和链接的受限 derivation 必须明确固定
预构建执行工具和输入，不借用操作机不确定的 PATH。固定 hash 本身不证明
archive 是 binary，必须核对
实际 inventory、entrypoints、ELF/库与 projectCommit。包声明的 requiredPaths
不是存在性证据。CI 对同一 archive 计算 raw SHA256 与 unpacked NAR hash，
写入 archive 外部发布锁/sidecar。运行端 fetchTree 校验 NAR tree；raw hash
是 CI 的归档来源证据，不能声称 fetchTree 同时验证了 raw bytes。
不得用两次独立 URL 下载假装证明二者关联，也不得在 archive 内的
inventory 嵌入包含它自身的 archive 摘要。inventory 记录非自引用的内部
文件摘要，外部 NAR pin 固定它和整个 payload 的字节。

现有 GitHub release v0.2.x 为 legacy VPS 包，不满足新 controller/Host
runtime environment inventory。未发布合规产物前，不得默认选它；应明确
报告 artifact unavailable。CI candidate 通过不等于已存在 GitHub Release。

发布附带来源清单、上游版本/摘要/许可证、项目 SHA；项目二进制必须消费
通过原有 gates 的同一批 bytes。运行环境包作为附加 release artifact，
不悄悄替换当前 candidate archive 合同。

外层 runtime artifact 摘要锁住内部 inventory 和 Host manifest 的精确字节；
inventory 记录项目 SHA、bundle manifest SHA256 和 Host 双架构摘要，发布
验收必须把它们与原 exact-SHA candidate 对上。无需为此另造 Provider state
或修改 Host resource identity。仅相同 release 字符串不足以证明 same-byte。

## 5. 系统 activation 合同

1. root、Ubuntu/systemd、环境完整性、文件 ownership 与权限先预检。
2. 未拥有的 Docker unit/socket/config、冲突用户/规则拒绝接管；无强制覆盖。
3. 生成配置先完整校验，再原子安装；显式记录本模块拥有的文件摘要。
4. 软件引用必须通过受保护的系统 profile/GC root 保持有效，用户 profile
   和 /etc 中的普通 symlink 不自动等于 Nix GC root。
5. systemd unit 明确 ExecStart、PATH、Docker 数据目录；同版本再次激活不
   重启 Docker，版本变化所需重启是单独的维护动作。
6. 不创建 `/var/lib/sub2api-host` 的 ownership/state 内容：现有 bootstrap
   会拒绝已存在的未知 root，必须按 Host 合同区分父目录与被管理目录。
7. 不自动生成 SSH key，不替换 sshd，不关闭现有 SSH 会话；deploy 身份与
   sudo 规则仅按已验证 transport 合同配置，不给任意 shell NOPASSWD。
8. 不全局 flush nftables，不创建 Runtime 拥有的动态 table；Docker 数据、
   ACME、journal、inventory、App/DB runtime files 不触碰。
9. 失败必须报告已执行阶段，不宣称跨用户、文件和服务操作具有事务性。
   profile rollback 只切软件，不自动回滚数据库或主机副作用。

首版由本模块拥有 `sub2api-nix-docker.service/socket`、对应 daemon 配置及
root profile `/nix/var/nix/profiles/sub2api-host`，默认 Docker socket 路径
保持 `/run/docker.sock` 以匹配现有 Runtime；CLI/socket 权限仍按特权
transport 合同验收。自有已启动 socket 不能被二次 activation 误判 foreign。
检测现有 distro units/config/data-root/运行 daemon，未明确拥有则拒绝接管。
安装系统文件前先完成工具、root ownership、不可变 payload、profile 和
全部冲突预检；不信任任意用户可写 payload、环境指定的特权工具或 manifest。

配置安装与服务启动分开：提供显式 `--start` 首次启动和 `--restart`
维护升级入口；默认只配置，不能暗中重启。切换系统 profile 时保留旧
generation/GC root，直到旧 daemon 已退出；普通 `/etc` 链接不承担 GC 保活。

## 6. 运维脚本收敛

下载/安装/版本选择/路径探测由 Nix 发布锁和环境入口替代；静态 unit 和
权限配置由声明生成，系统写入集中至一个 activation。
blue/green、readiness、回滚、API retry、adoption 和秘密原子写入仍属 Go
业务模块，不能搬进 Nix build/activation。后台 Node 清理任务独立实施；
本任务不重复改写其 legacy helpers、CI evidence parser 或测试迁移。

## 7. 实施顺序与实际阻塞

1. spec、自查、需求到验收映射。
2. 运行时专用 flake/锁校验与 fail-closed fetching；无源码 builder。
3. Controller 入口及 Host 静态配置/activation；无自动生产操作。
4. 由 CI 收集实际预构建依赖，产出 GitHub 环境包并锁入真实摘要。
5. 干净 Ubuntu VM 中验证两种环境和现有 Host/Engine gates。

本机已用官方 Nix 2.35.2 完成 production compositor 的 evaluation、realize、
profile install 和 app fixture；这不等于真实 runtime asset 或 VM 激活已通过。

安装接口使用真实的纯文件组合 derivation，不伪造 `type = derivation`，
也不引入编译 builder。Nix materialization/profile 管理是配置安装操作，
不是源码构建。最低版本 2.19，启用 nix-command、flakes、fetch-tree。
直接从固定发布描述的 flake 安装，避免 eval 打印路径后到第二进程安装
前的 GC 间隙。更新软件需要选择新发布描述；新版本先安装到候选 profile，
验证后显式切换旧 entry，不在运行命令中自动追 latest。
暂无合规完整 GitHub runtime assets；其发布是可安装验收的阻塞，不得
通过虚构 asset 或放宽 GitHub-only/no-build 约束来消除。

## 8. 自查结论

- 原始构建/运行分离保留；不引入 NixOS、Go devShell 或 Nix Go builder。
- Nix 不只下载 CLI，同时覆盖 Controller 与 Host 底座。
- 单一动态资源 owner 保留；不复写 Host binary、journal、数据库。
- 旧方案中的只复制 pulumi、隐式 nixpkgs 源码回退、sudo 任意 shell、
  user profile 作为 root 服务唯一 GC 保证均不接受。
- 真实 artifact 发布、非交互命令路径和 clean-VM 验收为显式门槛。
