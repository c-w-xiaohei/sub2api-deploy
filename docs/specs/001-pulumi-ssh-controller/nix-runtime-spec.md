# Nix Runtime Environment Spec

日期：2026-09-16。状态：已确认方案，实施中；不是上线验收结论。

## 1. 原始目标

1. 删除部署仓库中的 Node 运行时。
2. 使用 Nix 而不是 NixOS，为控制机和普通 Ubuntu Host 提供一致的软件环境。
3. 项目程序由 Go 与现有 exact-SHA CI 编译；Nix 不从项目源码构建程序。
4. Controller 运行 Pulumi、SOPS、系统 OpenSSH 和 CI 构建的项目程序。
5. Host 提供 Docker、containerd/runc、nftables/iptables 和 OpenSSH，并通过显式 activation 接入 Ubuntu 24.04/systemd。
6. Nix 不接管 Pulumi graph、Host 生命周期、App/data reconcile、SSH daemon 或秘密。

本任务不是建立新的 Linux 发行版、包管理器或通用发布审计系统。

## 2. 软件来源

- 项目 CLI、Environment Program、Host Provider 和双架构 `sub2api-host` 必须来自同一个 exact-SHA CI candidate。
- `flake.lock` 固定 nixpkgs，提供 Bash/coreutils、SOPS、OpenSSH、Docker、containerd、runc、nftables 和 iptables。
- `nix/prebuilt.nix` 只固定当前 nixpkgs 未提供所需版本的 Pulumi 3.256.0、Cloudflare Provider 6.18.0 和 Upstash Provider 0.5.0 官方发布树。
- 禁止在安装机编译 Sub2API、Pulumi Engine 或 Provider 源码。允许 Nix 运行只组合文件、生成 wrapper 和静态配置的受限 derivation。
- `nix/install-runtime.sh` 先用 `runtimeInputs.<system>.<role>` 从官方 binary cache 逐个复制锁定的 nixpkgs closure；任一 cache miss 立即失败。随后关闭 substituter 离线安装环境，不能回退源码构建。
- 不维护 Alpine APK source lock、手工 ELF 闭包、私有动态 loader 或重复 file inventory。

## 3. Controller

Controller 首版仅支持 `x86_64-linux`，包含：

- exact-SHA project candidate；
- Pulumi 3.256.0 与 Go language host；
- Cloudflare/Upstash Provider plugins；
- SOPS、OpenSSH 和必要基础工具。

环境保留 candidate 的项目程序字节。Pulumi state、credentials、配置、秘密与
解密结果均位于 store 外。`workspace-init` 在可写目录创建 `Pulumi.yaml` 和
`bin` 链接；冲突时拒绝覆盖。

## 4. Host

Host 支持 `x86_64-linux` 和 `aarch64-linux`，由锁定 nixpkgs 提供 Docker、
containerd、runc、nftables、iptables 和 OpenSSH。

Host activation：

1. 只支持 Ubuntu 24.04 + systemd，并要求 root。
2. 只管理 `sub2api-nix-docker.service/socket`、`/etc/docker/daemon.json` 和 root Nix profile。
3. 拒绝接管已有 distro Docker unit、未知配置、未知 data root 或 socket。
4. 默认只配置；`--start` 和 `--restart` 为显式操作。
5. Host environment installs the selected `sub2api-host` payload through the root Nix profile; activation does not modify it. 不创建 deploy 用户，不修改 sshd/host keys/sudoers，不处理 App、数据库、journal 或动态 nft table。

## 5. 发布

Release workflow：

1. 选择并验证目标 commit 的成功 exact-SHA CI candidate。
2. 要求同一 commit 的 Nix Runtime workflow 成功。
3. 用 candidate archive 和 CI metadata 生成小型 Nix descriptor；descriptor 记录 candidate URL、commit、NAR hash、由 project SHA 派生的 Host release identity，以及从同一 candidate manifest 校验得到的双架构 Host path/SHA256。
4. 一次发布 candidate、checksum、metadata、`runtime-release.json` 和 descriptor archive。

不再生成 Controller/Host 三份二次 runtime tarball，也不在 workflows 之间搬运
APK、ELF inventory 或多层 manifest。

## 6. 验收边界

- 本地/CI fixture 必须调用生产 `nix/environments.nix`。
- x86_64 验证 Controller、Host、workspace-init 和 activation fixture。
- arm64 runner 构建并运行 Host 的 Docker/nftables/iptables/SSH 入口。
- exact-SHA candidate 继续由主 CI 证明项目程序、Host 双架构 bytes 和 Provider 合同。
- Host profile 必须通过 `/nix/var/nix/profiles/sub2api-host` 提供 `bin/sub2api-host` 和 exact release identity。
- 未发布 candidate 时 Controller 必须 fail closed；Host 环境仍可独立求值。
- 测试通过不等于生产 activation 或正式 Release 已执行。
