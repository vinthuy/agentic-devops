import json
import re

import pytest
import yaml
from typer.testing import CliRunner

from agentdev.cli import EXIT_HUMAN, EXIT_OK, EXIT_USAGE, app
from tests.conftest import git

runner = CliRunner()


@pytest.fixture
def cli_home(home, repo_path):
    (home / "repos.yaml").write_text(
        yaml.safe_dump(
            {
                "repos": {
                    "acme/app": {
                        "local_path": repo_path.as_posix(),
                        "default_branch": "main",
                        "verify_commands": {"check": "python check.py"},
                        "verify_config_files": ["check.py"],
                    }
                }
            }
        ),
        encoding="utf-8",
    )
    return home


def _invoke(home, *args):
    return runner.invoke(app, [*args, "--home", str(home)])


def test_config_validate_ok(cli_home):
    result = _invoke(cli_home, "config", "validate")
    assert result.exit_code == EXIT_OK, result.output
    assert "校验通过" in result.output


def test_task_create_rejects_unknown_repo(cli_home):
    result = _invoke(cli_home, "task", "create", "--repo", "nope/x", "--text", "x")
    assert result.exit_code == EXIT_USAGE


def test_task_create_requires_exactly_one_source(cli_home):
    result = _invoke(cli_home, "task", "create", "--repo", "acme/app")
    assert result.exit_code == EXIT_USAGE


def test_create_then_run_fake_then_status(cli_home, tmp_path):
    created = _invoke(
        cli_home,
        "task", "create", "--repo", "acme/app",
        "--text", "创建 hello.txt", "--title", "hello",
    )
    assert created.exit_code == EXIT_OK, created.output
    task_id = re.search(r"t_\d{8}_[0-9a-f]{6}", created.output).group(0)

    script = tmp_path / "script.json"
    script.write_text(
        json.dumps([[{"tool": "write_file", "args": {"path": "hello.txt", "content": "hello\n"}}]]),
        encoding="utf-8",
    )
    ran = _invoke(cli_home, "run", "--backend", "fake", "--script", str(script))
    assert ran.exit_code == EXIT_OK, ran.output
    assert "DONE" in ran.output

    status = _invoke(cli_home, "task", "status", task_id)
    assert status.exit_code == EXIT_OK
    assert "DONE" in status.output


def test_run_with_no_queue_is_ok(cli_home):
    result = _invoke(cli_home, "run", "--backend", "fake", "--script", "unused.json")
    assert result.exit_code == EXIT_OK
    assert "没有排队" in result.output


def test_run_returns_human_exit_code_when_needs_human(cli_home, tmp_path):
    _invoke(cli_home, "task", "create", "--repo", "acme/app", "--text", "x")
    script = tmp_path / "bad.json"
    script.write_text(
        json.dumps([[{"tool": "write_file", "args": {"path": "other.txt", "content": "z"}}]] * 3),
        encoding="utf-8",
    )
    result = _invoke(cli_home, "run", "--backend", "fake", "--script", str(script))
    assert result.exit_code == EXIT_HUMAN


def test_task_list_shows_created(cli_home):
    _invoke(cli_home, "task", "create", "--repo", "acme/app", "--text", "a")
    result = _invoke(cli_home, "task", "list")
    assert result.exit_code == EXIT_OK
    assert "acme/app" in result.output
