from __future__ import annotations

import re
import subprocess
import time
from dataclasses import dataclass, field
from fnmatch import fnmatch
from pathlib import Path

SKIP_PATTERNS = re.compile(
    r"pytest\.mark\.skip|@pytest\.mark\.xfail|\bxfail\b|\bit\.skip\(|\bdescribe\.skip\("
    r"|\btest\.skip\(|\bit\.todo\(|@unittest\.skip|\bt\.Skip\("
)
TEST_DEF_PATTERNS = re.compile(
    r"^\s*(def test_|it\(|test\(|func Test|@Test\b)"
)
TEST_FILE_PATTERNS = re.compile(r"(^|/)(tests?/|__tests__/)|(test_[^/]*\.py$)|(\.(test|spec)\.[jt]sx?$)|(_test\.go$)")


@dataclass
class CommandOutcome:
    name: str
    command: str
    exit_code: int
    duration_s: float
    log_path: str
    tail: str


@dataclass
class VerificationReport:
    passed: bool
    outcomes: list[CommandOutcome] = field(default_factory=list)

    @property
    def failure_summary(self) -> str:
        for o in self.outcomes:
            if o.exit_code != 0:
                return f"命令 `{o.command}` ({o.name}) 失败，退出码 {o.exit_code}。输出末尾:\n{o.tail}"
        return ""

    def to_dict(self) -> dict:
        return {
            "passed": self.passed,
            "outcomes": [
                {
                    "name": o.name,
                    "command": o.command,
                    "exit_code": o.exit_code,
                    "duration_s": round(o.duration_s, 2),
                    "log_path": o.log_path,
                }
                for o in self.outcomes
            ],
        }


class Verifier:
    def __init__(self, *, timeout_s: int, artifacts_dir: Path, tail_chars: int = 3000):
        self.timeout_s = timeout_s
        self.artifacts_dir = Path(artifacts_dir)
        self.tail_chars = tail_chars

    def run(self, cwd: Path, commands: dict[str, str], attempt: int) -> VerificationReport:
        self.artifacts_dir.mkdir(parents=True, exist_ok=True)
        outcomes: list[CommandOutcome] = []
        passed = True
        for name, command in commands.items():
            start = time.monotonic()
            try:
                proc = subprocess.run(
                    command,
                    shell=True,
                    cwd=cwd,
                    capture_output=True,
                    text=True,
                    encoding="utf-8",
                    errors="replace",
                    timeout=self.timeout_s,
                )
                code = proc.returncode
                output = (proc.stdout or "") + (proc.stderr or "")
            except subprocess.TimeoutExpired as e:
                code = 124
                output = (e.stdout or "") + (e.stderr or "") + f"\n超时（>{self.timeout_s}s）"
                if isinstance(output, bytes):
                    output = output.decode("utf-8", errors="replace")
            log_path = self.artifacts_dir / f"verify-{attempt}-{name}.log"
            log_path.write_text(output, encoding="utf-8")
            outcome = CommandOutcome(
                name=name,
                command=command,
                exit_code=code,
                duration_s=time.monotonic() - start,
                log_path=log_path.as_posix(),
                tail=output[-self.tail_chars:],
            )
            outcomes.append(outcome)
            if code != 0:
                passed = False
                break
        return VerificationReport(passed=passed, outcomes=outcomes)


def check_anticheat(
    changed: list[tuple[str, str]],
    diff_text: str,
    verify_config_patterns: list[str],
) -> list[str]:
    """防作弊的最小检查，返回可疑原因列表。空列表表示未发现问题。"""
    findings: list[str] = []

    for status, path in changed:
        posix = path.replace("\\", "/")
        if any(fnmatch(posix, pat) for pat in verify_config_patterns):
            findings.append(f"修改了验证配置文件: {posix}")
        if status == "D" and TEST_FILE_PATTERNS.search(posix):
            findings.append(f"删除了测试文件: {posix}")

    added, removed = _collect_changed_lines(diff_text)

    added_skips = sum(len(SKIP_PATTERNS.findall(line)) for line in added)
    removed_skips = sum(len(SKIP_PATTERNS.findall(line)) for line in removed)
    if added_skips > removed_skips:
        findings.append(f"新增了跳过/预期失败标记 {added_skips - removed_skips} 处")

    added_tests = sum(1 for line in added if TEST_DEF_PATTERNS.search(line))
    removed_tests = sum(1 for line in removed if TEST_DEF_PATTERNS.search(line))
    if removed_tests > added_tests:
        findings.append(f"测试用例数量减少 {removed_tests - added_tests} 个")

    return findings


def _collect_changed_lines(diff_text: str) -> tuple[list[str], list[str]]:
    added: list[str] = []
    removed: list[str] = []
    for line in diff_text.splitlines():
        if line.startswith("+++") or line.startswith("---"):
            continue
        if line.startswith("+"):
            added.append(line[1:])
        elif line.startswith("-"):
            removed.append(line[1:])
    return added, removed
