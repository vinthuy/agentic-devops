package orchestrator

import (
	"encoding/json"
	"strings"
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/delivery"
	"agentdev/internal/planning"
)

// G-01：推送前必须拒绝默认分支与非预期前缀。
func TestPushGuardRejectsProtectedBranches(t *testing.T) {
	cases := []struct {
		name string
		req  delivery.PushRequest
	}{
		{"默认分支 main", delivery.PushRequest{Branch: "main"}},
		{"默认分支大小写", delivery.PushRequest{Branch: "MASTER"}},
		{"配置的默认分支", delivery.PushRequest{Branch: "release/1.0", Protected: "release/1.0"}},
		{"缺少配置前缀", delivery.PushRequest{Branch: "feature/x", AllowedPrefix: "agent/"}},
		{"前缀不匹配", delivery.PushRequest{Branch: "agent/x", AllowedPrefix: "feature/"}},
		{"空分支", delivery.PushRequest{Branch: ""}},
	}
	for _, c := range cases {
		if err := delivery.EnsurePushable(c.req); err == nil {
			t.Errorf("%s: 应当拒绝推送", c.name)
		}
	}
	if err := delivery.EnsurePushable(delivery.PushRequest{
		Branch: "agent/t_20261018_abc123-fix", Protected: "main", AllowedPrefix: "agent/",
	}); err != nil {
		t.Errorf("合法的任务分支应允许推送: %v", err)
	}
}

// G-02：每次工具调用都要进入事件表，且不得记录文件正文。
func TestToolCallsAreAuditedWithoutContent(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	secretContent := "hello-secret-payload\n"
	script := []backend.ToolCall{{Tool: "write_file", Args: map[string]any{
		"path": "hello.txt", "content": secretContent,
	}}}
	if _, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{script})).RunTask(task.ID); err != nil {
		t.Fatal(err)
	}

	calls := eventsOfType(t, f.st, task.ID, "tool_call")
	if len(calls) == 0 {
		t.Fatal("应记录工具调用事件")
	}
	var found bool
	for _, e := range calls {
		if e.Payload["tool"] != "write_file" {
			continue
		}
		found = true
		if e.Payload["path"] != "hello.txt" {
			t.Errorf("应记录写入路径: %+v", e.Payload)
		}
		if e.Payload["ok"] != true {
			t.Errorf("成功调用应标记 ok: %+v", e.Payload)
		}
		raw, _ := json.Marshal(e.Payload)
		if strings.Contains(string(raw), "hello-secret-payload") {
			t.Errorf("事件不应包含文件正文: %s", raw)
		}
		if _, ok := e.Payload["bytes"]; !ok {
			t.Errorf("应记录字节数而非内容: %+v", e.Payload)
		}
	}
	if !found {
		t.Errorf("没有找到 write_file 调用记录: %+v", calls)
	}
}

// G-03：提交信息包含任务 ID，PR 正文包含需求、验收矩阵、成本与报告路径。
func TestCommitMessageAndPRBodyContent(t *testing.T) {
	f := setup(t)
	task := f.task("S")

	message := commitMessage(task)
	if !strings.Contains(message, task.ID) {
		t.Errorf("提交信息应包含任务 ID: %q", message)
	}
	if !strings.Contains(message, "\n\nTask: "+task.ID) {
		t.Errorf("提交信息应有任务 trailer: %q", message)
	}

	plan := planning.Plan{
		Status:  planning.StatusReady,
		Summary: "把 + 号加入邮箱字符类",
		Scope:   planning.Scope{AllowedPaths: []string{"hello.txt"}},
		Steps:   []planning.Step{{ID: "s1", Description: "改文件", Files: []string{"hello.txt"}}},
		Acceptance: []planning.Acceptance{{
			ID: "ac1", Description: "测试通过",
			Check: planning.Check{Type: planning.CheckCommand, Name: "check"},
		}},
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	task.PlanJSON = string(planJSON)
	task.Source = "issue"
	task.SourceRef = "https://github.com/acme/app/issues/7"
	task.SpentUSD = 0.0123

	if err := f.st.RecordVerification(task.ID, 0, true, map[string]any{
		"passed": true,
		"outcomes": []any{map[string]any{
			"command": "go run check.go", "name": "check", "exit_code": float64(0),
		}},
		"acceptance": []any{map[string]any{
			"id": "ac1", "description": "测试通过", "passed": true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AppendEvent(task.ID, "plan_ready", map[string]any{"source": "model"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AppendEvent(task.ID, "review_result", map[string]any{
		"decision": "approve", "summary": "改动符合需求",
	}); err != nil {
		t.Fatal(err)
	}

	body := prBody(f.st, f.cfg, task)
	for _, want := range []string{
		task.ID,
		task.SourceRef,
		"把 + 号加入邮箱字符类",
		"hello.txt",
		"| 验收标准 | 结果 |",
		"满足",
		"`go run check.go`",
		"$0.0123",
		"运行报告",
		"模型生成",
		"approve",
		"不会自动合并",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR 正文缺少 %q:\n%s", want, body)
		}
	}
}
