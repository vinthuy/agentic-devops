# agentic-devops

需求到 PR 的自动化工程流水线。设计文档见 `docs/`：

- `docs/PRD.md`：产品需求
- `docs/SDD.md`：系统设计
- `docs/IMPLEMENTATION_PLAN.md`：本轮实施方案

## 当前状态（M0 原型）

已实现：状态机、SQLite 持久化（乐观锁）、策略引擎、工具运行时、git worktree 工作区、
验证命令执行、防作弊检查、Anthropic 后端（工具调用循环）、编排器、运行报告、CLI。

未实现（见实施方案 1.2）：Planner 模型计划、模型分级、M 级审批、推送与 PR、CI 监听、
Docker 沙箱、成本与预算熔断。

## 快速开始

```bash
pip install -e ".[dev]"

# 数据目录默认为 ~/.agentdev，可用 AGENTDEV_HOME 或 --home 指定
mkdir ~/.agentdev
cp config/repos.example.yaml ~/.agentdev/repos.yaml     # 修改 local_path 与验证命令
cp config/policy.example.yaml ~/.agentdev/policy.yaml

agentdev doctor
agentdev config validate
agentdev task create --repo acme/web-app --text "修复登录页邮箱含 + 号时校验失败"
agentdev run --backend anthropic     # 需要 ANTHROPIC_API_KEY 与 anthropic 依赖
agentdev task status <task-id>
```

运行结果位于 `~/.agentdev/runs/<task-id>/report.md`。任务的修改位于
`agent/<task-id>-<slug>` 分支（worktree 在 `~/.agentdev/workspaces/`），
本轮不会推送到远程。

## 测试

```bash
pytest
```

测试使用临时 git 仓库与脚本化的 FakeBackend，不消耗模型费用。

## 目录

```
src/agentdev/   核心代码
tests/          unit / component / integration
config/         配置模板
docs/           PRD、SDD、实施方案
```
