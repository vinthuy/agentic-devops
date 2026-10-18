# agentic-devops

需求到 PR 的自动化工程流水线。设计文档见 `docs/`：

- `docs/PRD.md`：产品需求
- `docs/SDD.md`：系统设计（技术选型见第 3 节，决策见第 17 节）
- `docs/IMPLEMENTATION_PLAN.md`：实施方案

## 技术栈

Go 1.25+。已从 Python 原型迁移（见 SDD ADR-11）；Python 原型已删除，历史见 git 提交 `05f080c` 及之前。

## 当前状态（M2 部分实现）

已实现：状态机、SQLite 持久化（乐观锁）、策略引擎、工具运行时、git worktree 工作区、
验证命令执行、防作弊检查、Anthropic 协议后端（可指向 DeepSeek 等兼容端点）、
**结构化计划与机器可校验的验收标准**、**规则+模型分级**、**M 级审批**、**成本核算与预算熔断**、
**密钥扫描 + 推送 + 草稿 PR**、**中断恢复**、运行报告、CLI。

M2 已实现：独立 Reviewer、`REVIEWING` 状态、可选 `CI_WATCHING` 与红 CI 自动修复、
GitHub Issue 导入、成本看板、编码时 `request_info` 澄清闭环。Docker 沙箱是可选增强，
不是本地开发的前置条件；没有 Docker daemon 时仍可使用受限本地进程执行。多租户与并发调度不在 M2 范围。

## 构建与测试

```bash
go build -o agentdev ./cmd/agentdev
go test ./...
```

## 快速开始（DeepSeek）

```bash
export AGENTDEV_HOME=~/.agentdev              # 数据目录，也可用 --home 指定
mkdir -p $AGENTDEV_HOME
cp config/repos.example.yaml  $AGENTDEV_HOME/repos.yaml    # 修改 local_path 与验证命令
cp config/policy.example.yaml $AGENTDEV_HOME/policy.yaml
cp config/models.example.yaml $AGENTDEV_HOME/models.yaml   # DeepSeek 配置与单价
cp config/config.example.yaml $AGENTDEV_HOME/config.yaml   # 交付 / 分级 / 规划开关

export DEEPSEEK_API_KEY=sk-...                # Key 只从环境变量读取，不写入文件

agentdev doctor
agentdev config validate
agentdev task create --repo acme/web-app --text "修复登录页邮箱含 + 号时校验失败"
agentdev task create --repo acme/web-app --issue 123  # 需 gh 登录；从 GitHub 导入 Issue 正文
agentdev run                                  # 默认 --backend anthropic（协议），指向 DeepSeek
agentdev task status <task-id>
agentdev cost <task-id>                       # 花费明细
agentdev cost report                          # 全局成本看板
```

`--home` 是全局参数，可以放在任何子命令之后。

运行结果位于 `$AGENTDEV_HOME/runs/<task-id>/report.md`（含验证记录、验收标准、成本明细、
事件时间线）。任务的修改位于 `agent/<task-id>-<slug>` 分支，worktree 在
`$AGENTDEV_HOME/workspaces/`。

### 分级与人工介入

- `--level` 缺省时自动分级：命中敏感区域/大范围改动的需求会升级为 M 或 L。
  L 级直接拒绝，M 级停在 `AWAITING_APPROVAL`。
- 需求信息不足时停在 `NEEDS_INFO` 并列出澄清问题。
- GitHub Issue 正文按不可信输入包裹；自动导入只读取 Issue，不会自动关闭或评论 Issue。
- 验证通过后 Reviewer 只因需求、正确性、安全或测试完整性问题打回，不因风格偏好阻塞。
- CI 监听通过 `delivery.ci_watch: true` 显式开启；默认关闭。默认间隔 60 秒、超时 30 分钟、失败最多自动修复 2 次。
- 需要人工介入的命令（退出码 3 表示需要人工处理）：

```bash
agentdev task approve <id> --by 张三        # 审批 M 级任务
agentdev task reply   <id> --text "只改前端" # 回答澄清问题
agentdev task resume  <id>                  # 恢复 NEEDS_HUMAN（会重置修复次数）
agentdev task cancel  <id> --reason "作废"   # 取消
agentdev run --task <id>                    # 继续推进指定任务
agentdev run --max 5                        # 一次连续推进多个任务
```

### 成本与预算

`models.yaml` 的 `pricing` 按“每百万 token”配置单价；预算取 `repos.yaml` 的 `budget_usd`
（或 `task create --budget`）。达到预算的 80% 记一次预警事件，达到 100% 立即停止调用模型
并转 `NEEDS_HUMAN`。未配置单价的模型按 $0 记账，但会显式记录事件并在报告中提示，
不会把“未知”当成“免费”。

### 推送与草稿 PR

默认关闭。确认本地流程可用后再开启：

```yaml
# config.yaml
delivery:
  enabled: true
  branch_prefix: "agent/"
  draft_pr: true     # 强烈建议保持 true
```

开启后 `COMMITTING` 阶段会：密钥扫描 → 推送分支 → `gh pr create --draft`。
密钥扫描命中会**阻止推送**并挂起等待人工处理。推送使用当前 git 凭据，
创建 PR 使用已登录的 `gh` CLI——本项目不保存任何 token。
也可以只对单次运行强制开启：`agentdev run --push`。

## 目录

```
cmd/agentdev/       程序入口
internal/
  cli/              命令行（cobra）
  config/           配置加载与校验
  state/            状态机与转换表
  store/            Repository 接口与 SQLite 实现
  policy/           路径与命令策略
  tools/            模型可调用的工具
  workspace/        git worktree、diff、提交
  verify/           验证命令、验收标准求值、防作弊检查
  llm/              Anthropic Messages 协议客户端
  cost/             单价表与费用换算
  triage/           分级（规则 + 模型估算）
  planning/         结构化计划与验收标准
  delivery/         密钥扫描、推送、草稿 PR
  backend/          执行后端（Fake、Anthropic 协议）
  orchestrator/     状态机驱动器
  report/           运行报告
  testutil/         测试用临时 git 仓库
config/             配置模板
docs/               PRD、SDD、实施方案
```
