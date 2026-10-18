# agentic-devops 实施计划与当前状态

| 项目 | 内容 |
|---|---|
| 文档版本 | v0.3 |
| 对齐代码 | Go 实现，M2 功能子集 |
| 关联文档 | `docs/PRD.md`、`docs/SDD.md` |
| 更新日期 | 2026-10-18 |

---

## 1. 产品与实现基线

agentic-devops 是本地/自托管的单机需求到 Draft PR 流水线。生产实现为 Go；运行数据写入 `AGENTDEV_HOME` 下的 SQLite，代码修改隔离在 Git worktree 中。

默认执行方式是经路径/命令策略检查的本地子进程。Docker 是可选增强隔离，不是本地开发、构建或任务执行的前置条件。用户没有 Docker daemon 时，普通任务照常运行。

当前范围：M1 流程完整；M2 的 Reviewer、CI 闭环、Issue 导入、成本看板和 `request_info` 已实现。PRD 的真实 GitHub CI 首次通过率目标还未在基准集测量；这属于产品指标验证，不阻止本地开发。

---

## 2. 模块地图（以代码为准）

| 包 | 职责 |
|---|---|
| `cmd/agentdev`、`internal/cli` | Cobra CLI、配置/任务管理、`run`、成本明细与看板 |
| `internal/config` | `config.yaml`、`repos.yaml`、`policy.yaml`、`models.yaml` 加载和校验 |
| `internal/state` | 状态枚举、合法转换、终态/挂起状态集合 |
| `internal/store` | SQLite schema、幂等迁移、事务转换、事件、验证、成本和交互持久化 |
| `internal/orchestrator` | 根据当前持久化状态推进一步；处理限额、恢复、审查、交付和 CI |
| `internal/llm` | Anthropic Messages HTTP 客户端、重试、用量解析、兼容端点能力探测 |
| `internal/triage`、`internal/planning`、`internal/review` | 规则/模型分级、结构化计划与验收标准、独立结构化审查 |
| `internal/backend`、`internal/tools` | Coder 工具循环、Fake 后端、命令/文件工具和 `request_info` |
| `internal/policy` | 路径、命令白名单/黑名单、敏感路径策略 |
| `internal/workspace` | Git worktree、diff、提交，以及提交前过滤工具生成产物 |
| `internal/verify` | 执行仓库验证命令、验收标准求值、防作弊检查 |
| `internal/delivery` | 密钥扫描、push、幂等 PR、`gh pr checks` CI 查询及 Fake 实现 |
| `internal/cost` | 单价表、费用计算与预算熔断阈值 |
| `internal/report` | Markdown 报告、来源/降级提示、Reviewer/CI/提交审计区块 |

---

## 3. 当前任务生命周期

```text
QUEUED → TRIAGING → PLANNING → [AWAITING_APPROVAL] → CODING → VERIFYING
                                                           ↑       │
                                                           └─失败───┘
                                                              通过
                                                               ↓
                  NEEDS_INFO ←── request_info           REVIEWING（启用且有模型）
                      │                                      │通过 / request_changes
                   reply → PLANNING                         ↓          └→ CODING
                                                        COMMITTING → PUSHED → PR_OPENED
                                                                            │
                                                     ci_watch=false ────────┴→ DONE
                                                     ci_watch=true → CI_WATCHING
                                                                          ├→ DONE
                                                                          └→ CODING（失败且未超限）
```

- L 级任务拒绝；M 级计划审批后才编码；S 级可自动执行。
- Reviewer 只因需求未满足、正确性、安全或验证被削弱等可指认问题打回；意见必须引用 diff 文件。审查接口失败或 verdict 不合法会转人工，不会伪装成代码问题反复改代码。
- CI 默认不监听（`delivery.ci_watch: false`）。启用后按配置轮询；无 checks 会明确记录为“未运行 CI”，不伪装为通过。
- Review/CI 修复有独立尝试计数；`task resume` 会重置验证、审查和 CI 尝试计数。
- 重跑交付时按分支查已有 PR 并更新，不重复创建。

---

## 4. 配置与 CLI

### 配置文件

- `repos.yaml`：仓库本地路径、默认分支、远端、验证命令、额外命令、敏感路径、预算。
- `policy.yaml`：拒绝路径/命令、每步轮数、验证修复/审查修复次数、命令超时、任务超时。
- `models.yaml`：Messages 协议端点、模型、API key 环境变量、模型单价。
- `config.yaml`：交付、Draft PR、可选 CI 监听、Reviewer 开关、分级和规划开关。

