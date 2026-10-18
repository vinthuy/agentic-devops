package planning

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

func names(items ...string) map[string]bool {
	out := map[string]bool{}
	for _, i := range items {
		out[i] = true
	}
	return out
}

func TestValidateAcceptsWellFormedPlan(t *testing.T) {
	plan := Plan{
		Status:  StatusReady,
		Summary: "修一个边界条件",
		Scope:   Scope{AllowedPaths: []string{"src/**"}},
		Acceptance: []Acceptance{
			{ID: "ac1", Description: "测试通过", Check: Check{Type: CheckCommand, Name: "test"}},
			{ID: "ac2", Description: "新增用例", Check: Check{Type: CheckTest, Name: "test", Selector: "tests/test_x.py::test_y"}},
			{ID: "ac3", Description: "文件存在", Check: Check{Type: CheckFileExists, Path: "src/a.py"}},
			{ID: "ac4", Description: "已移除临时开关", Check: Check{Type: CheckGrep, Path: "src/a.py", Pattern: "TEMP_", Expect: "absent"}},
		},
	}
	if err := Validate(plan, names("test")); err != nil {
		t.Fatalf("合法计划不应报错: %v", err)
	}
}

func TestValidateRejectsBadPlans(t *testing.T) {
	cases := map[string]Plan{
		"缺少验收标准":         {Status: StatusReady, Acceptance: nil},
		"needs_info 无问题": {Status: StatusNeedsInfo},
		"未知状态":           {Status: "whatever"},
		"引用不存在的命令": {Status: StatusReady, Acceptance: []Acceptance{
			{ID: "a", Description: "d", Check: Check{Type: CheckCommand, Name: "nope"}}}},
		"选择器含 shell 元字符": {Status: StatusReady, Acceptance: []Acceptance{
			{ID: "a", Description: "d", Check: Check{Type: CheckTest, Name: "test", Selector: "x; rm -rf /"}}}},
		"路径越界": {Status: StatusReady, Acceptance: []Acceptance{
			{ID: "a", Description: "d", Check: Check{Type: CheckFileExists, Path: "../../etc/passwd"}}}},
		"绝对路径": {Status: StatusReady, Acceptance: []Acceptance{
			{ID: "a", Description: "d", Check: Check{Type: CheckFileExists, Path: "C:/windows"}}}},
		"grep 正则非法": {Status: StatusReady, Acceptance: []Acceptance{
			{ID: "a", Description: "d", Check: Check{Type: CheckGrep, Path: "a.py", Pattern: "(unclosed", Expect: "present"}}}},
		"grep expect 非法": {Status: StatusReady, Acceptance: []Acceptance{
			{ID: "a", Description: "d", Check: Check{Type: CheckGrep, Path: "a.py", Pattern: "x", Expect: "maybe"}}}},
		"验收标准 id 重复": {Status: StatusReady, Acceptance: []Acceptance{
			{ID: "a", Description: "d", Check: Check{Type: CheckCommand, Name: "test"}},
			{ID: "a", Description: "e", Check: Check{Type: CheckCommand, Name: "test"}}}},
		"allowed_paths 越界": {Status: StatusReady, Scope: Scope{AllowedPaths: []string{"../outside/**"}},
			Acceptance: []Acceptance{{ID: "a", Description: "d", Check: Check{Type: CheckCommand, Name: "test"}}}},
	}
	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Validate(plan, names("test")); err == nil {
				t.Error("应当校验失败")
			}
		})
	}
}

func TestStaticPlannerUsesVerifyCommandsAsAcceptance(t *testing.T) {
	plan, err := StaticPlanner{}.Plan(context.Background(), Requirement{VerifyNames: []string{"lint", "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != StatusReady || len(plan.Acceptance) != 2 {
		t.Fatalf("计划异常: %+v", plan)
	}
	if err := Validate(plan, names("lint", "test")); err != nil {
		t.Errorf("静态计划必须自洽: %v", err)
	}
}

func TestModelPlannerParsesForcedToolCall(t *testing.T) {
	var sawToolChoice bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ToolChoice map[string]any `json:"tool_choice"`
			Tools      []any          `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)
		if req.ToolChoice["name"] == "submit_plan" && len(req.Tools) == 1 {
			sawToolChoice = true
		}
		io.WriteString(w, `{
			"content": [{"type":"tool_use","id":"t1","name":"submit_plan","input":{
				"status":"ready","summary":"新增校验",
				"acceptance":[{"id":"ac1","description":"测试通过","check":{"type":"test","name":"test","selector":"tests/x.py"}}]
			}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":11,"output_tokens":7}
		}`)
	}))
	defer srv.Close()

	var reported llm.Usage
	p := &ModelPlanner{
		Client: llm.NewClient(srv.URL, "k"), Model: "m", MaxTokens: 10,
		OnUsage: func(role string, u llm.Usage) { reported = u },
	}
	plan, err := p.Plan(context.Background(), Requirement{Text: "改 x", VerifyNames: []string{"test"}})
	if err != nil {
		t.Fatalf("规划失败: %v", err)
	}
	if !sawToolChoice {
		t.Error("必须以强制工具调用的方式请求计划")
	}
	if plan.Summary != "新增校验" || len(plan.Acceptance) != 1 {
		t.Errorf("计划解析错误: %+v", plan)
	}
	if reported.InputTokens != 11 {
		t.Errorf("用量未上报: %+v", reported)
	}
}

func TestModelPlannerRejectsInvalidPlan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{
			"content": [{"type":"tool_use","id":"t1","name":"submit_plan","input":{
				"status":"ready","summary":"x","acceptance":[]
			}}],
			"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	defer srv.Close()

	p := &ModelPlanner{Client: llm.NewClient(srv.URL, "k"), Model: "m", MaxTokens: 10}
	if _, err := p.Plan(context.Background(), Requirement{VerifyNames: []string{"test"}}); err == nil {
		t.Error("非法计划应当被拒绝")
	}
}

func TestParseAndPromptText(t *testing.T) {
	plan := Plan{
		Status:  StatusReady,
		Summary: "概要",
		Steps:   []Step{{ID: "s1", Description: "改文件", Files: []string{"a.py"}}},
		Acceptance: []Acceptance{
			{ID: "ac1", Description: "测试通过", Check: Check{Type: CheckCommand, Name: "test"}},
		},
		Scope: Scope{AllowedPaths: []string{"src/**"}},
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(string(data))
	if err != nil || got.Summary != "概要" {
		t.Fatalf("解析失败: %v %+v", err, got)
	}
	text := got.PromptText()
	for _, want := range []string{"概要", "改文件", "ac1", "src/**"} {
		if !strings.Contains(text, want) {
			t.Errorf("提示文本缺少 %q:\n%s", want, text)
		}
	}
	if empty, err := Parse(""); err != nil || empty.Summary != "" {
		t.Errorf("空计划应解析为零值: %v", err)
	}
	if _, err := Parse("{not json"); err == nil {
		t.Error("非法 JSON 应当报错")
	}
}
