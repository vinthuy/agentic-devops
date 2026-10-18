import pytest

from agentdev.config import PolicyConfig, RepoConfig
from agentdev.policy import PolicyEngine
from agentdev.tools import ToolRuntime, truncate


@pytest.fixture
def worktree(tmp_path):
    root = tmp_path / "wt"
    root.mkdir()
    (root / "src").mkdir()
    (root / "src" / "a.py").write_text("x = 1\ny = 2\nx = 1\n", encoding="utf-8")
    (root / ".env").write_text("SECRET=1\n", encoding="utf-8")
    return root


@pytest.fixture
def violations():
    return []


@pytest.fixture
def tools(worktree, violations):
    repo = RepoConfig(local_path=worktree, verify_commands={"test": "python -c 'x'"})
    policy = PolicyEngine(PolicyConfig(), repo, worktree)
    return ToolRuntime(
        policy,
        max_output_chars=200,
        command_timeout_s=30,
        on_violation=lambda kind, detail, blocked: violations.append((kind, blocked)),
    )


def test_read_file_numbers_lines(tools):
    result = tools.execute("read_file", {"path": "src/a.py"})
    assert result.ok
    assert result.content.splitlines()[0] == "1: x = 1"


def test_read_file_range(tools):
    result = tools.execute("read_file", {"path": "src/a.py", "start": 2, "end": 2})
    assert result.content == "2: y = 2"


def test_read_denied_file_reports_violation(tools, violations):
    result = tools.execute("read_file", {"path": ".env"})
    assert not result.ok
    assert "策略拒绝" in result.content
    assert violations == [("path", True)]


def test_write_then_read(tools, worktree):
    assert tools.execute("write_file", {"path": "new/b.txt", "content": "hi"}).ok
    assert (worktree / "new" / "b.txt").read_text(encoding="utf-8") == "hi"


def test_edit_requires_unique_match(tools, worktree):
    result = tools.execute("edit_file", {"path": "src/a.py", "old": "x = 1", "new": "x = 9"})
    assert not result.ok
    assert "恰好出现一次" in result.content
    ok = tools.execute("edit_file", {"path": "src/a.py", "old": "y = 2", "new": "y = 9"})
    assert ok.ok
    assert "y = 9" in (worktree / "src" / "a.py").read_text(encoding="utf-8")


def test_search_skips_denied_and_git(tools, worktree):
    (worktree / ".git").mkdir()
    (worktree / ".git" / "config").write_text("x = 1\n", encoding="utf-8")
    result = tools.execute("search", {"pattern": "x = 1"})
    assert result.ok
    assert "src/a.py:1:" in result.content
    assert ".git" not in result.content
    assert ".env" not in result.content


def test_list_dir_marks_directories(tools):
    result = tools.execute("list_dir", {"path": "."})
    assert "src/" in result.content.splitlines()
    assert ".env" not in result.content


def test_run_command_policy_blocks_dangerous(tools, violations):
    result = tools.execute("run_command", {"command": "rm -rf ."})
    assert not result.ok
    assert violations[-1] == ("command", True)


def test_unknown_tool_and_missing_args(tools):
    assert not tools.execute("nope", {}).ok
    result = tools.execute("write_file", {"path": "a.txt"})
    assert not result.ok
    assert "缺少参数" in result.content


def test_truncate_keeps_head_and_tail():
    text = "A" * 500 + "B" * 500
    out = truncate(text, 100)
    assert out.startswith("A")
    assert out.endswith("B")
    assert "已截断" in out
    assert truncate("short", 100) == "short"


def test_schemas_expose_all_tools(tools):
    names = {s["name"] for s in tools.schemas()}
    assert names == {"read_file", "write_file", "edit_file", "list_dir", "search", "run_command"}
