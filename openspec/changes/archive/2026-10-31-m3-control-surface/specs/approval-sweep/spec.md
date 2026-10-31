# Spec Delta

## Purpose

在没有人执行普通运行命令时，仍然处理已经到期的审批和锁，并且不把该命令变成常驻服务。

## ADDED Requirements

### Requirement: Sweep expires pending work only
`agentdev sweep` 必须处理已到期的审批和锁。它不得创建任务，不得领取排队任务，也不得读取接入标签。

#### Scenario: An approval has timed out
- **WHEN** 一个等待审批的任务已经超过配置的超时时间，并且执行 `agentdev sweep`
- **THEN** 该任务按现有超时规则升级为待人工

#### Scenario: Nothing is expired
- **WHEN** 没有到期审批或到期锁，并且执行 `agentdev sweep`
- **THEN** 命令退出码为 0，且不创建任务

### Requirement: Sweep shares the run expiry rule
`sweep` 与普通运行命令必须调用同一个到期处理函数。两条路径不得各自实现一套超时规则。

#### Scenario: Timeout rule changes
- **WHEN** 审批超时的升级条件被修改
- **THEN** `sweep` 和普通运行命令都使用修改后的同一条件

### Requirement: Sweep process does not stay resident
`sweep` 结束后不得留下进程内定时循环。

#### Scenario: Command finishes
- **WHEN** `agentdev sweep` 打印处理数量并退出
- **THEN** 没有继续轮询的后台循环
