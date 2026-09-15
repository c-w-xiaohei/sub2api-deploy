# Cloudflare Go SDK Isolation Amendment

状态：用户授权实施，2026-09-14；性能结果待 CI，不是已优化结论。

## 目标

移除全量 `github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare`
生成 package 的编译依赖。Go 按 package 编译，仅使用 DNS 也承担全包成本；
`-p=1` 不会拆分单个大 package。运行时继续使用官方预构建 Cloudflare
Provider 6.18.0，通过公共 Pulumi Go SDK 注册资源；不自行调用 Cloudflare
HTTP API，不修改 Engine/Provider，不升级任何 Provider 版本。

## 最小实现

新增 `internal/cloudflareresource`，只声明实际用到的 Provider、DnsRecord、
ZoneSetting。当前 `internal/program` 和 legacy `infra` 均迁移到它，避免
旧链测试再次触发全量 SDK 编译。实现采用小型 typed inputs/state 和
`pulumi.Context.RegisterResource`；不做通用云 SDK 或反射代理框架。

固定 6.18.0 SDK 源码是兼容行为的对照，不把它作为测试 import 留在闭包。
必须保持：

- `pulumi:providers:cloudflare` 和 resource type tokens。
- 原逻辑名、parent、aliases、Provider reference、DependsOn 及调用方 options。
- Provider 凭据输入 secret 标记与 AdditionalSecretOutputs。
- DnsRecord 的 `cloudflare:index/record:Record` type alias。
- package version 6.18.0 及 plugin metadata 行为。
- required fields、wire 字段名/类型、unknown/computed/secret 传播。
- 旧 ZoneSetting SSL strict 输入与现有 callers 消费的 output。

本模块不是全量 SDK 的 drop-in replacement。当前 caller 只直接注册资源、
消费 ID 和传递依赖，不使用 Cloudflare typed resource-reference rehydration；
不为不存在的调用路径恢复整套 Input/Output/module registry。未来引入这类
调用时必须明确扩展合同，不能将当前子集当作完整 SDK。

预构建 Go language-host shim 的 `go list -m -json all` 必须返回 Cloudflare
及 Upstash 插件 metadata，独立于源码 go.mod。其中 Cloudflare module 名称
是 plugin discovery descriptor，不得为了消除字符串而删除；它不会触发
Go SDK 编译。shim 必须继续拒绝 build/install 等编译命令。

所有源码调用者迁完再移除 go.mod 中直接 SDK requirement；go.sum 的准确
清理由 CI tidy 产生，不本地生成。不要以优化名义删除 legacy 行为或测试。

## 验收

1. 核心 SDK mock 捕获真实注册 RPC，断言类型、输入、alias、Provider 版本、
   secret/dependency/parent 等，而不是比对实现字符串。
2. CI 依赖闭包拒绝全量 Cloudflare SDK，包括测试依赖；禁止把旧 SDK 放进
   parity test 而恢复巨型 package 编译。
3. 旧 checkpoint 在新 Program 下 preview 无非预期 replace/delete；保留
   managed DNS identity、reverse-removal 和 readiness-before-publication。
4. 使用官方固定 Provider schema/Check 验证所用字段；假 Provider 通过
   不等于官方 Provider 兼容证据。
5. exact-SHA 原有八项 gates 和 same-byte release 仍通过。
6. 同 runner 冷缓存比较 Program/Engine/Import 编译 wall time 与 cgroup peak，
   不预先承诺减少多少。只把有测量的目标称为已优化。

本地不运行会触发完整 Cloudflare SDK 的 build/test/vet/list，也不重新
编译基线来测资源。当前 Node 清理、Nix 工作继续独立实施。
