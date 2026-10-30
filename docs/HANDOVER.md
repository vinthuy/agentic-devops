# 交接说明（v0.13.0）

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
agentdev cost forecast [--repo R] [--level S|M|L] [--json] [--accuracy]
agentdev bench --manifest bench/tasks.yaml --repeat 2
agentdev bench --manifest bench/tasks.yaml --shared-home DIR   # 样本跨用例累积，用于校准预测
```

流水线：分级 → 规划 → 编码 → 验证 → 独立审查 → 提交 → 推送 → Draft PR →（可选）CI 监听。

## 2. 已验证

| 项目 | 证据 |
|---|---|
| 构建与静态检查 | `gofmt -l .` 无输出；`go build ./...`、`go vet ./...` 通过 |
| 测试 | 310 个顶层测试函数（含子测试共 352 条 PASS），19 个含测试的包全部 `ok`（共 21 个包，另 2 个无测试文件） |
| 真实模型端到端 | DeepSeek 完成 邮箱/slug/CSV/字典 等任务，Reviewer 返回 approve |
| 基准集 | 24 个用例（S 级 20：Python 10 / Go 7 / JS 3；M 级 4）：修复前均确认失败。S 级重复采样 38/40 = 95%，平均 $0.0105/次；ADR-31 回归轮 20/20、ADR-33 回归轮 20/20（均 $0.0095/次，SDD §22.3）。M 级 4/4 通过（合成审批，$0.0080～$0.0205，SDD §22.7），其中 `py-session-refresh` 是唯一**跨两文件**改动，真实模型同时改对 `clock.py` 与 `session.py` |
| **审批门（放行 / 驳回 / 回退）** | 真实 DeepSeek 端到端（SDD §22.8，ADR-39）：M 级任务停在 `AWAITING_APPROVAL` 且后端未被调用；`task reject --reason`（含 `--by`）→ `REJECTED`，工作区一个字节未改、任务不再被取走；`task reject --replan` → 回 `PLANNING`，重新规划后**再次**停在审批门；`task approve` → `DONE`，提交 `bd7f6d4` 真把 `a - b` 改成 `a + b`，`main` 未动。驳回理由确实进了规划器（新计划摘要吸收了「确认 test_core.py 的期望」） |
| **审批者能看到计划** | ADR-41：`task status` 与 `report.md` 渲染摘要/步骤（含文件）/`allowed_paths`/验收标准/风险/预估花费。真实输出纠正了一个误报——`step2「运行 python -m unittest test_core.py」` 把测试文件列进 `files`，第一版据此报「计划要改测试」，而该文件不在 `allowed_paths` 内、写它会被策略拦下。改为按 `allowed_paths` 划界，只报「策略拦不住、只有人能拦」的情形；`estimated_cost_usd`（模型自估，实测把预算 $1.00 当预估，真实 $0.0045）改为标注来源 |
| **审批超时** | ADR-40：超时升级到 `NEEDS_HUMAN`（不自动放行/驳回），`resume` 可送回审批门并重新计时；7 个确定性测试覆盖（`TestRunEscalatesStaleApproval` 等），时钟通过回拨 `updated_at` 模拟，不 sleep |
| 崩溃恢复 | 真实子进程在模型调用中途被 kill，重启后完成且只产生 1 个提交 |
| 策略 | 路径穿越、符号链接逃逸、敏感路径、危险命令、`-exec` 类追加参数均被拒绝 |
| 成本 | 逐次模型调用记账、80% 预警、100% 熔断；缺单价显式告警 |
| 真实 Draft PR | 在本仓库上跑通：PR #1（后因需求前提不成立而关闭，见 SDD §18），仅改目标文件、Draft、正文含验收矩阵与成本、Reviewer approve、`master` 未被改动 |
| 无 CI 时的处理 | `gh pr checks` 无检查时记录 `ci: none` 后完成，不伪装成通过 |
| 交付（CI 回修） | 密钥扫描、分支推送、幂等 PR 更新由 Fake/替身覆盖；红 CI 回修已在真实仓库验证（见下条） |
| **红 CI → 自动修复闭环** | `vinthuy/agentdev-sandbox` 上真实跑通并**扩样到 6 次**（SDD §23.9）：首次 CI 失败（`test_edges.py` 的 None/bool 边界）→ `fix_requested{source: ci}` 携带 CI 日志尾部 → 回修 → **同一个 PR 更新** → 第二次 CI 绿 → `DONE`，每次 `ci_attempts=1`。剧本（`bench/ci_loop.yaml`）在 v0.7.0 上只有 2/6 走通，暴露两个链路缺陷（日志按头部截断、Reviewer 要求回退 CI 要求的改动）；修复后 6/6，平均 $0.0212 |
| **本仓库 CI（双 OS）** | `.github/workflows/ci.yml` 已启用：run [40083639699](https://github.com/vinthuy/agentic-devops/actions/runs/40083639699) 的 `test (ubuntu-latest)`、`test (windows-latest)`、`benchmark manifest` 全绿。18 个包在 Linux 上全部通过 |
| CI 首跑发现的问题 | Windows runner 以 CRLF 检出导致 `gofmt -l` 误报全部 40 个 `.go`；加 `.gitattributes`（`* text=auto eol=lf`）后修复（ADR-29） |
| **CI 首次通过率（PRD ≥50%）** | `bench --ci-repo vinthuy/agentdev-sandbox` 让 20 个基准用例各开一个真实草稿 PR 并等待真实 CI：**20/20 = 100%**，`ci_none=0`，平均 $0.00825/次，总 $0.165（SDD §22.6）。口径：CI 与本地验证跑同一批命令 |
| **需求前提核查** | 需求声称“某测试缺失/需补充”时，规划前在测试文件里查同名引用，并把命中位置与文件内容注入规划器（ADR-31）。用 PR #1 的原始需求实测：24 秒、$0.0057 停在 `NEEDS_INFO`，明确指出 `cost_test.go:12-14` 已覆盖；无证据注入的版本会产出重复测试（$0.0190） |
| **实现前提核查** | 需求对现有实现提主张（“`parse_port` 没处理空字符串”“改为按百万计价”）时，在非测试源码里查被点名符号，注入定义位置与命中行前后的带行号窗口（ADR-33）。在 `parse.py` 已有 `if not s: raise ValueError` 的仓库上实测：需求声称“没有校验空字符串”，两次运行都停在 `NEEDS_INFO` 并精确指出 `parse.py:9-10`（$0.0021、$0.0015，约 11 秒）；未含该功能的二进制只指出测试重复，未识别实现前提 |
| **花费预测** | `agentdev cost forecast [--repo] [--level]` 按历史实际花费给出 P50/P90（ADR-32）；样本不足 5 条时拒绝给数字。计划阶段写 `cost_forecast` 事件，`--accuracy` 校准（SDD §5.10） |
| **花费预测的真实校准** | 用 `bench --shared-home` 让基准用例共用数据目录累积样本：20 个真实用例里有 15 个记录了预测，实测 **11/15 = 73% 落在 P90 内**，低于名义 90%。根因是线性插值在小样本下位次偏低（第 k 个顺序统计量的期望覆盖率是 `k/(n+1)`），改用 `ceil((n+1)p)` 后新跑 7 个用例，**6/7 = 86%**；存储的 P90 与新算法精确吻合，确认走的是新代码（SDD §5.10） |

## 3. 未验证（重要）

| 项目 | 原因 | 需要什么 |
|---|---|---|
| M3 设计（§24，ADR-42 至 ADR-49） | 只写在 PRD / SDD 里，`v0.13.0` 没有对应命令和状态 | 按 §24 实现后再改「未实现」声明；在此之前不要把这些 FR 当成已有能力 |
| 基准统计显著性 | 20 用例 × 1 次运行（成功率与 CI 首次通过率都是单样本） | `--repeat N` 多轮采样，或更多仓库 |
| 前提核查的覆盖面 | 已覆盖“测试缺失”与“现有实现主张（带命名符号）”两类；且都依赖关键词触发，换个说法可能不触发 | 描述性前提（“登录流程在并发下会丢状态”）既定位不到符号也无从核查；需要模型驱动的只读检索工具 |
| 花费预测的准确度 | 已测出 73%（线性插值）→ 86%（顺序统计量，7 个新样本），**但仍不能说已校准到名义 90%**：7 个点的 95% 置信区间约 42%～99%；且样本等于下限 5 时 P90 必然等于最大值 | 累积到 50 条以上真实样本后重跑 `cost forecast --accuracy` |
| CI 的额外发现能力 | 基准里 CI 与本地验证跑同一批命令，因此 100% 只说明“环境没有翻车” | 像 `bench/sandbox/` 那样让 CI 跑本地不跑的检查，再统计 |
| 非 ubuntu 的 Linux 发行版 | 仅覆盖 `ubuntu-latest` | 目标发行版或容器内复跑 |
| Docker 隔离 | 你已明确不作为需求；默认使用受限本地进程 | 仅在需要 OS 级隔离时再排期 |

## 4. 已知限制

- 本地进程不是 OS 级沙箱：限制来自 worktree、路径/命令策略、超时与密钥扫描，不是内核隔离。
- Reviewer 判断质量依赖模型；只在四类问题（需求/正确性/安全/验证削弱）上阻塞，风格意见不阻塞。
- 审批门**默认不会超时**：`approval_timeout_minutes` 默认为 0（一直等）。开启后超时只升级到
  `NEEDS_HUMAN`（`resume` 可送回审批门），不会自动放行或自动驳回；需要自行定时跑
  `agentdev run` 或任何会调 `run` 的命令来触发检查（没有常驻的清理进程）。
- 规划器可能写出不可执行的验收标准（错误选择器、过窄 grep）。系统会识别并回到规划一次，仍失败则转人工，但会消耗时间与少量费用。
- 规划器还可能写出**把实现写法钉死**的验收标准（断言 `x = now()` 这类赋值右侧）。这类断言不会触发「不可执行」，而是让一个语义正确的实现失败、白跑一轮修复（SDD §22.7 有实测）。已在规划提示词里加了具体反例，但**只能靠模型遵守，无法用测试钉住**：计划校验硬拒会降级成静态计划，判成 `Invalid` 又会把可用例推向转人工（ADR-38）。
- `locks` 表为多进程部署预留，当前单进程实现不使用。
- 真实 GitHub 操作依赖 `gh` 登录状态；agentdev 自身不保存 GitHub 凭据。

## 5. 版本基线

- 代码版本：`v0.13.0`（`internal/cli.Version`）
- `v0.13.0` 相对 `v0.12.0` 的实质变化：
  - **审批者终于看得到计划**（§22.8，ADR-41）：`plan_json` 此前是只写不读的数据，
    `task status` 只有需求一句话。现在渲染摘要/步骤/涉及文件/`allowed_paths`/
    验收标准/风险标记/预估花费，`report.md` 也新增「计划」章节可复盘。
  - **风险提示以 `allowed_paths` 划界**：只有落在可写范围内的验证配置文件/敏感路径
    才报警——那是策略拦不住、只有人能拦的部分。第一版按 `steps[].files` 一律报，
    被真实输出证伪（「运行 test_core.py」被误报成「要改测试」）。
  - 模型自估的 `estimated_cost_usd` 标注为「未经校准」：实测出现过填预算 $1.00
    而真实花费 $0.0045 的情况。
- `v0.12.0` 相对 `v0.11.0` 的实质变化：
  - **审批超时升级**（§22.8，ADR-40）：`policy.yaml` 新增 `approval_timeout_minutes`
    （默认 0 = 不超时）。超过阈值升级到 `NEEDS_HUMAN` 并记录 `approval_timeout` 事件，
    **不自动放行也不自动驳回**；`task resume` 可把它送回审批门并重新计时
    （为此单独放开 `NEEDS_HUMAN → AWAITING_APPROVAL` 这条边）。
    注意：检查是在 `run` 入口与 `RunTask` 里做的，没有常驻清理进程——不跑 `run`
    就不会触发。
- `v0.11.0` 相对 `v0.10.0` 的实质变化：
  - **补上审批门的拒绝分支**（§22.8，ADR-39）：`AwaitingApproval` 此前只有
    `→ CODING` 一条出边，CLI 也只有 `task approve`——「审批」实际退化成「通知」。
    新增 `task reject --reason R`（终止，`REJECTED`）与 `task reject --reason R --replan`
    （退回 `PLANNING` 重做，理由并入需求）。`--reason` 必填。
  - **审批门可见性**：`approved_by` 此前只写进事件负载、任何渲染路径都不读，
    现在时间线显示「审批人/驳回人」；`task list` 增加 `等待` 列，挂起任务的
    等待时长可见。
  - 用真实模型把整条门跑通：停 → 驳回 → 回退重规划 → 再停 → 放行 → `DONE`，
    并核对提交内容确实是真修。
- `v0.10.0` 相对 `v0.9.0` 的实质变化：
  - **基准集补上跨文件 M 级用例**（§22.2、§22.7）：新增 `py-session-refresh`，
    清单从 23 增到 24。此前 3 个 M 用例都是单函数改动，只检验「审批门之后流水线
    能不能跑完」，不检验规划质量。这一例把过期语义拆到 `clock.py` 与 `session.py`，
    只改一个文件不可能全绿——修好 `get` 的过期检查会让 `refresh` 的缺陷以新的
    失败形式暴露出来。真实模型一次跑通，两个文件都改对。
  - **修正规划提示词**（§22.7、ADR-38）：该用例第一次运行时测试全过却被判未满足，
    因为规划器写了 `grep` 断言 `session\.issued_at = now\(\)`，把赋值右侧钉死；
    编码器用局部变量复用时间（`= current`）语义正确却匹配不到，白跑一轮修复。
    提示词里加了具体反例，要求只断言标识符/字面量、行为交给 `test`。
    对照：修复轮数 1 → 0，花费 $0.0289 → $0.0169（各 1 次，非受控实验）。
- `v0.9.0` 相对 `v0.8.0` 的实质变化：
  - **基准集补上 M 级**（§22.7，ADR-37）：20 个 S 级用例连续两轮 100%，基准已饱和，
    只能防灾难性回归、不能防质量退化；而 M 级一直被结构性排除——bench 只调用一次
    `RunTask`，任务停在 `AWAITING_APPROVAL` 就记为失败，于是所有用例措辞都写得很「安全」。
  - 新增 `bench --auto-approve`：M 级任务用**合成审批**越过审批门继续执行。
    结果里强制带 `synthetic_approval`，表格显示为 `PASS*`——把「绕开了门」说成
    「通过了门」正是 ADR-22/ADR-36 反复要避免的失真。
  - 新增 3 个 M 级用例（`py-dependency-version`、`py-token-expiry`、`go-auth-header`），
    清单从 20 增到 23；`TestCommittedManifestIsValid` 现在要求至少 3 个 M 级用例，
    防止基准悄悄退回全 S。
  - 实测：3/3 通过，$0.0080～$0.0205，36～61 秒，均为 0 次修复、0 次审查打回。
    人工核对确认是真修（`>`→`>=`、剥离 `v` 前缀、`EqualFold`+`TrimSpace`），
    测试文件与 fixture 逐字节一致。
  - **注意**：M 级的「M」来自需求措辞而非真实风险；审批门本身没有被验证。
- `v0.8.0` 相对 `v0.7.0` 的实质变化：
  - **红 CI 闭环从 N=1 扩到 6 次重复运行**（§23.9，新增 `bench/ci_loop.yaml` 与
    `bench/ci_loop_workflow.yml`）。扩样把通过率从 2/6 拉到 **6/6**，代价是暴露两个
    与模型能力无关的链路缺陷：
  - CI 失败文本改为**保留日志尾部**（预算 4000 字符）。原先按头部截断，GitHub Actions
    的 workflow 样板把断言挤掉，编码步骤因此判定「代码已正确、无需修改」（ADR-34）
  - 审查上下文新增 `<ci_failure trusted="true">`：当本轮改动在修 CI 失败时注入那次失败，
    Reviewer 不再把 CI 要求的改动判成「超范围」并回退（ADR-34）
  - `git push` 对传输层抖动重试 3 次，认证/权限/分支保护等错误立即失败（ADR-35）
  - 基准测量：基础设施故障的运行标记 `blocked` 并排除出成功率分母（ADR-36）；
    基线分支改为强制推送，避免崩溃残留的远端分支阻断后续运行
  - 新增 `internal/delivery/network_retry_test.go`、`internal/orchestrator/ci_evidence_test.go`、
    `internal/orchestrator/ci_failure_text_test.go`、`internal/bench/infra_blocked_test.go`
- `v0.7.0` 相对 `v0.6.0` 的实质变化：
  - 前提核查扩展到**现有实现主张**（ADR-33）：需求说“某函数没处理 X”“改为 Y”时，
    规划前在非测试源码里定位被点名符号，注入定义位置与带行号的代码窗口。
    实测在“已有空串校验”的仓库上两次都停在 `NEEDS_INFO` 并指出 `parse.py:9-10`（§5.9b）
  - 新增 `internal/planning/implementation.go`（含定义置信度排序与片段窗口提取）
  - 修正 SDD §21.1 的过期基线（229 函数 / 18 包 → 275 / 21）
- `v0.6.0` 相对 `v0.5.0` 的实质变化：
  - 花费预测的 P90 改用顺序统计量 `ceil((n+1)p)`，替代线性插值：真实校准显示插值版
    只覆盖 73%（名义 90%），改后新跑 7 个用例覆盖 6 个（ADR-32 修订，§5.10）
  - `bench --shared-home <dir>`：让基准用例共用一个数据目录，使花费样本跨用例累积，
    为 `cost forecast --accuracy` 提供真实校准数据；默认仍是用例独立目录
- `v0.5.0` 相对 `v0.4.0` 的实质变化：
  - 新增花费预测：`agentdev cost forecast [--repo] [--level] [--json] [--accuracy]`，
    报 P50/P90，样本不足 5 条时拒绝给数字；计划阶段记录 `cost_forecast` 事件（ADR-32，§5.10）
  - 新增 `internal/forecast` 包（纯计算，分位数与筛选分层有独立测试）
- `v0.4.0` 相对 `v0.3.0` 的实质变化：
  - 新增需求前提核查（ADR-31）：需求声称“测试缺失/需补充”时，规划前把现有测试中同名
    引用的位置与文件内容注入规划器；前提不成立时停在 `NEEDS_INFO` 而不是产出重复改动。
    全量基准回归 20/20 = 100%，平均 $0.0095（§22.3 第四轮）
- `v0.3.0` 相对 `v0.2.0` 的实质变化（均为已验证项，见 §2）：
  - 红 CI → 自动修复闭环真实跑通，并修掉两个交付缺陷：CI 失败日志回传、PR 更新复用（§2）
  - 仓库 CI 启用（ubuntu + windows 双跑），新增 `.gitattributes` 固定 LF 检出（ADR-29）
  - `agentdev bench --ci-repo`：基准集接入真实 CI，测量 CI 首次通过率（ADR-30 + `ci: none` 宽限期修复）
- 本仓库 CI 已启用为 `.github/workflows/ci.yml`（提交 `f55725d`），配套提交 `da3a5b7` 增加 `.gitattributes`。
  `ci/github-workflow.yml.template` 保留作为分发模板，内容与启用后的文件一致。
- 推送 `.github/workflows/*` 需要 `gh` 令牌具备 `workflow` scope：
  `gh auth refresh -h github.com -s workflow`。
- 回退方式：`git checkout v0.9.0`。
  **不要回退到 v0.8.0 以下**：会丢掉 M 级用例与 `--auto-approve`，基准重新退回全 S
  饱和状态（连续两轮 100%，测不出质量退化）。
  **不要回退到 v0.7.0 以下**：会丢掉 CI 失败日志的尾部保留与审查上下文注入（ADR-34），
  红 CI 闭环会从 6/6 掉回 2/6——失败的形态是「编码步骤说无需修改」或「Reviewer 要求
  回退 CI 要求的改动」，两者都不会报错，只会悄悄转人工。
  **不要回退到 v0.5.0 以下的预测实现**：会丢掉 P90 的顺序统计量修正（预测覆盖率从 80% 掉回 73%）。
  **不要回退到 v0.6.0 以下**：会丢掉实现前提核查（ADR-33），
  “某函数没处理 X”这类需求会重新变成盲猜。
  **不要回退到 v0.2.0**：会丢掉 ADR-29（CRLF 检出导致 Windows CI 全红）、ADR-30
  （`ci: none` 抢跑污染 CI 首次通过率）与 `--ci-repo`；若确实要回退，需同时禁用仓库 CI
  的 windows job。
