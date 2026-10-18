# agentic-devops 实施方案（Iteration 1）

| 项目 | 内容 |
|---|---|
| 依据 | `docs/PRD.md`、`docs/SDD.md` |
| 本轮目标 | 完成 M0 原型的可运行骨架，并落地 M1 中最关键的基础件（状态机、持久化、策略、工具、验证） |
| 日期 | 2026-10-18 |

---

## 1. 本轮范围

### 1.1 包含

| 模块 | SDD 对应 | 说明 |
|---|---|---|
| 配置加载 | 7.1–7.4 | `config.yaml` / `repos.yaml` / `policy.yaml`，Pydantic 校验 |
| 状态机 | 4.1–4.2 | 状态枚举、合法转换表、非法转换报错 |
| 持久化 | 6.1 | SQLite（WAL），`tasks` / `task_events` 两张表，乐观锁转换 |
| 策略引擎 | 5.4、10.2 | 路径规范化、deny/allowed 路径、命令白名单与黑名单 |
| 工具运行时 | 5.4 | `read_file` / `write_file` / `edit_file` / `list_dir` / `search` / `run_command` |
| 工作区 | 2.1、15 | `git worktree` 创建与清理，生成 diff |
| 验证 | 5.5 | 顺序执行 `verify_commands`，记录结果；防作弊检查的最小版本 |
| 执行后端接口 | 8.2 | `ExecutionBackend` 协议；提供 `FakeBackend`（测试用）和 `AnthropicBackend`（真实调用，SDK 可选） |
| 编排器 | 4、11.2 | 驱动 `QUEUED → … → DONE` 的主流程（本地模式，不推送） |
| CLI | 8.1 | `config validate`、`doctor`、`task create/list/status`、`run --once` |
| 测试 | 13 | 单元测试 + 组件测试（临时 git 仓库）+ 安全测试 |

### 1.2 不包含（延后）

| 项 | 延后到 | 原因 |
|---|---|---|
| Planner 调用模型生成计划 | M1 后续 | 本轮用“需求即计划”的最小形式，先保证执行链路 |
| Triage 的模型估算 | M1 后续 | 本轮用规则 + 人工 `--level` |
| Delivery（push / PR / CI） | M1 后续 | 需要 GitHub 凭证与测试仓库，本轮只到本地提交 |
| 密钥扫描 | M1 后续 | 放在推送前，本轮无推送 |
| Reviewer、CI_WATCHING、Docker 沙箱 | M2 | 按 SDD 里程碑划分 |
| 成本账本与预算熔断 | M1 后续 | 本轮只定义数据表与接口，不接真实计费 |
| 审批 / 澄清（NEEDS_INFO、AWAITING_APPROVAL） | M1 后续 | 状态机中保留定义，流程暂不走 |

### 1.3 对 SDD 的有意偏离（需要记录）

1. **COMMITTING → DONE 的快捷路径**：SDD 中 COMMITTING 之后必须经过 PUSHED。本轮 `delivery.enabled = false` 时直接结束，因此在转换表中增加一条 `COMMITTING → DONE`，并在配置中标注仅用于本地模式。开启推送后该路径不可用。
2. **事件表字段精简**：SDD 的 `task_events` 字段保持不变；`cost_ledger`、`verification_runs`、`policy_violations`、`interactions`、`locks` 本轮只建表不写入，避免空实现逻辑。
3. **数据目录**：默认 `AGENTDEV_HOME` 为 `~/.agentdev`。测试中通过环境变量指向临时目录，避免污染用户数据。

---

## 2. 包结构

```
src/agentdev/
├── __init__.py          版本号
├── __main__.py          python -m agentdev
├── config.py            Pydantic 配置模型 + 加载函数
├── state.py             Status 枚举、TRANSITIONS、can_transition()
├── store.py             SQLite 持久化、乐观锁转换、事件追加
├── policy.py            PolicyEngine：路径与命令检查
├── tools.py             ToolRuntime 与各工具实现
├── workspace.py         git worktree 管理、diff 生成
├── verify.py            验证命令执行与防作弊检查
├── orchestrator.py      任务主流程驱动
├── report.py            运行报告（Markdown）
├── errors.py            异常类型
├── backends/
│   ├── __init__.py
│   ├── base.py          ExecutionBackend 协议、StepContext、StepResult
│   ├── fake.py          脚本化后端（测试用）
│   └── anthropic.py     真实后端（懒加载 anthropic SDK）
└── cli.py               Typer 命令入口

prompts/coder.v1.md      Coder 系统提示词（模板，M0 即落盘）
config/*.example.yaml    配置模板
tests/
├── conftest.py          临时 git 仓库、临时 AGENTDEV_HOME 的 fixture
├── unit/                state / policy / config / tools 的纯逻辑测试
├── component/           store / workspace / verify / orchestrator（真实 git）
└── security/            路径穿越、危险命令、防作弊、推送保护
```

