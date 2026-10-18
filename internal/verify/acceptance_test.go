package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentdev/internal/config"
	"agentdev/internal/planning"
)

func newVerifier(t *testing.T) (Verifier, string) {
	t.Helper()
	dir := t.TempDir()
	return Verifier{Timeout: 30 * time.Second, ArtifactsDir: filepath.Join(dir, "art")}, dir
}

func TestRunAllReusesMatchingCommandResult(t *testing.T) {
	v, dir := newVerifier(t)
	commands := config.VerifyCommands{{Name: "base", Command: "echo base"}}
	acceptance := []planning.Acceptance{
		{ID: "ac1", Description: "基础命令通过", Check: planning.Check{Type: planning.CheckCommand, Name: "base"}},
	}

	rep, err := v.RunAll(dir, commands, acceptance, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed || len(rep.Acceptance) != 1 || !rep.Acceptance[0].Passed {
		t.Fatalf("应当整体通过: %+v", rep)
	}
	if !strings.Contains(rep.Acceptance[0].Evidence, "复用") {
		t.Errorf("与验证命令等价的检查应复用结果: %s", rep.Acceptance[0].Evidence)
	}
	// 复用意味着不会为这条验收标准再写一份日志。
	if _, err := os.Stat(filepath.Join(dir, "art", "acceptance-0-ac1.log")); err == nil {
		t.Error("不应重复执行等价的验证命令")
	}
}

func TestRunAllEvaluatesFileAndGrepChecks(t *testing.T) {
	v, dir := newVerifier(t)
	must(t, os.WriteFile(filepath.Join(dir, "src.py"), []byte("def add(a, b):\n    return a + b\n"), 0o644))

	acceptance := []planning.Acceptance{
		{ID: "exists", Description: "文件存在", Check: planning.Check{Type: planning.CheckFileExists, Path: "src.py"}},
		{ID: "hasadd", Description: "包含 add", Check: planning.Check{Type: planning.CheckGrep, Path: "src.py", Pattern: "def add", Expect: "present"}},
		{ID: "nofixme", Description: "无 FIXME", Check: planning.Check{Type: planning.CheckGrep, Path: "src.py", Pattern: "FIXME", Expect: "absent"}},
	}
	rep, err := v.RunAll(dir, nil, acceptance, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Passed {
		t.Fatalf("应当通过: %+v", rep.Acceptance)
	}
}

func TestRunAllFailsWhenAcceptanceUnmet(t *testing.T) {
	v, dir := newVerifier(t)
	acceptance := []planning.Acceptance{
		{ID: "missing", Description: "文件应当存在", Check: planning.Check{Type: planning.CheckFileExists, Path: "nope.py"}},
	}
	rep, err := v.RunAll(dir, nil, acceptance, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed {
		t.Fatal("缺少文件时应当失败")
	}
	if !strings.Contains(rep.FailureSummary(), "missing") {
		t.Errorf("失败摘要应指向验收标准: %s", rep.FailureSummary())
	}
}

func TestRunAllRejectsUnsafeSelector(t *testing.T) {
	v, dir := newVerifier(t)
	acceptance := []planning.Acceptance{
		{ID: "evil", Description: "注入", Check: planning.Check{
			Type: planning.CheckTest, Name: "base", Selector: "x; rm -rf /",
		}},
	}
	rep, err := v.RunAll(dir, config.VerifyCommands{{Name: "base", Command: "echo base"}}, acceptance, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed || !strings.Contains(rep.Acceptance[0].Evidence, "禁止字符") {
		t.Errorf("危险选择器应被拒绝执行: %+v", rep.Acceptance[0])
	}
}

func TestRunAllSkipsAcceptanceWhenCommandFails(t *testing.T) {
	v, dir := newVerifier(t)
	commands := config.VerifyCommands{{Name: "bad", Command: "go run does_not_exist.go"}}
	acceptance := []planning.Acceptance{
		{ID: "ac1", Description: "文件存在", Check: planning.Check{Type: planning.CheckFileExists, Path: "."}},
	}
	rep, err := v.RunAll(dir, commands, acceptance, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed {
		t.Fatal("验证命令失败时不应通过")
	}
	if len(rep.Acceptance) != 0 {
		t.Errorf("验证命令已失败，不应再执行验收标准: %+v", rep.Acceptance)
	}
}

func TestReportToMapCarriesAcceptance(t *testing.T) {
	rep := Report{Passed: true, Acceptance: []AcceptanceOutcome{
		{ID: "ac1", Description: "d", Target: "echo", Passed: true, Evidence: "ok"},
	}}
	m := rep.ToMap()
	items, ok := m["acceptance"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("验收标准未进入报告: %+v", m)
	}
	if items[0].(map[string]any)["id"] != "ac1" {
		t.Errorf("内容错误: %+v", items[0])
	}
}
