# pr-review-sync Specification

## Purpose
把已经在本地生效的独立审查结论发表到草稿 PR 上，同时保持本地任务状态为结论真源。

## Requirements

### Requirement: Review publication follows the local decision
系统必须在审查结论已经通过校验且任务状态已经转移之后，才发表 PR review。打回且含阻塞意见时必须请求修改。通过时必须发表评论而不是请求修改。

#### Scenario: Changes are requested locally
- **WHEN** 本地审查结论为打回，并且任务已经回到编码或待人工
- **THEN** 草稿 PR 上的 review 请求修改，每条阻塞意见对应一条评论

#### Scenario: Review is approved locally
- **WHEN** 本地审查结论为通过，并且任务已经继续交付
- **THEN** 草稿 PR 上的 review 是评论，不请求修改

### Requirement: Publication failure preserves local state
PR review 发表失败或当前没有 PR 时，系统必须保持已经做出的本地状态转移。失败必须留下可查询的错误原因。

#### Scenario: GitHub publication fails
- **WHEN** 本地审查状态已经转移，但发表 PR review 失败
- **THEN** 任务状态保持转移后的值，并记录发表失败及错误原因

#### Scenario: No pull request exists
- **WHEN** 审查完成时任务还没有 PR
- **THEN** 系统不发表 review，也不因此把任务标为失败