CI 配置示例：

```yaml
delivery:
  enabled: false
  draft_pr: true
  ci_watch: false
  max_ci_attempts: 2
  ci_timeout_minutes: 30
  ci_poll_seconds: 60
review:
  enabled: true
```

### 常用命令

```text
agentdev doctor
agentdev config validate
agentdev task create --repo owner/name --text "..."
agentdev task create --repo owner/name --issue 123 [--issue-repo owner/name]
agentdev run [--task ID] [--max N] [--backend anthropic|fake]
agentdev task list
agentdev task status ID
agentdev task approve ID --by NAME
agentdev task reply ID --text "..."
agentdev task resume ID
agentdev task cancel ID
agentdev cost ID
agentdev cost report
```

Issue 导入仅读取标题与正文，不读取评论、不自动评论或关闭 Issue。正文在分级、规划、编码和 Reviewer 上下文中均标为不可信。

---

## 5. 已实现内容与验证

### M1 基线

- Go CLI、SQLite 状态机、乐观锁与任务事件。
- S/M/L 分级、结构化计划、机器可校验验收标准、M 级人工审批。
- worktree 编码、策略校验、Verifier、防作弊、验证失败修复循环。
- 逐模型调用成本账本、预算预警/熔断。
- commit、密钥扫描、push、Draft PR；默认不合并。
- `NEEDS_INFO`、`AWAITING_APPROVAL`、`NEEDS_HUMAN`、resume/cancel。
- 真实 DeepSeek 端到端暴露并修复 `tool_choice` 不兼容、降级不明显、生成产物被提交、命令拒绝未提示白名单等问题。

### M2 可验证子集

| 项 | 当前实现与验证 |
|---|---|
| Reviewer | 结构化结论与程序校验；Fake 覆盖通过、打回、上限、接口失败；真实 DeepSeek 端到端返回 approve |
| CI 闭环 | 轮询、通过/无 checks/失败/超时、日志尾部脱敏与有界修复；FakeCI 和可注入 GhCli Runner 测试 |
| 幂等 PR | 同分支已有 PR 执行 edit；未找到才 create；鉴权失败不误判成不存在；Runner 测试 |
| Issue 导入 | `gh issue view` 读取标题/正文；Fake Reader 覆盖任务创建、source/source_ref 持久化和需求正文 |
| 成本看板 | 全局角色/模型聚合、高花费任务、缺少单价警告；多任务测试 |
| `request_info` | 编码步骤立即转 `NEEDS_INFO`；与写操作同批出现时先暂停，不先落盘；编排器测试 |
| 数据迁移 | M1 SQLite 库幂等增加 `review_attempts`；旧 schema fixture 测试 |
| Docker | 可选，不作为本地开发目标；默认受限进程执行 |

真实 GitHub push/CI 闭环尚未执行，因为当前工作仓库没有配置目标 remote；不以 Fake 测试宣称线上行为已验证。

---

## 6. 验收与安全不变量

1. `go build ./...`、`go vet ./...`、`go test ./...` 必须通过。
2. 不自动合并；任何推送前必须完成密钥扫描；不推送默认分支。
3. 模型只通过注册工具访问 worktree；命令须通过策略；路径限制由程序强制。
4. 验收命令由程序执行；模型不得自报通过。`grep` 验收必须能区分修复前后。
5. Reviewer 与 CI 的拒绝/失败都有上限；未知 CI 状态绝不视为通过。
6. 任何模型/规划降级要在 CLI 和报告显式展示；未知价格不得显示成“免费”。
7. Issue、CI 日志、工具输出均按不可信数据处理；日志与审查事件中的已知密钥格式需脱敏。
8. commit 前过滤常见生成产物；被排除路径仍留在工作区并进入提交审计报告。

---

## 7. 后续工作（不阻塞本地开发）

1. 配置目标 GitHub 仓库后跑真实 Draft PR 与 CI 失败修复闭环，测量 PRD 的 CI 首次通过率目标（≥50%）。
2. 如产品需要，再增加 Docker `Sandbox` 实现；使用配置明确选择，未安装 Docker 时普通 process 模式照常运行。显式要求 Docker 但不可用时应 fail-fast，不能静默降低隔离级别。
3. 按实际使用再评估按仓库/日期成本筛选、Issue 评论回写、Webhook、并发调度与 Linux 服务部署。