---

## 3. 模块设计要点

### 3.1 config.py

- 模型：`GlobalConfig`、`RepoConfig`、`PolicyConfig`、`ModelsConfig`，`AppConfig` 聚合。
- 加载顺序：`AGENTDEV_HOME/config.yaml` → 其余三个文件，缺失时使用内置默认值（测试可直接构造对象，不依赖文件）。
- 校验：`verify_commands` 不能为空；`local_path` 必须存在且是 git 仓库；`budget_usd > 0`。
- 每个仓库的 `repos` 键为 `owner/name`，与 CLI 参数一致。

### 3.2 state.py

```python
class Status(str, Enum): QUEUED, TRIAGING, REJECTED, PLANNING, NEEDS_INFO, AWAITING_APPROVAL,
                         CODING, VERIFYING, REVIEWING, COMMITTING, PUSHED, PR_OPENED,
                         CI_WATCHING, DONE, NEEDS_HUMAN, FAILED, CANCELLED

TRANSITIONS: dict[Status, frozenset[Status]]   # 合法后继集合
TERMINAL = {REJECTED, DONE, FAILED, CANCELLED}
SUSPENDED = {NEEDS_INFO, AWAITING_APPROVAL, NEEDS_HUMAN}

def can_transition(src, dst) -> bool
def assert_transition(src, dst) -> None   # 非法则抛 InvalidTransition
```

本轮转换表以 SDD 4.2 为准，并加入 `COMMITTING → DONE`（见 1.3）。`REVIEWING`、`CI_WATCHING`、`NEEDS_INFO`、`AWAITING_APPROVAL` 的转换规则已定义，但本轮编排器不会走到。

### 3.3 store.py

- 表：`tasks`、`task_events`（其余表在 `migrations/001_init.sql` 中一并建好）。
- 连接：`sqlite3`，`PRAGMA journal_mode=WAL`，`PRAGMA foreign_keys=ON`。
- 核心方法：
  - `create_task(task) -> Task`
  - `get_task(task_id) -> Task`，`list_tasks(status=None, repo=None) -> list[Task]`
  - `transition(task_id, expected_version, to, event_payload) -> Task`：单事务内校验转换合法性、比较版本号、更新状态与 `version+1`、写入 `state_changed` 事件；版本不符抛 `ConcurrentModification`。
  - `append_event(task_id, type, payload)`
  - `list_events(task_id) -> list[Event]`
- 时间戳统一为 UTC ISO 8601。

### 3.4 policy.py

```python
class PolicyEngine:
    def __init__(self, policy: PolicyConfig, repo: RepoConfig, worktree: Path): ...
    def check_path(self, rel_or_abs: str, *, write: bool) -> Path   # 返回规范化路径，违规抛 PolicyViolation
    def check_command(self, cmd: str) -> None                        # 违规抛 PolicyViolation
```

**路径检查顺序**（与 SDD 5.4 一致）：
1. `Path(worktree, p).resolve()`，消除 `..` 与符号链接
2. 必须位于 `worktree` 内（`is_relative_to`）
3. 相对路径（POSIX 形式）匹配 `deny_paths` → 拒绝
4. 写操作：若 `scope.allowed_paths` 非空，必须匹配其中之一

**命令检查顺序**：
1. 命中 `deny_command_patterns` 任一正则 → 拒绝
2. 若 `allowed_commands` 中存在前缀，命令必须以其中之一开头；`verify_commands` 中的命令本身也自动视为允许
3. 含 `|`、`;`、`&&`、`>` 且目标不在白名单内的组合命令 → 拒绝（M0 的简化规则，后续细化）

**glob 匹配**：使用 `pathlib.PurePosixPath.match` 不支持 `**`，故自行实现 `fnmatch` + `**` 转换；这一点在单元测试中覆盖。

### 3.5 tools.py

```python
class ToolRuntime:
    def __init__(self, policy: PolicyEngine, *, max_output_chars: int): ...
    def schemas(self) -> list[dict]          # Anthropic tool schema 格式
    def execute(self, name: str, args: dict) -> ToolResult   # ToolResult(ok, content, error)
```

- 所有工具先过 `PolicyEngine`；策略违规不抛异常到外层，而是返回 `ToolResult(ok=False, error=...)` 给模型，并写入 `policy_violations`（通过回调 `on_violation` 注入，避免 tools 直接依赖 store）。
- `run_command` 使用 `subprocess.run(cmd, shell=True, cwd=worktree, timeout=..., capture_output=True)`，输出截断到 `max_output_chars`，保留头尾。
- `edit_file`：`old` 必须在文件中唯一出现，否则返回错误。
- `search`：纯 Python 实现（递归 + 正则），忽略 `.git`，结果上限 100 条。

### 3.6 workspace.py

