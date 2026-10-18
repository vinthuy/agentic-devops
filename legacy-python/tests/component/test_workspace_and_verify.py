from agentdev.config import AppConfig
from agentdev.verify import Verifier
from agentdev.workspace import Workspace
from tests.conftest import git


def test_create_worktree_and_commit(config: AppConfig, repo_path):
    ws = Workspace(config)
    repo = config.repos["acme/app"]
    info = ws.create("acme/app", repo, "t_test_1", "fix login")

    assert info.branch.startswith("agent/t_test_1-")
    assert (info.path / "README.md").exists()
    assert ws.commit(info, "noop", ("a", "a@b")) is None

    (info.path / "hello.txt").write_text("hello\n", encoding="utf-8")
    changed = ws.changed_files(info)
    assert ("A", "hello.txt") in changed
    assert "hello.txt" in ws.diff(info)

    sha = ws.commit(info, "agent: add hello", ("agentdev", "agentdev@localhost"))
    assert sha and len(sha) == 40
    assert "agent: add hello" in git(info.path, "log", "-1", "--format=%s")

    ws.remove("acme/app", repo, info, keep_branch=True)
    assert not info.path.exists()
    assert info.branch in git(repo_path, "branch", "--list", info.branch)


def test_verifier_passes_and_writes_log(tmp_path):
    (tmp_path / "ok.py").write_text("print('done')\n", encoding="utf-8")
    verifier = Verifier(timeout_s=30, artifacts_dir=tmp_path / "art")
    report = verifier.run(tmp_path, {"ok": "python ok.py"}, attempt=0)
    assert report.passed
    assert (tmp_path / "art" / "verify-0-ok.log").read_text(encoding="utf-8").strip() == "done"


def test_verifier_stops_at_first_failure(tmp_path):
    (tmp_path / "bad.py").write_text("import sys; print('boom'); sys.exit(3)\n", encoding="utf-8")
    verifier = Verifier(timeout_s=30, artifacts_dir=tmp_path / "art")
    report = verifier.run(
        tmp_path,
        {"bad": "python bad.py", "never": "python -c \"raise SystemExit(1)\""},
        attempt=1,
    )
    assert not report.passed
    assert [o.name for o in report.outcomes] == ["bad"]
    assert report.outcomes[0].exit_code == 3
    assert "boom" in report.failure_summary
