package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"agentdev/internal/testutil"
)

func writeHome(t *testing.T, repoPath string) string {
	t.Helper()
	home := t.TempDir()
	data, err := yaml.Marshal(map[string]any{
		"repos": map[string]any{
			"acme/app": map[string]any{
				"local_path":          filepath.ToSlash(repoPath),
				"default_branch":      "main",
				"verify_commands":     map[string]string{"check": "go run check.go"},
				"verify_config_files": []string{"check.go"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "repos.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Execute(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestConfigValidateOK(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	code, out, errOut := invoke(t, "config", "validate", "--home", home)
	if code != ExitOK {
		t.Fatalf("退出码 %d，stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "配置校验通过") {
		t.Errorf("输出异常: %s", out)
	}
}

func TestTaskCreateRejectsUnknownRepo(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	code, _, _ := invoke(t, "task", "create", "--repo", "nope/x", "--text", "x", "--home", home)
	if code != ExitUsage {
		t.Errorf("退出码应为 %d，实际 %d", ExitUsage, code)
	}
}

func TestTaskCreateRequiresExactlyOneSource(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	code, _, _ := invoke(t, "task", "create", "--repo", "acme/app", "--home", home)
	if code != ExitUsage {
		t.Errorf("缺少需求来源应为用法错误，实际 %d", code)
	}
	code, _, _ = invoke(t, "task", "create", "--repo", "acme/app", "--text", "a", "--file", "b", "--home", home)
	if code != ExitUsage {
		t.Errorf("两种来源同时给出应为用法错误，实际 %d", code)
	}
}

var idPattern = regexp.MustCompile(`t_\d{8}_[0-9a-f]{6}`)

func TestCreateThenRunFakeThenStatus(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))

	code, out, errOut := invoke(t, "task", "create", "--repo", "acme/app",
		"--text", "创建 hello.txt", "--title", "hello", "--home", home)
	if code != ExitOK {
		t.Fatalf("创建失败 %d: %s", code, errOut)
	}
	taskID := idPattern.FindString(out)
	if taskID == "" {
		t.Fatalf("输出中找不到任务 ID: %s", out)
	}

	script := filepath.Join(home, "script.json")
	data, _ := json.Marshal([][]map[string]any{{
		{"tool": "write_file", "args": map[string]any{"path": "hello.txt", "content": "hello\n"}},
	}})
	if err := os.WriteFile(script, data, 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut = invoke(t, "run", "--backend", "fake", "--script", script, "--home", home)
	if code != ExitOK {
		t.Fatalf("run 退出码 %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "DONE") {
		t.Errorf("运行结果应为 DONE: %s", out)
	}

	code, out, _ = invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "DONE") {
		t.Errorf("status 异常 (%d): %s", code, out)
	}
}

func TestRunWithEmptyQueue(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	code, out, _ := invoke(t, "run", "--backend", "fake", "--script", "unused.json", "--home", home)
	if code != ExitOK || !strings.Contains(out, "没有排队中的任务") {
		t.Errorf("空队列异常 (%d): %s", code, out)
	}
}

func TestRunReturnsHumanExitCode(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	invoke(t, "task", "create", "--repo", "acme/app", "--text", "x", "--home", home)

	step := []map[string]any{{"tool": "write_file", "args": map[string]any{"path": "other.txt", "content": "z"}}}
	data, _ := json.Marshal([][]map[string]any{step, step, step})
	script := filepath.Join(home, "bad.json")
	if err := os.WriteFile(script, data, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, _ := invoke(t, "run", "--backend", "fake", "--script", script, "--home", home)
	if code != ExitHuman {
		t.Errorf("应返回人工介入退出码 %d，实际 %d", ExitHuman, code)
	}
}

func TestTaskListShowsCreated(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	invoke(t, "task", "create", "--repo", "acme/app", "--text", "a", "--home", home)
	code, out, _ := invoke(t, "task", "list", "--home", home)
	if code != ExitOK || !strings.Contains(out, "acme/app") {
		t.Errorf("list 异常 (%d): %s", code, out)
	}
}

func TestAnthropicBackendRequiresKey(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	t.Setenv("DEEPSEEK_API_KEY", "")
	os.Unsetenv("DEEPSEEK_API_KEY")
	invoke(t, "task", "create", "--repo", "acme/app", "--text", "a", "--home", home)
	code, _, errOut := invoke(t, "run", "--home", home)
	if code != ExitUsage || !strings.Contains(errOut, "DEEPSEEK_API_KEY") {
		t.Errorf("缺少 Key 应给出明确提示 (%d): %s", code, errOut)
	}
}
