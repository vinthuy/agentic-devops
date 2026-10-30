# Proposal

## Why

审批、审查和超时都停在本机 CLI 上，而提需求的人在 GitHub Issue 里。规划又无法处理没有点名符号的需求，L 级只能整单拒绝。这些缺口要在不把 GitHub 或仓库说明变成第二条编排器的前提下补上。

## What Changes

- Issue 评论中仅 `/approve`、`/reject`、`/replan` 映射为现有审批命令；计划通知与完成摘要回写到同一 Issue。
- 独立审查的结论在状态转移之后写到草稿 PR 的 review 上。发表失败不回滚状态。
- `agentdev sweep` 只处理到期审批和锁，不领取新任务，进程内不常驻。
- 规划前增加有界只读检索。证据不足时必须停在待澄清，不得编造文件。
- 分级、规划、编码、审查可以各选模型。未指定的角色使用默认模型。用到的模型必须有单价。
- 仓库根目录的 `AGENTS.md` 注入规划与审查，不能放宽路径、命令或预算。
- L 级拆成按顺序审批的 M 级子任务。批准一段不会批准下一段。父任务不编码。
- 用来衡量 CI 额外发现能力的基准，必须包含一条本地验证不跑的检查。

## Capabilities

### New Capabilities

- `issue-approval`: 从来源 Issue 批准、驳回、退回重规划，并回写计划与最终结果
- `pr-review-sync`: 把已经生效的审查结论发表到草稿 PR
- `approval-sweep`: 一次性处理到期审批和锁
- `planning-retrieval`: 规划前的有界只读检索
- `role-models`: 按角色选择模型并保持单价约束
- `repo-instructions`: 注入 `AGENTS.md` 且不让它改写策略
- `level-split`: 把 L 级拆成逐段审批的 M 级子任务
- `ci-differential-bench`: 基准中用一条本地不跑的检查衡量 CI

### Modified Capabilities

- 无。仓库里还没有 `openspec/specs/`，现有行为仍以 `docs/PRD.md` 与代码为准。

## Impact

- CLI：新增 `sweep`；`run` 的到期处理必须与 `sweep` 共用同一函数。
- GitHub：`gh` 读评论、识别协作者、发表 Issue 评论和 PR review。
- 配置：`models.yaml` 增加可选 `roles`。
- 数据：`tasks` 增加可空列 `parent_task_id`。
- 规划与审查提示上下文：检索摘录和 `AGENTS.md`。
- 基准剧本：新增一条只存在于 workflow 的检查。
- 不改变：不自动合并，不做 Web 后台，不在进程内常驻定时器，不支持按角色使用不同厂商的 API key。
