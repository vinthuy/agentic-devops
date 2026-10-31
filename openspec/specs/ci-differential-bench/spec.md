# ci-differential-bench Specification

## Purpose
让专门比较 CI 与本地验证的基准能够测到 CI 独有的失败，而不是重复执行同一批命令。

## Requirements

### Requirement: Differential benchmark has a CI-only check
用于衡量 CI 额外发现能力的基准剧本，必须在 workflow 中包含至少一条本地验证命令不执行的检查。该检查失败时必须能进入现有的 CI 修复流程。

#### Scenario: CI-only check fails
- **WHEN** 本地验证命令通过，且 workflow 中那条额外检查失败
- **THEN** 该次运行不得记为 CI 首次通过，并且失败可被现有 CI 修复流程读取

### Requirement: Existing same-command results stay unchanged
本地验证与 CI 使用同一批命令的既有基准结果必须保持原结论。本能力不得把那些结果改写成 CI 比本地更严格。

#### Scenario: Same commands are used
- **WHEN** 一个基准剧本的 CI 命令与本地验证命令相同
- **THEN** 它的既有通过记录不被本能力改写
