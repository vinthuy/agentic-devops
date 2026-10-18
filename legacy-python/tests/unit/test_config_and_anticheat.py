import pytest
import yaml

from agentdev.config import RepoConfig, load_config
from agentdev.errors import ConfigError
from agentdev.verify import check_anticheat


def test_repo_requires_verify_commands(tmp_path):
    with pytest.raises(ValueError, match="verify_commands"):
        RepoConfig(local_path=tmp_path, verify_commands={})


def test_repo_budget_must_be_positive(tmp_path):
    with pytest.raises(ValueError, match="budget_usd"):
        RepoConfig(local_path=tmp_path, verify_commands={"t": "x"}, budget_usd=0)


def test_load_config_defaults_when_files_missing(home):
    config = load_config(home)
    assert config.repos == {}
    assert config.delivery.enabled is False
    assert config.policy.max_fix_attempts == 3
    assert config.db_path == home / "agentdev.db"


def test_load_config_reads_repos_yaml(home, tmp_path):
    (home / "repos.yaml").write_text(
        yaml.safe_dump(
            {
                "repos": {
                    "acme/app": {
                        "local_path": tmp_path.as_posix(),
                        "verify_commands": {"test": "pytest -q"},
                    }
                }
            }
        ),
        encoding="utf-8",
    )
    config = load_config(home)
    assert config.repos["acme/app"].verify_commands == {"test": "pytest -q"}


def test_load_config_reports_field_errors(home, tmp_path):
    (home / "repos.yaml").write_text(
        yaml.safe_dump({"repos": {"acme/app": {"local_path": tmp_path.as_posix()}}}),
        encoding="utf-8",
    )
    with pytest.raises(ConfigError, match="verify_commands"):
        load_config(home)


def test_load_config_rejects_bad_yaml(home):
    (home / "policy.yaml").write_text("deny_paths: [\n", encoding="utf-8")
    with pytest.raises(ConfigError, match="YAML"):
        load_config(home)


def test_anticheat_clean_change_has_no_findings():
    changed = [("M", "src/app.py")]
    diff = "+++ b/src/app.py\n+def handle():\n+    return 1\n"
    assert check_anticheat(changed, diff, ["jest.config.*"]) == []


def test_anticheat_flags_verify_config_change():
    findings = check_anticheat([("M", "jest.config.js")], "", ["jest.config.*"])
    assert any("验证配置" in f for f in findings)


def test_anticheat_flags_deleted_test_file():
    findings = check_anticheat([("D", "tests/test_login.py")], "", [])
    assert any("删除了测试文件" in f for f in findings)


def test_anticheat_flags_added_skip_marker():
    diff = "+++ b/tests/test_a.py\n+@pytest.mark.skip(reason='flaky')\n def test_a():\n"
    findings = check_anticheat([("M", "tests/test_a.py")], diff, [])
    assert any("跳过" in f for f in findings)


def test_anticheat_ignores_moved_skip_marker():
    diff = (
        "--- a/tests/test_a.py\n+++ b/tests/test_a.py\n"
        "-@pytest.mark.skip(reason='x')\n+@pytest.mark.skip(reason='y')\n"
    )
    assert check_anticheat([("M", "tests/test_a.py")], diff, []) == []


def test_anticheat_flags_reduced_test_count():
    diff = "--- a/tests/test_a.py\n+++ b/tests/test_a.py\n-def test_one():\n-def test_two():\n+def test_one():\n"
    findings = check_anticheat([("M", "tests/test_a.py")], diff, [])
    assert any("数量减少" in f for f in findings)
