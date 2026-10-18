package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/config"
	"agentdev/internal/policy"
)

type violationLog struct {
	kinds []string
}

func newRuntime(t *testing.T) (*Runtime, string, *violationLog) {
	t.Helper()
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "src"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("x = 1\ny = 2\nx = 1\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1\n"), 0o644))

	repo := config.RepoConfig{
		LocalPath:      root,
		VerifyCommands: config.VerifyCommands{{Name: "version", Command: "go version"}},
	}
	p := config.PolicyConfig{DenyPaths: config.DefaultDenyPaths, DenyCommandPatterns: config.DefaultDenyCommandPatterns}
	eng, err := policy.NewEngine(p, repo, root, nil)
	must(t, err)
	log := &violationLog{}
	rt := New(eng, 200, 60, func(kind, detail string, blocked bool) {
		log.kinds = append(log.kinds, kind)
	})
	return rt, root, log
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadFileNumbersLines(t *testing.T) {
	rt, _, _ := newRuntime(t)
	res := rt.Execute("read_file", map[string]any{"path": "src/a.go"})
	if !res.OK || !strings.HasPrefix(res.Content, "1: x = 1") {
		t.Fatalf("read_file 结果异常: %+v", res)
	}
}

func TestReadFileRange(t *testing.T) {
	rt, _, _ := newRuntime(t)
	res := rt.Execute("read_file", map[string]any{"path": "src/a.go", "start": 2.0, "end": 2.0})
	if res.Content != "2: y = 2" {
		t.Errorf("范围读取结果: %q", res.Content)
	}
}

func TestReadDeniedFileRecordsViolation(t *testing.T) {
	rt, _, log := newRuntime(t)
	res := rt.Execute("read_file", map[string]any{"path": ".env"})
	if res.OK || !strings.Contains(res.Content, "策略拒绝") {
		t.Fatalf("应当被拒绝: %+v", res)
	}
	if len(log.kinds) != 1 || log.kinds[0] != "path" {
		t.Errorf("违规记录异常: %v", log.kinds)
	}
}

func TestWriteThenRead(t *testing.T) {
	rt, root, _ := newRuntime(t)
	if res := rt.Execute("write_file", map[string]any{"path": "new/b.txt", "content": "hi"}); !res.OK {
		t.Fatalf("写入失败: %+v", res)
	}
	data, err := os.ReadFile(filepath.Join(root, "new", "b.txt"))
	must(t, err)
	if string(data) != "hi" {
		t.Errorf("文件内容: %q", data)
	}
}

func TestEditRequiresUniqueMatch(t *testing.T) {
	rt, root, _ := newRuntime(t)
	if res := rt.Execute("edit_file", map[string]any{"path": "src/a.go", "old": "x = 1", "new": "x = 9"}); res.OK {
		t.Error("多处匹配应当失败")
	}
	if res := rt.Execute("edit_file", map[string]any{"path": "src/a.go", "old": "y = 2", "new": "y = 9"}); !res.OK {
		t.Fatalf("唯一匹配应当成功: %+v", res)
	}
	data, _ := os.ReadFile(filepath.Join(root, "src", "a.go"))
	if !strings.Contains(string(data), "y = 9") {
		t.Error("编辑未生效")
	}
}

func TestSearchSkipsDeniedAndGit(t *testing.T) {
	rt, root, _ := newRuntime(t)
	must(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("x = 1\n"), 0o644))
	res := rt.Execute("search", map[string]any{"pattern": "x = 1"})
	if !res.OK || !strings.Contains(res.Content, "src/a.go:1:") {
		t.Fatalf("搜索结果异常: %+v", res)
	}
	if strings.Contains(res.Content, ".git") || strings.Contains(res.Content, ".env") {
		t.Errorf("不应包含被跳过的文件: %s", res.Content)
	}
}

func TestSearchNotAffectedByWorkspaceNameContainingSkipDir(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "build", "ws")
	must(t, os.MkdirAll(root, 0o755))
	must(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("needle\n"), 0o644))
	repo := config.RepoConfig{LocalPath: root, VerifyCommands: config.VerifyCommands{{Name: "t", Command: "go version"}}}
	p := config.PolicyConfig{DenyPaths: config.DefaultDenyPaths, DenyCommandPatterns: config.DefaultDenyCommandPatterns}
	eng, err := policy.NewEngine(p, repo, root, nil)
	must(t, err)
	rt := New(eng, 200, 60, nil)
	res := rt.Execute("search", map[string]any{"pattern": "needle"})
	if !strings.Contains(res.Content, "a.txt:1:") {
		t.Errorf("工作区路径中含 build 不应导致跳过: %s", res.Content)
	}
}

func TestListDirMarksDirectoriesAndHidesDenied(t *testing.T) {
	rt, _, _ := newRuntime(t)
	res := rt.Execute("list_dir", map[string]any{"path": "."})
	if !strings.Contains(res.Content, "src/") {
		t.Errorf("目录应以 / 结尾: %s", res.Content)
	}
	if strings.Contains(res.Content, ".env") {
		t.Errorf("敏感文件不应出现在列表中: %s", res.Content)
	}
}

func TestRunCommandBlocksDangerous(t *testing.T) {
	rt, _, log := newRuntime(t)
	res := rt.Execute("run_command", map[string]any{"command": "rm -rf ."})
	if res.OK {
		t.Fatal("危险命令应当被拒绝")
	}
	if len(log.kinds) == 0 || log.kinds[len(log.kinds)-1] != "command" {
		t.Errorf("违规记录异常: %v", log.kinds)
	}
}

func TestRunCommandAllowedRuns(t *testing.T) {
	rt, _, _ := newRuntime(t)
	res := rt.Execute("run_command", map[string]any{"command": "go version"})
	if !res.OK || !strings.HasPrefix(res.Content, "exit=0") {
		t.Errorf("白名单命令应当执行成功: %+v", res)
	}
}

func TestUnknownToolAndMissingArgs(t *testing.T) {
	rt, _, _ := newRuntime(t)
	if res := rt.Execute("nope", nil); res.OK {
		t.Error("未知工具应当失败")
	}
	res := rt.Execute("write_file", map[string]any{"path": "a.txt"})
	if res.OK || !strings.Contains(res.Content, "缺少参数") {
		t.Errorf("缺少参数应当返回明确错误: %+v", res)
	}
}

func TestTruncateKeepsHeadAndTailAndRunes(t *testing.T) {
	text := strings.Repeat("A", 500) + strings.Repeat("好", 500)
	out := Truncate(text, 100)
	if !strings.HasPrefix(out, "A") || !strings.HasSuffix(out, "好") {
		t.Errorf("头尾保留异常: %q", out)
	}
	if !strings.Contains(out, "已截断") {
		t.Error("应当标注截断")
	}
	if Truncate("short", 100) != "short" {
		t.Error("短文本不应被截断")
	}
}

func TestSchemasExposeAllTools(t *testing.T) {
	rt, _, _ := newRuntime(t)
	names := map[string]bool{}
	for _, s := range rt.Schemas() {
		names[s["name"].(string)] = true
	}
	for _, want := range []string{"read_file", "write_file", "edit_file", "list_dir", "search", "run_command"} {
		if !names[want] {
			t.Errorf("缺少工具定义 %s", want)
		}
	}
}
