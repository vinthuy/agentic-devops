package review

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentdev/internal/llm"
)

func TestValidateAcceptsValidApproveAndRequestChanges(t *testing.T) {
	if err := Validate(Review{Decision: Approve, Summary: "满足需求"}); err != nil {
		t.Errorf("approve 应合法: %v", err)
	}
	if err := Validate(Review{
		Decision: Approve, Summary: "满足需求，有一条低优先级提示",
		Findings: []Finding{{Path: "a.go", Line: 3, Severity: SeverityLow, Comment: "可抽函数"}},
	}); err != nil {
		t.Errorf("approve 带 low 级意见应合法: %v", err)
	}
	if err := Validate(Review{
		Decision: RequestChanges, Summary: "缺少边界检查",
		Findings: []Finding{{Path: "a.go", Line: 10, Severity: SeverityHigh, Comment: "索引可能越界"}},
	}); err != nil {
		t.Errorf("request_changes 应合法: %v", err)
	}
}

func TestValidateRejectsInconsistentOrIncompleteVerdicts(t *testing.T) {
	cases := []struct {
		name string
		r    Review
	}{
		{"未知 decision", Review{Decision: "maybe", Summary: "x"}},
		{"approve 无 summary", Review{Decision: Approve}},
		{"approve 但含 high 问题", Review{
			Decision: Approve, Summary: "ok",
			Findings: []Finding{{Path: "a.go", Severity: SeverityHigh, Comment: "SQL 注入"}},
		}},
		{"approve 但含 medium 问题", Review{
			Decision: Approve, Summary: "ok",
			Findings: []Finding{{Path: "a.go", Severity: SeverityMedium, Comment: "请求校验缺失"}},
		}},
		{"request_changes 无 finding", Review{Decision: RequestChanges, Summary: "不行"}},
		{"request_changes 只有 low", Review{
			Decision: RequestChanges, Summary: "不行",
			Findings: []Finding{{Path: "a.go", Severity: SeverityLow, Comment: "可改命名"}},
		}},
		{"finding 缺 path", Review{
			Decision: RequestChanges, Summary: "不行",
			Findings: []Finding{{Severity: SeverityHigh, Comment: "坏了"}},
		}},
		{"finding 缺 comment", Review{
			Decision: RequestChanges, Summary: "不行",
			Findings: []Finding{{Path: "a.go", Severity: SeverityHigh}},
		}},
		{"非法 severity", Review{
			Decision: RequestChanges, Summary: "不行",
			Findings: []Finding{{Path: "a.go", Severity: "critical", Comment: "坏了"}},
		}},
	}
	for _, c := range cases {
		if err := Validate(c.r); err == nil {
			t.Errorf("%s 应被拒绝: %+v", c.name, c.r)
		}
	}
}

func TestFormatIncludesFileAndLine(t *testing.T) {
	r := Review{
		Decision: RequestChanges,
		Summary:  "正则未覆盖加号",
		Findings: []Finding{
			{Path: "validators.py", Line: 3, Severity: SeverityHigh, Comment: "把 + 加入字符类"},
			{Path: "validators.py", Severity: SeverityMedium, Comment: "补充注释"},
		},
	}
	got := r.Format()
	for _, want := range []string{"validators.py:3", "[high]", "把 + 加入字符类", "[medium] validators.py "} {
		if !strings.Contains(got, want) {
			t.Errorf("Format 缺少 %q:\n%s", want, got)
		}
	}
}

func TestValidateFindingPathsMustExistInDiff(t *testing.T) {
	r := Review{
		Decision: RequestChanges, Summary: "问题",
		Findings: []Finding{{Path: "not-changed.go", Severity: SeverityHigh, Comment: "问题"}},
	}
	if err := Validate(r); err != nil {
		t.Fatalf("结论结构自身应合法: %v", err)
	}
	if err := validateFindingPaths(r, "--- a/changed.go\n+++ b/changed.go\n"); err == nil {
		t.Fatal("不得引用 diff 之外的文件")
	}
	if err := validateFindingPaths(r, "--- a/not-changed.go\n+++ b/not-changed.go\n"); err != nil {
		t.Errorf("diff 中的文件应允许: %v", err)
	}
}

func TestModelReviewerParsesStructuredToolCall(t *testing.T) {
	var seenUserContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		if len(req.Messages) > 0 {
			seenUserContent = req.Messages[0].Content
		}
		resp := map[string]any{
			"content": []any{map[string]any{
				"type": "tool_use", "id": "tu_1", "name": "submit_review",
				"input": map[string]any{
					"decision": "request_changes",
					"summary":  "未覆盖加号",
					"findings": []any{map[string]any{
						"path": "validators.py", "line": 3, "severity": "high", "comment": "加 +",
					}},
				},
			}},
			"stop_reason": "tool_use",
			"usage":       map[string]any{"input_tokens": 50, "output_tokens": 20},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	var reported llm.Usage
	rv := &ModelReviewer{
		Client: llm.NewClient(srv.URL, "k"), Model: "m", MaxTokens: 512,
		OnUsage: func(role string, u llm.Usage) {
			if role != "reviewer" {
				t.Errorf("角色名应为 reviewer，实际 %s", role)
			}
			reported = u
		},
	}
	verdict, err := rv.Review(context.Background(), Input{
		Requirement: "支持 + 号邮箱",
		Plan:        "修改 validators.py",
		Diff:        "diff --git a/validators.py b/validators.py\n--- a/validators.py\n+++ b/validators.py\n+_EMAIL = ...",
		Acceptance:  "[ac1] 满足",
		CIFailure:   "CI 失败：1 项检查失败（test）\n\nAssertionError: ValueError not raised",
	})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Approved() || len(verdict.Findings) != 1 {
		t.Errorf("结论解析错误: %+v", verdict)
	}
	if reported.InputTokens != 50 || reported.OutputTokens != 20 {
		t.Errorf("用量未上报: %+v", reported)
	}
	for _, tag := range []string{"<task_requirement", "<plan", "<acceptance_results", "<ci_failure", "<diff"} {
		if !strings.Contains(seenUserContent, tag) {
			t.Errorf("用户消息缺少 %s: %q", tag, seenUserContent)
		}
	}
	if !strings.Contains(seenUserContent, "ValueError not raised") {
		t.Errorf("CI 失败输出未进入上下文: %q", seenUserContent)
	}
}

func TestReviewerSeesAgentsFileAsUntrusted(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		if len(req.Messages) > 0 {
			seen = req.Messages[0].Content
		}
		resp := map[string]any{
			"content": []any{map[string]any{
				"type": "tool_use", "id": "tu_1", "name": "submit_review",
				"input": map[string]any{
					"decision": "approve", "summary": "可以", "findings": []any{},
				},
			}},
			"stop_reason": "tool_use",
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	_, err := (&ModelReviewer{
		Client: llm.NewClient(srv.URL, "k"), Model: "m", MaxTokens: 256,
	}).Review(context.Background(), Input{
		Requirement: "写 hello.txt",
		Diff:        "diff --git a/hello.txt b/hello.txt\n",
		AgentsMD:    "来源：仓库文件 AGENTS.md\n先写测试。",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "先写测试") || !strings.Contains(seen, `trusted="false"`) || !strings.Contains(seen, "AGENTS.md") {
		t.Fatalf("审查上下文应把仓库文件标成不可信: %q", seen)
	}
}
