# issue-approval Specification

## Purpose
让来源 Issue 上的协作者完成批准、驳回和退回重规划，并在同一 Issue 上看到计划与最终结果。

## Requirements

### Requirement: Slash command maps to an existing approval action
系统必须只把协作者评论的首个词当作审批动作。`/approve` 必须等价于批准。`/reject` 与 `/replan` 必须带有理由，并分别等价于终止驳回和退回规划。评论的其余内容不得进入规划器或编码器。

#### Scenario: Collaborator approves
- **WHEN** 任务处于等待审批，且协作者发表 `/approve`
- **THEN** 任务按现有批准规则离开等待审批，动作记录包含该评论 id 和评论者

#### Scenario: Reject without a reason
- **WHEN** 协作者发表 `/reject` 且理由为空
- **THEN** 任务状态保持不变

#### Scenario: Non-collaborator comments
- **WHEN** 没有仓库协作权限的人发表 `/approve`
- **THEN** 任务状态保持不变，并且 Issue 上回复拒绝原因

### Requirement: Applied comment is idempotent
系统必须记录已应用的 GitHub comment id。再次处理同一个 id 时不得第二次转移状态。

#### Scenario: Duplicate delivery
- **WHEN** 同一条已应用的 `/approve` 评论被再次处理
- **THEN** 任务不发生新的状态转移

### Requirement: Label does not approve
系统必须把指定接入标签只用于创建任务。标签不得表示批准、驳回或退回规划。

#### Scenario: Ready label on an issue
- **WHEN** Issue 被打上接入标签
- **THEN** 系统可以创建任务，但任务不会因此进入编码

### Requirement: Issue receives plan and final result
系统必须在任务等待审批时回写当前计划。任务到达终态后必须再回写一次，内容包含 PR 链接和结果摘要。任一次回写失败都不得改变任务状态。

#### Scenario: Plan comment fails
- **WHEN** 任务进入等待审批，但计划回写失败
- **THEN** 任务仍处于等待审批

#### Scenario: Task reaches a terminal state
- **WHEN** 任务进入完成、拒绝或取消
- **THEN** 来源 Issue 收到一条含结果摘要的评论，且 Issue 不被关闭
