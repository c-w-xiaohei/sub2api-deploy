# Sub2API Deploy Specs

本目录保存已经冻结、可以直接指导实现和验收的规格。目录名使用三位递增编号和稳定 topic：

```text
docs/specs/<NNN>-<topic>/tech-spec.md
docs/specs/<NNN>-<topic>/test-spec.md
```

编号表达规格登记顺序，不随实现提交或版本号重排。topic 一旦进入实现只允许通过同目录文档修订，不通过改目录名制造第二份事实源。

| 编号 | Topic | 状态 | 技术规格 | 测试规格 |
| --- | --- | --- | --- | --- |
| 001 | Pulumi SSH Controller | Ready for implementation（Terra freeze gate passed） | [tech-spec](001-pulumi-ssh-controller/tech-spec.md) | [test-spec](001-pulumi-ssh-controller/test-spec.md) |

Topic 001的机器可检查附件：

- [requirements.yaml](001-pulumi-ssh-controller/requirements.yaml)：规范性需求、测试层、负向oracle和CI evidence追踪；
- [side-effects.yaml](001-pulumi-ssh-controller/side-effects.yaml)：所有受管外部副作用的恢复契约；
- [pairwise-model.yaml](001-pulumi-ssh-controller/pairwise-model.yaml)：等价类all-pairs模型、约束、seed和覆盖门槛。
- [legacy-bridge.yaml](001-pulumi-ssh-controller/legacy-bridge.yaml)：旧Shell mutation入口、嵌套锁与接管attestation。
