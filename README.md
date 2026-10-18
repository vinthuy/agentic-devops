# agentic-devops

需求到 PR 的自动化工程流水线。设计文档见 `docs/`：

- `docs/PRD.md`：产品需求
- `docs/SDD.md`：系统设计（技术选型见第 3 节，决策见第 17 节）
- `docs/IMPLEMENTATION_PLAN.md`：实施方案

## 技术栈

Go 1.25+。已从 Python 原型迁移（见 SDD ADR-11）；Python 原型已删除，历史见 git 提交 `05f080c` 及之前。

## 当前状态（M0 原型）

已实现：状态机、SQLite 持久化（乐观锁）、策略引擎、工具运行时、git worktree 工作区、
验证命令执行、防作弊检查、Anthropic 协议后端（可指向 DeepSeek 等兼容端点）、
编排器、运行报告、CLI。

未实现（见实施方案 1.2）：Planner 模型计划、模型分级、M 级审批、推送与 PR、CI 监听、
Docker 沙箱、成本与预算熔断。

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
cp config/models.example.yaml $AGENTDEV_HOME/models.yaml   # 默认即 DeepSeek 配置

export DEEPSEEK_API_KEY=sk-...                # Key 只从环境变量读取，不写入文件

agentdev doctor
agentdev config validate
agentdev task create --repo acme/web-app --text "修复登录页邮箱含 + 号时校验失败"
agentdev run                                  # 默认 --backend anthropic（协议），指向 DeepSeek
agentdev task status <task-id>
```

`--home` 是全局参数，可以放在任何子命令之后。

运行结果位于 `$AGENTDEV_HOME/runs/<task-id>/report.md`。任务的修改位于
`agent/<task-id>-<slug>` 分支（worktree 在 `$AGENTDEV_HOME/workspaces/`），
本轮不会推送到远程。

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
  verify/           验证命令与防作弊检查
  backend/          执行后端（Fake、Anthropic 协议）
  orchestrator/     任务主流程
  report/           运行报告
  testutil/         测试用临时 git 仓库
config/             配置模板
docs/               PRD、SDD、实施方案
```
