# Spec Delta

## Purpose

把仓库根目录的 `AGENTS.md` 提供给规划与审查，同时保证它不能放宽已经生效的安全策略。

## ADDED Requirements

### Requirement: Repository instructions are injected when present
仓库根目录存在 `AGENTS.md` 时，系统必须把它注入规划上下文和审查上下文。文件不存在时必须跳过，不得把任务标为失败。

#### Scenario: Instruction file exists
- **WHEN** 仓库根目录有 `AGENTS.md` 且任务进入规划
- **THEN** 规划上下文包含该文件内容，并标明来源是仓库文件

#### Scenario: Instruction file is absent
- **WHEN** 仓库根目录没有 `AGENTS.md`
- **THEN** 规划正常继续，不因缺少该文件而失败

### Requirement: Long instruction file is truncated
`AGENTS.md` 超过 8000 字符时，系统必须只保留前部，并在注入内容中标明已经截断。

#### Scenario: File exceeds the cap
- **WHEN** `AGENTS.md` 长于 8000 字符
- **THEN** 注入内容不超过前 8000 字符，并包含截断标记

### Requirement: Instruction text cannot relax policy
`AGENTS.md` 中要求放宽禁止路径、命令策略、预算或审批门的句子必须没有效力。程序侧策略必须仍按原配置拒绝。

#### Scenario: File asks to edit a denied path
- **WHEN** `AGENTS.md` 写明可以修改被禁止的路径
- **THEN** 对该路径的写操作仍然被策略拒绝
