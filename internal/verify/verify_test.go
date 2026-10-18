package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentdev/internal/config"
	"agentdev/internal/workspace"
)

func TestAnticheatCleanChange(t *testing.T) {
	changes := []workspace.FileChange{{Status: 'M', Path: "src/app.go"}}
	diff := "+++ b/src/app.go\n+func Handle() int {\n+\treturn 1\n+}\n"
	if got := Anticheat(changes, diff, []string{"jest.config.*"}); len(got) != 0 {
		t.Errorf("正常改动不应误报: %v", got)
	}
}

func TestAnticheatFlagsVerifyConfigChange(t *testing.T) {
	got := Anticheat([]workspace.FileChange{{Status: 'M', Path: "jest.config.js"}}, "", []string{"jest.config.*"})
	if !containsAll(got, "验证配置") {
		t.Errorf("应当标记验证配置修改: %v", got)
	}
}

func TestAnticheatFlagsDeletedTestFile(t *testing.T) {
	got := Anticheat([]workspace.FileChange{{Status: 'D', Path: "tests/test_login.py"}}, "", nil)
	if !containsAll(got, "删除了测试文件") {
		t.Errorf("应当标记删除测试: %v", got)
	}
}

func TestAnticheatFlagsAddedSkipMarker(t *testing.T) {
	diff := "+++ b/tests/test_a.py\n+@pytest.mark.skip(reason='flaky')\n def test_a():\n"
	got := Anticheat([]workspace.FileChange{{Status: 'M', Path: "tests/test_a.py"}}, diff, nil)
	if !containsAll(got, "跳过") {
		t.Errorf("应当标记新增 skip: %v", got)
	}
}

func TestAnticheatIgnoresMovedSkipMarker(t *testing.T) {
	diff := "--- a/tests/test_a.py\n+++ b/tests/test_a.py\n" +
		"-@pytest.mark.skip(reason='x')\n+@pytest.mark.skip(reason='y')\n"
	if got := Anticheat([]workspace.FileChange{{Status: 'M', Path: "tests/test_a.py"}}, diff, nil); len(got) != 0 {
		t.Errorf("仅移动 skip 不应误报: %v", got)
	}
}

func TestAnticheatFlagsReducedTestCount(t *testing.T) {
	diff := "--- a/tests/test_a.py\n+++ b/tests/test_a.py\n" +
		"-def test_one():\n-def test_two():\n+def test_one():\n"
	got := Anticheat([]workspace.FileChange{{Status: 'M', Path: "tests/test_a.py"}}, diff, nil)
	if !containsAll(got, "数量减少") {
		t.Errorf("应当标记测试数量减少: %v", got)
	}
}

func TestVerifierPassesAndWritesLog(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("done"), 0o644))
	v := Verifier{Timeout: 30 * time.Second, ArtifactsDir: filepath.Join(dir, "art")}
	rep, err := v.Run(dir, config.VerifyCommands{{Name: "ok", Command: "go version"}}, 0)
	must(t, err)
	if !rep.Passed {
		t.Fatalf("应当通过: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(dir, "art", "verify-0-ok.log")); err != nil {
		t.Errorf("日志文件未生成: %v", err)
	}
}

func TestVerifierStopsAtFirstFailure(t *testing.T) {
	dir := t.TempDir()
	v := Verifier{Timeout: 30 * time.Second, ArtifactsDir: filepath.Join(dir, "art")}
	cmds := config.VerifyCommands{
		{Name: "bad", Command: "go run does_not_exist.go"},
		{Name: "never", Command: "go version"},
	}
	rep, err := v.Run(dir, cmds, 1)
	must(t, err)
	if rep.Passed {
		t.Fatal("应当失败")
	}
	if len(rep.Outcomes) != 1 || rep.Outcomes[0].Name != "bad" {
		t.Errorf("应当在第一个失败处停止: %+v", rep.Outcomes)
	}
	if rep.Outcomes[0].ExitCode == 0 {
		t.Error("退出码应非零")
	}
	if !strings.Contains(rep.FailureSummary(), "bad") {
		t.Errorf("失败摘要异常: %s", rep.FailureSummary())
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func containsAll(items []string, sub string) bool {
	for _, it := range items {
		if strings.Contains(it, sub) {
			return true
		}
	}
	return false
}
