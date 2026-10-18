package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"agentdev/internal/testutil"
)

// 进程级故障注入：不是模拟状态，而是真的启动 agentdev 子进程、在模型调用中途杀掉，
// 再用新进程 resume，验证“从最后一个已持久化的状态继续且不重复提交”。
const crashTestKeyEnv = "AGENTDEV_CRASH_TEST_KEY"

// scriptedModel 按顺序返回预设响应；blockOnCall 指定的那一次请求会挂起，
// 让测试有机会杀掉进程。
type scriptedModel struct {
	mu          sync.Mutex
	calls       int
	blockOnCall int
	blocked     chan struct{}
	blockedOnce sync.Once
	release     chan struct{}
	responses   []string
	seen        []string
}

func (m *scriptedModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	tool := "text"
	var req struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err == nil && len(req.Tools) > 0 {
		tool = req.Tools[0].Name
	}

	m.mu.Lock()
	m.calls++
	call := m.calls
	m.seen = append(m.seen, fmt.Sprintf("#%d %s", call, tool))
	m.mu.Unlock()

	if call == m.blockOnCall {
		// 不写响应：真实进程会一直等在这里，直到测试杀掉它。
		m.blockedOnce.Do(func() { close(m.blocked) })
		<-m.release
		return
	}
	idx := call - 1
	if idx >= len(m.responses) {
		idx = len(m.responses) - 1
	}
	w.Header().Set("content-type", "application/json")
	_, _ = io.WriteString(w, m.responses[idx])
}

func (m *scriptedModel) callsSeen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...)
}

func toolUseResponse(name, id string, input map[string]any) string {
	body := map[string]any{
		"content": []any{map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": input,
		}},
		"stop_reason": "tool_use",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
	}
	data, _ := json.Marshal(body)
	return string(data)
}

func textResponse(text string) string {
	body := map[string]any{
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
	}
	data, _ := json.Marshal(body)
	return string(data)
}

func buildCLI(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "agentdev-crash-test.exe")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/agentdev")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建 CLI 失败: %v\n%s", err, out)
	}
	return bin
}

func writeCrashHome(t *testing.T, repoPath, baseURL string) string {
	t.Helper()
	home := t.TempDir()
	writeYAML := func(name string, value any) {
		data, err := yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeYAML("repos.yaml", map[string]any{
		"repos": map[string]any{
			"acme/app": map[string]any{
				"local_path":      filepath.ToSlash(repoPath),
				"default_branch":  "main",
				"verify_commands": map[string]string{"check": "go run check.go"},
			},
		},
	})
	writeYAML("models.yaml", map[string]any{
		"provider":    "anthropic",
		"base_url":    baseURL,
		"api_key_env": crashTestKeyEnv,
		"model":       "crash-test-model",
		"max_tokens":  1024,
	})
	writeYAML("config.yaml", map[string]any{
		"delivery": map[string]any{"enabled": false},
		"review":   map[string]any{"enabled": false},
	})
	return home
}

func runCLI(t *testing.T, bin, home string, args ...string) (int, string) {
	t.Helper()
	full := append([]string{}, args...)
	full = append(full, "--home", home)
	cmd := exec.Command(bin, full...)
	cmd.Env = append(os.Environ(), crashTestKeyEnv+"=test-key")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("执行 CLI 失败: %v\n%s", err, out)
		}
	}
	return code, string(out)
}

