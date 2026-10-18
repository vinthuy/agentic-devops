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

// 选择器匹配不到测试时必须标记为“不可执行”，而不是普通失败或假通过。
// `go test -run 不存在的测试` 退出码为 0，若只看退出码会得到假通过。
func TestRunAllMarksSelectorMatchingNoTestsAsInvalid(t *testing.T) {
	v, dir := newVerifier(t)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"),
		[]byte("package fixture\n\nimport \"testing\"\n\nfunc TestOK(t *testing.T) {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	commands := config.VerifyCommands{{Name: "test", Command: "go test ./..."}}
	acceptance := []planning.Acceptance{{
		ID: "ac1", Description: "指定测试通过",
		Check: planning.Check{Type: planning.CheckTest, Name: "test", Selector: "-run NoSuchTest"},
	}}

	rep, err := v.RunAll(dir, commands, acceptance, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Passed {
		t.Fatalf("匹配不到测试的检查不能算通过: %+v", rep)
	}
	invalid := rep.InvalidAcceptance()
	if len(invalid) != 1 || invalid[0].ID != "ac1" {
		t.Fatalf("应标记为不可执行的验收标准: %+v", rep.Acceptance)
	}
	if !strings.Contains(invalid[0].Evidence, "未匹配到任何测试") {
		t.Errorf("证据应说明原因: %s", invalid[0].Evidence)
	}
}

// 运行器直接拒绝选择器语法时，也必须标记为不可执行，而不是普通失败。
func TestRunAllMarksRunnerRejectedSelectorAsInvalid(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{"unittest 加载失败", "ERROR: py::test_nope (unittest.loader._FailedTest.py::test_nope)"},
		{"go 混用文件与包", "named files must be .go files: ./..."},
		{"node 找不到测试文件", "Could not find any test files matching pattern"},
	}
	for _, c := range cases {
		if !looksLikeNoTests(c.output) {
			t.Errorf("%s: 应识别为选择器不可执行: %q", c.name, c.output)
		}
	}
	// 真正的代码失败不能被误判成选择器问题。
	if looksLikeNoTests("FAIL: TestParse (0.00s)\n  parse_test.go:9: want 3, got 2") {
		t.Error("普通断言失败不应被识别为选择器问题")
	}
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
