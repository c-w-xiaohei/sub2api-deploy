# Nix Runtime Test Spec

日期：2026-09-16。状态：实施中。

## 必须证明

| ID | 行为 | 证据 |
| --- | --- | --- |
| NR-01 | 空 project release lock 时 Controller fail closed。 | `nix/tests/runtime-fixture-test.sh` |
| NR-02 | Host 在 x86_64 与 arm64 均可由锁定 flake 构建。 | Nix Runtime workflow 双 runner |
| NR-03 | Controller 组合 exact project payload、Pulumi、SOPS、SSH 和两个 Provider。 | `profile-install-fixture-test.sh` |
| NR-04 | workspace-init 幂等并拒绝覆盖冲突。 | `controller-workspace-fixture-test.sh` |
| NR-05 | Docker unit 绑定最终 Host store path，activation 只消费生成配置。 | environment/profile fixtures |
| NR-06 | activation 拒绝 foreign Docker 状态，支持恢复、幂等、显式 start/restart。 | `host-activation-fixture-test.sh` |
| NR-07 | descriptor 绑定 exact candidate bytes，校验 Host manifest schema/path/size/hash/release identity 并拒绝篡改、不安全 archive 和 descriptor source symlink。 | `nix_release_descriptor_test.py` |
| NR-08 | 正式发布要求同 commit 的主 CI 与 Nix Runtime workflow 成功。 | `release.yml` |
| NR-09 | 非空 release lock 可通过唯一的 `candidateOverride` 测试 seam 组合 Nix store candidate，并保持 project SHA 派生的 Host release binding。 | `nix/tests/runtime-fixture-test.sh` |

## 不再测试

- 私有 APK 解包器；
- 手工 musl/ELF dependency closure；
- 三份二次 runtime archive；
- 重复 inventory/descriptor manifest；
- 第三方 static-tools provenance。
- Host binary 的自定义 APK/ELF runtime layer。

这些机制已从设计中删除，不应以测试形式继续保留。
