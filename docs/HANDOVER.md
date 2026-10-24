# 交接说明（v0.2.0）

本文件记录当前版本**已验证**与**未验证**的边界，避免把单元测试通过当成线上可用。
设计依据见 `docs/PRD.md`、`docs/SDD.md`；验收矩阵在 SDD §21。

## 1. 现在能做什么

```bash
go build -o agentdev ./cmd/agentdev
cp config/*.example.yaml $AGENTDEV_HOME/          # 按需修改 repos/models/policy/config
export DEEPSEEK_API_KEY=sk-...

agentdev doctor
agentdev task create --repo owner/name --text "明确的小改动需求"
agentdev run --max 3 --parallel 2
agentdev task status <id>
agentdev task logs <id> [--limit 20] [--json]
agentdev cost <id>
agentdev cost report
agentdev bench --manifest bench/tasks.yaml --repeat 2
```

流水线：分级 → 规划 → 编码 → 验证 → 独立审查 → 提交 → 推送 → Draft PR →（可选）CI 监听。

## 2. 已验证

| 项目 | 证据 |
|---|---|
| 构建与静态检查 | `gofmt -l .` 无输出；`go build ./...`、`go vet ./...` 通过 |
| 测试 | 224 个测试函数，18 个包全部通过 |
| 真实模型端到端 | DeepSeek 完成 邮箱/slug/CSV/字典 等任务，Reviewer 返回 approve |
| 基准集 | 20 个 S 级用例（Python 10 / Go 7 / JS 3），修复前均确认失败；重复采样 38/40 = 95%，平均 $0.0105/次 |
| 崩溃恢复 | 真实子进程在模型调用中途被 kill，重启后完成且只产生 1 个提交 |
| 策略 | 路径穿越、符号链接逃逸、敏感路径、危险命令、`-exec` 类追加参数均被拒绝 |
| 成本 | 逐次模型调用记账、80% 预警、100% 熔断；缺单价显式告警 |
| 真实 Draft PR | 在本仓库上跑通：PR #1，仅改目标文件、Draft、正文含验收矩阵与成本、Reviewer approve、`master` 未被改动 |
| 无 CI 时的处理 | `gh pr checks` 无检查时记录 `ci: none` 后完成，不伪装成通过 |
| 交付（CI 回修） | 密钥扫描、分支推送、幂等 PR 更新由 Fake/替身覆盖；红 CI 回修已在真实仓库验证（见下条） |
| **红 CI → 自动修复闭环** | `vinthuy/agentdev-sandbox` 上真实跑通：首次 CI 失败（`test_edges.py` 的 None/bool 边界）→ `fix_requested{source: ci}` 携带 CI 日志尾部 → 回修 → **同一个 PR #1** 更新 → 第二次 CI 绿 → `DONE`，`ci_attempts=1`，$0.0267。E2E-1…E2E-6 全部通过（SDD §23.8） |
| **本仓库 CI（双 OS）** | `.github/workflows/ci.yml` 已启用：run [40083639699](https://github.com/vinthuy/agentic-devops/actions/runs/40083639699) 的 `test (ubuntu-latest)`、`test (windows-latest)`、`benchmark manifest` 全绿。18 个包在 Linux 上全部通过 |
| CI 首跑发现的问题 | Windows runner 以 CRLF 检出导致 `gofmt -l` 误报全部 40 个 `.go`；加 `.gitattributes`（`* text=auto eol=lf`）后修复（ADR-29） |

## 3. 未验证（重要）

| 项目 | 原因 | 需要什么 |
|---|---|---|
| CI 首次通过率（PRD 目标 ≥50%） | 基础设施已就绪，但 §22 的 20 个基准用例尚未接真实 CI；沙箱剧本设计为首次必红，不能计入分子 | 把基准集用例接入带 CI 的仓库复跑 |
| 基准统计显著性 | 20 用例 × 少量重复采样 | 更多重复与更多仓库 |
| 非 ubuntu 的 Linux 发行版 | 仅覆盖 `ubuntu-latest` | 目标发行版或容器内复跑 |
| Docker 隔离 | 你已明确不作为需求；默认使用受限本地进程 | 仅在需要 OS 级隔离时再排期 |

## 4. 已知限制

- 本地进程不是 OS 级沙箱：限制来自 worktree、路径/命令策略、超时与密钥扫描，不是内核隔离。
- Reviewer 判断质量依赖模型；只在四类问题（需求/正确性/安全/验证削弱）上阻塞，风格意见不阻塞。
- 规划器可能写出不可执行的验收标准（错误选择器、过窄 grep）。系统会识别并回到规划一次，仍失败则转人工，但会消耗时间与少量费用。
- `locks` 表为多进程部署预留，当前单进程实现不使用。
- 真实 GitHub 操作依赖 `gh` 登录状态；agentdev 自身不保存 GitHub 凭据。

## 5. 版本基线

- 代码版本：`v0.2.0`（`internal/cli.Version`）
- 本仓库 CI 已启用为 `.github/workflows/ci.yml`（提交 `f55725d`），配套提交 `da3a5b7` 增加 `.gitattributes`。
  `ci/github-workflow.yml.template` 保留作为分发模板，内容与启用后的文件一致。
- 推送 `.github/workflows/*` 需要 `gh` 令牌具备 `workflow` scope：
  `gh auth refresh -h github.com -s workflow`。
- 回退方式：`git checkout v0.2.0`。