```python
class Workspace:
    def create(self, repo: RepoConfig, task_id: str, slug: str) -> WorkspaceInfo   # worktree + 分支
    def diff(self) -> str                 # git diff（相对于基线 commit）
    def commit(self, message: str) -> str | None   # 有改动则提交，返回 SHA
    def remove(self, keep_branch: bool = True) -> None
```

- 分支名：`agent/<task-id>-<slug>`（与 SDD 5.6 一致；本地模式也使用此命名，便于后续直接推送）。
- worktree 路径：`AGENTDEV_HOME/workspaces/<owner>__<name>/<task-id>`。
- 基线：创建时记录 `base_sha = git rev-parse <default_branch>`，diff 以此为基准。
- 使用 `git -C <path> ...` 调用，不依赖工作目录。

### 3.7 verify.py

```python
@dataclass
class CommandResult: name, command, exit_code, duration_s, output_path

class Verifier:
    def run(self, worktree: Path, commands: dict[str, str], timeout_s: int) -> VerificationReport
```

- 顺序执行，任一失败即停止（配置项 `fail_fast`，默认 True）。
- 完整输出写入 `runs/<task-id>/artifacts/verify-<name>.log`，报告中只保留摘要。
- 防作弊（最小版）：`anticheat.check(diff, verify_config_files)`：
  - diff 修改了 `verify_config_files` 匹配的文件 → 标记为可疑
  - diff 新增 `skip` / `xfail` 关键字，且基线中没有 → 标记为可疑
  - 结果为 `list[Finding]`；有 Finding 则任务转 `NEEDS_HUMAN`

### 3.8 backends

```python
@dataclass
class StepContext:
    task_id: str
    requirement: str
    plan: str                   # M0 为需求原文
    failure_summary: str | None
    worktree: Path
    tools: ToolRuntime
    max_turns: int

@dataclass
class StepResult:
    final_text: str
    turns: int
    usage: Usage                # input/output tokens，供成本模块使用

class ExecutionBackend(Protocol):
    name: str
    def run_step(self, ctx: StepContext) -> StepResult: ...
```

- `FakeBackend(script)`：`script` 为预设的工具调用序列，例如 `[("write_file", {...}), ("run_command", {...})]`，用于完整链路测试。
- `AnthropicBackend(model, api_key)`：实现工具循环（参见 SDD 5.3）。`anthropic` 包为可选依赖，在 `__init__` 中懒加载导入，缺失时给出清晰的安装提示。本轮不做真实 API 测试，仅做请求构造的单元测试（mock 客户端）。

### 3.9 orchestrator.py

```python
class Orchestrator:
    def __init__(self, store, config, backend, workspace_factory, verifier): ...
    def run_task(self, task_id: str) -> Task
```

**主流程（本地模式）**：

```
QUEUED → TRIAGING      记录分级（M0 来自任务创建时的 level）
  ├─ L → REJECTED（返回）
TRIAGING → PLANNING    M0: plan = requirement
PLANNING → CODING      创建 worktree，记录 branch / worktree_path
CODING   ← backend.run_step(ctx)
CODING → VERIFYING     生成 diff
VERIFYING:
  ├─ 防作弊有 Finding → NEEDS_HUMAN
  ├─ 通过 → COMMITTING
  ├─ 失败且 fix_attempts < max → fix_attempts+1，CODING（带失败摘要）
  └─ 失败且已达上限 → NEEDS_HUMAN
COMMITTING → DONE      本地提交；delivery.enabled=false
异常 → FAILED（不可恢复）或 NEEDS_HUMAN（可恢复类，如超时）
```

- 每次转换都经过 `store.transition(expected_version)`，版本号从调用前的任务对象读取。
- 副作用（调用模型、执行验证）在转换之前完成；失败后的状态与副作用的对应关系由“先副作用、后转换”保证。
- 本轮不实现 `resume`，但 `previous_status` 字段已写入，便于下一轮补齐。

### 3.10 report.py

- 生成 `runs/<task-id>/report.md`：基本信息、需求、状态时间线（来自 `task_events`）、验证结果、防作弊结果、diff 统计、分支名与提交 SHA。
- 使用字符串模板（不引入 Jinja2，减少依赖；后续 PR 正文再考虑 Jinja2）。

### 3.11 cli.py

| 命令 | 行为 |
|---|---|
| `agentdev config validate` | 加载并校验所有配置，输出错误位置 |
| `agentdev doctor` | 检查 git、gh、ANTHROPIC_API_KEY、AGENTDEV_HOME 可写、各仓库 local_path |
| `agentdev task create --repo R --text T [--level S] [--budget B]` | 创建任务（状态 QUEUED），打印任务 ID |
| `agentdev task list [--status S] [--repo R]` | 表格输出 |
| `agentdev task status <id>` | 展示任务详情与最近事件 |
| `agentdev run --once [--backend fake\|anthropic]` | 取一个 QUEUED 任务执行到终态或挂起 |

