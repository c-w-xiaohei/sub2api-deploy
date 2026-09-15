# Nix Runtime Acceptance

关联：[Nix Runtime Spec](./nix-runtime-spec.md)。
所有当前尚未执行的验收均为 pending，不继承旧 Go CI 的成功结论。

## 声明环境修订验收

以 Runtime Spec 的“当前修订”作为权威：允许由预构建 shell/coreutils
执行纯文件 materialization，不允许任何源码编译。此前只测试 opaque
store-path 的 fixture 保留为基础接口证据，不再充当产品成功路径验收。

| ID | 必须执行的测试 | 所证明的边界 |
| --- | --- | --- |
| ND-01 | 用固定测试组件调用生产 environment Nix 模块，真实 realize/profile install/app run | 生产组合实现能工作，而非另一个手写 fake flake |
| ND-02 | 改变支持的 Host 参数，检查生成的 unit/config/manifest 随之变化；非法路径或参数拒绝 | 配置在声明层，Shell 不另有一套模板 |
| ND-03 | activation 消费生成清单，在隔离 fixture 检查权限、foreign 拒绝、同版无重启和中断重入 | 执行器正确应用声明；不代表真实 systemd 工作 |
| ND-04 | materialization 只有锁定预构建工具，构建依赖闭包无源码/编译器；禁 remote builder fallback | 本机配置组合不是源码编译 |
| ND-05 | 发布生成器读取实际 tar bytes，外部锁/描述绑定 archive/tree/inventory/project SHA；缺资产、篡改、路径逃逸拒绝 | 自动发布描述真实绑定已有资产，不需手填 hash |
| ND-06 | 从发布描述下载真实 GitHub 组件，干净 Ubuntu VM 执行 Controller 与 Host 激活、重启、隔离部署 | 实际软件闭包、language host/plugin、服务和 SSH 路径可用 |

ND-01 可用测试组件，但不能放宽生产 GitHub URL 或摘要校验入口。
ND-05 中 NAR 工具替身仅测试生成器协议，真正的 NAR 计算必须在 CI 使用
固定预构建 Nix 执行。ND-06 在完整运行组件未发布时明确 blocked。

| ID | 需求 | 行为验收 |
| --- | --- | --- |
| NR-01 | 运行端只取 GitHub 预构建产物 | 空编译缓存/无 Go 环境安装；无 compiler/源码下载进程；所有软件来源对锁 |
| NR-02 | 发布锁可信且完整 | 缺失/非法 hash、浮动 URL、平台/role 不符拒绝；篡改 archive 无可执行安装 |
| NR-03 | Controller 路径确定 | 恶意 PATH shim 不获调用；cwd/HOME/相对 backend 保持；真实 Pulumi 版本 3.256.0 |
| NR-04 | 完整 Pulumi/插件 | 清空普通 plugin cache 后隔离 preview/up/import；不自动下载或构建；Go language host 真正执行 |
| NR-05 | 秘密在 store 外 | SOPS/passphrase/token fixture 不出现在 outputs/derivations/log/artifact；私有文件清理 |
| NR-06 | Host 非交互可用 | clean Ubuntu VM，真实 SSH/sudo 路径执行 Provider/Runtime，不依赖登录 shell |
| NR-07 | activation 幂等 | 二次同版本无文件漂移/无 Docker 重启；升级为显式维护动作 |
| NR-08 | 拒绝已有系统冲突 | 现有 Docker unit/socket/config/user/sudo 冲突不覆盖；失败前无系统 mutation |
| NR-09 | 状态与权限隔离 | 不预建未知 Host root、不改 Runtime binary/hash，不读写 journal/data，不开放任意 sudo shell |
| NR-10 | GC 与失败 | 系统 root 保留完整环境；旧 profile GC 不破坏 daemon；中断激活可报告/重入 |
| NR-11 | 网络所有权 | 保留 foreign nft sentinel，不管理 Host 自有 table，不改变现有 SSH daemon |
| NR-12 | 发布证据 | 新运行包引用原 exact-SHA 已测项目 bytes；上游 runtime inventory/ELF/库/许可证齐全 |

workspace-init 必须在任意写入前检查 `Pulumi.yaml` 和 `bin` 的全部冲突；
同版本重入不改变任何文件。第二个目标冲突时第一个目标也不得被创建。
实际 `nix profile install` 与 `nix run` 须测试成功路径，只有空锁/非法锁
拒绝用例不够；attrset 标记 `type = "derivation"` 不证明 profile 可安装。
archive 的摘要记录在外部发布锁/sidecar，不能要求 archive 内 inventory
嵌入它自身 archive 的最终摘要（这会产生不可构造的自引用）。

本地只做静态或不启动系统服务的 fixture 测试；不运行真实 activation、
Docker 或生产 Pulumi。Nix evaluation/安装检查需已有 Nix，VM/Engine/privileged
测试由 CI 运行。所有本地检查与后台 Node 任务的执行协调，保持串行。

实现不得通过字符串匹配断言替代下载校验、进程行为、路径绑定或 VM 验证。
新 runtime assets 未发布时，预期行为是明确不可用，不以 legacy 包代替。

## 当前本地证据（工作树，未提交）

- `bash nix/tests/host-activation-fixture-test.sh`：临时文件系统、模拟
  systemctl/profile 下通过；覆盖幂等、真实 Unix socket、foreign 拒绝、
  配置部分写入后的恢复。不是 root/真实 Docker/VM 验收。
- `bash nix/tests/controller-workspace-fixture-test.sh`：通过，覆盖两目标
  全预检、冲突时不部分写入及同版本重复使用。
- `nix/tests/profile-install-fixture-test.sh`：已接入独立
  `.github/workflows/nix-runtime.yml`，并在本机 Nix 2.35.2 通过；用本地测试
  payload 验证 production compositor、profile/app 和固定 PATH 行为，不代表
  GitHub 运行包已存在。
- `nix/tests/runtime-fixture-test.sh`：已在本机通过并接入同一 CI；验证空锁
  和非法发布锁拒绝。真实下载/ELF/plugin/Host transport 仍 pending。
- 所有 `nix run`/profile 示例均以发布锁有真实合规条目为前提；当前为空。
