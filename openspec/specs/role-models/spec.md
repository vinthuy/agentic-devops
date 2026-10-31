# role-models Specification

## Purpose
允许分级、规划、编码和审查使用不同模型，同时保留默认模型和缺单价告警。

## Requirements

### Requirement: Unset role uses the default model
`roles` 中没有写明的角色必须使用 `model` 字段的默认模型。未配置 `roles` 时，所有角色必须继续使用默认模型。

#### Scenario: Only coder is overridden
- **WHEN** `roles.coder` 指定了另一个模型，且其他角色未指定
- **THEN** 编码使用指定模型，分级、规划、审查和未列出的调用使用默认模型

#### Scenario: Roles field is absent
- **WHEN** `models.yaml` 没有 `roles`
- **THEN** 每个角色都使用 `model` 字段

### Requirement: Used model must have a price entry
被实际调用的模型若没有单价，系统必须发出缺单价告警。不得在没有告警的情况下把该调用显示为免费。

#### Scenario: Role model has no price
- **WHEN** 某角色配置的模型不在单价表中并被调用
- **THEN** 该次调用产生缺单价告警，费用记录仍能看出所用模型名

### Requirement: One endpoint for all roles
这一变更中的角色模型必须共用同一份端点地址和 API key 环境变量。系统不得要求或读取按角色分开的厂商密钥。

#### Scenario: Role names differ
- **WHEN** 两个角色使用同一端点上的不同模型名
- **THEN** 两次调用都使用全局的端点地址和同一个密钥环境变量