func TestCrashDuringCodingThenResumeCompletes(t *testing.T) {
	if testing.Short() {
		t.Skip("进程级故障注入在 -short 下跳过")
	}
	bin := buildCLI(t)
	repoPath := testutil.NewRepo(t)

	// 第 1 次模型调用：分级；第 2 次：规划；第 3 次：写文件；第 4 次：挂起（在此 kill）。
	model := &scriptedModel{
		blockOnCall: 4,
		blocked:     make(chan struct{}),
		release:     make(chan struct{}),
		responses: []string{
			toolUseResponse("submit_estimate", "t1", map[string]any{
				"level": "S", "reasons": []string{"单文件小改动"},
				"estimated_files": 1, "estimated_lines": 3,
			}),
			toolUseResponse("submit_plan", "t2", map[string]any{
				"status":  "ready",
				"summary": "创建 hello.txt 并验证",
				"scope": map[string]any{
					"allowed_paths": []string{"hello.txt"},
				},
				"steps": []any{map[string]any{
					"id": "s1", "description": "写 hello.txt", "files": []string{"hello.txt"},
				}},
				"acceptance": []any{map[string]any{
					"id": "a1", "description": "验证命令通过",
					"check": map[string]any{"type": "command", "name": "check"},
				}},
			}),
			toolUseResponse("write_file", "t3", map[string]any{
				"path": "hello.txt", "content": "hello\n",
			}),
			textResponse("不应到达"),
		},
	}
	server := httptest.NewServer(model)
	defer func() {
		close(model.release) // 放行被阻塞的 handler，避免 Close 挂住
		server.Close()
	}()
	home := writeCrashHome(t, repoPath, server.URL)

	code, created := runCLI(t, bin, home, "task", "create", "--repo", "acme/app",
		"--text", "在仓库根目录创建 hello.txt，内容为 hello")
	if code != 0 {
		t.Fatalf("创建任务失败 (%d): %s", code, created)
	}
	taskID := idPattern.FindString(created)
	if taskID == "" {
		t.Fatalf("创建输出中找不到任务 ID: %s", created)
	}

	// 启动执行进程，等它卡在第 4 次模型调用后杀掉。
	cmd := exec.Command(bin, "run", "--backend", "anthropic", "--task", taskID, "--home", home)
	cmd.Env = append(os.Environ(), crashTestKeyEnv+"=test-key")
	var childOut strings.Builder
	cmd.Stdout = &childOut
	cmd.Stderr = &childOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.blocked:
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("执行进程未进入阻塞的模型调用；已见调用: %v\n子进程输出:\n%s",
			model.callsSeen(), childOut.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("杀掉执行进程失败: %v", err)
	}
	_ = cmd.Wait()

	// 崩溃点：状态仍应是 CODING，且文件已经写入（副作用发生在状态转换之前）。
	code, out := runCLI(t, bin, home, "task", "status", taskID)
	if code != 0 || !strings.Contains(out, "CODING") {
		t.Fatalf("崩溃后状态应为 CODING (%d): %s", code, out)
	}
	worktree := worktreeFor(t, home, taskID)
	if _, err := os.Stat(filepath.Join(worktree, "hello.txt")); err != nil {
		t.Fatalf("崩溃前写入的文件应保留: %v", err)
	}

	// 用新进程 resume：模型直接结束本轮，交给验证与提交。
	resumeModel := &scriptedModel{
		responses: []string{textResponse("没有更多改动")},
	}
	resumeServer := httptest.NewServer(resumeModel)
	defer resumeServer.Close()
	rewriteBaseURL(t, home, resumeServer.URL)

	code, out = runCLI(t, bin, home, "run", "--backend", "anthropic", "--task", taskID)
	if code != 0 {
		t.Fatalf("resume 失败 (%d): %s", code, out)
	}
	code, out = runCLI(t, bin, home, "task", "status", taskID)
	if code != 0 || !strings.Contains(out, "DONE") {
		t.Fatalf("resume 后应完成 (%d): %s", code, out)
	}

	commits := testutil.Git(t, worktree, "log", "--format=%s")
	if got := strings.Count(commits, "agent("+taskID+")"); got != 1 {
		t.Fatalf("任务提交应恰好 1 个，实际 %d:\n%s", got, commits)
	}
}

func worktreeFor(t *testing.T, home, taskID string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, "workspaces", "*", taskID))
	if err != nil || len(matches) == 0 {
		t.Fatalf("找不到任务工作区: %v %v", err, matches)
	}
	return matches[0]
}

func rewriteBaseURL(t *testing.T, home, baseURL string) {
	t.Helper()
	path := filepath.Join(home, "models.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var models map[string]any
	if err := yaml.Unmarshal(data, &models); err != nil {
		t.Fatal(err)
	}
	models["base_url"] = baseURL
	updated, err := yaml.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		t.Fatal(err)
	}
}
