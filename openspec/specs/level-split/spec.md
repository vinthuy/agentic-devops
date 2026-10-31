# level-split Specification

## Purpose
把无法直接编码的 L 级需求拆成按顺序单独审批的 M 级子任务，并在拆不开时停下来。

## Requirements

### Requirement: Level L does not code as one task
分级为 L 的任务不得进入编码。拆分校验通过时，系统必须只创建第一段 M 级子任务，并让该子任务停在审批门。父任务必须记录子任务 id，且父任务不得产生提交。

#### Scenario: Split succeeds
- **WHEN** 任务被分级为 L，且分段计划通过校验
- **THEN** 只存在第一段子任务，它的级别为 M 并处于等待审批，父任务没有提交

#### Scenario: Split fails validation
- **WHEN** 任务被分级为 L，且分段计划无法通过校验
- **THEN** 父任务进入待人工，原因为拆分失败，并且没有子任务进入编码

### Requirement: Approval covers one slice
批准、驳回或退回某一段不得改变其他段的审批状态。上一段被拒绝或取消后，系统不得创建后续段。

#### Scenario: First slice is approved
- **WHEN** 操作者只批准第一段
- **THEN** 第一段可以离开审批门，第二段尚未创建或仍未获批准

#### Scenario: First slice is rejected
- **WHEN** 第一段被驳回或取消
- **THEN** 系统不再创建下一段

### Requirement: Later slice starts only after the previous slice is done
下一段必须只在上一段完成后创建，并且必须自己停在审批门。

#### Scenario: Previous slice completes
- **WHEN** 当前段进入完成，并且拆分计划还有下一段
- **THEN** 系统创建下一段 M 级子任务，并使它进入等待审批