退出码遵循 SDD 8.1：0 成功，1 一般错误，2 参数错误，3 需要人工。

---

## 4. 关键流程的数据流

```
create_task ──▶ tasks(QUEUED)
run --once
  └─ Orchestrator.run_task
       ├─ Workspace.create ──▶ git worktree add -b agent/<id>-<slug>
       ├─ Backend.run_step ──▶ ToolRuntime.execute* ──▶ PolicyEngine.check* ──▶ 文件 / 命令
       ├─ Workspace.diff
       ├─ anticheat.check(diff)
       ├─ Verifier.run ──▶ 执行 verify_commands（日志写入 artifacts）
       ├─ Workspace.commit
       └─ report.write ──▶ runs/<id>/report.md
     每一步：store.transition(...) 写 tasks + task_events
```

---

## 5. 测试计划

| 层次 | 用例（代表性） | 工具 |
|---|---|---|
| 单元 · state | 合法转换通过；非法转换（如 QUEUED→DONE）抛错；终态无出边 | pytest 参数化 |
| 单元 · policy | `../` 穿越被拒；符号链接指向外部被拒；`.env` 读写被拒；`allowed_paths` 外写入被拒；`rm -rf` 被拒；白名单命令通过 | pytest |
| 单元 · config | 缺失字段报错；`budget_usd<=0` 报错；`verify_commands` 为空报错 | pytest + pydantic |
| 单元 · tools | `edit_file` 多处匹配报错；输出截断保留头尾；`search` 忽略 `.git` | pytest + tmp_path |
| 组件 · store | 乐观锁冲突抛 `ConcurrentModification`；事件与状态在同一事务；WAL 模式开启 | pytest + tmp sqlite |
| 组件 · workspace | 在临时 git 仓库中创建 worktree、产生 diff、提交、删除 | pytest + 真实 git |
| 组件 · verify | 命令成功/失败；失败后停止；日志文件生成 | pytest |
| 安全 · anticheat | 删除 `skip` 相反的增加被识别；修改 `jest.config.js` 被标记；正常改动不误报 | pytest |
| 集成 · orchestrator | FakeBackend 脚本：正常完成 → DONE；验证失败一次后修复 → DONE；验证持续失败 → NEEDS_HUMAN；修改验证配置 → NEEDS_HUMAN；L 级 → REJECTED | pytest + 真实 git 临时仓库 |
| 集成 · CLI | `task create` → `run --once` → `task status` 的退出码与输出 | typer.testing.CliRunner |

**验收标准（本轮）**：
1. `pytest` 全部通过
2. 用 FakeBackend 在临时仓库中完成一个 S 级任务，产生提交与 `report.md`
3. `agentdev config validate`、`agentdev doctor` 在本机可运行

---

## 6. 编码顺序（任务拆分）

| 序号 | 任务 | 依赖 |
|---|---|---|
| 1 | 项目骨架：`pyproject.toml`、包目录、`errors.py`、`__init__.py` | — |
| 2 | `state.py` + 单元测试 | 1 |
| 3 | `config.py` + 配置模板 + 单元测试 | 1 |
| 4 | `store.py` + migrations + 组件测试 | 2 |
| 5 | `policy.py` + 安全单元测试 | 3 |
| 6 | `tools.py` + 单元测试 | 5 |
| 7 | `workspace.py` + 组件测试 | 3 |
| 8 | `verify.py` + `anticheat` + 测试 | 7 |
| 9 | `backends/`（base、fake、anthropic 骨架） | 6 |
| 10 | `orchestrator.py` + `report.py` + 集成测试 | 4, 7, 8, 9 |
| 11 | `cli.py` + CLI 测试 | 10 |
| 12 | `prompts/coder.v1.md`、`README.md`、运行验证 | 11 |

---

## 7. 风险与应对（本轮）

| 风险 | 应对 |
|---|---|
| Windows 下 `shell=True` 与路径分隔符差异 | 路径统一用 `pathlib`，存储时转为 POSIX 形式；测试在 Windows 上直接运行 |
| 符号链接在 Windows 上需要权限 | 符号链接相关测试在无权限时 skip，并在报告中注明 |
| `git worktree` 与临时目录清理 | 测试 fixture 中显式 `worktree remove --force`，避免残留 |
| anthropic SDK 版本接口变化 | 本轮只做可选依赖与请求构造，真实调用留到有 API Key 时验证 |

---

## 8. 待确认（不阻塞本轮编码）

1. 本轮使用的 `models.yaml` 默认模型名：先以占位符配置，实际模型在接入真实后端时确认。
2. `verify_commands` 在 Windows 上的写法（如 `npm test` 需要 `shell=True`）：本轮按 `shell=True` 实现。
