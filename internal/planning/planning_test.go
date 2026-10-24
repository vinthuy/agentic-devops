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

// 需求声称“缺失”且现有测试已有同名引用时，证据必须真的进入提示：规划器看不到仓库，
// 没有文件内容就无法判断既有断言覆盖了什么，只能退回“产出一份新测试”。
func TestModelPlannerIncludesExistingCoverageEvidence(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		// Go 的 json 编码会把 < > 转义成 \u003c，这里还原后再断言提示内容。
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &req); err == nil && len(req.Messages) > 0 {
			body = req.Messages[0].Content
		}
		io.WriteString(w, `{
			"content": [{"type":"tool_use","id":"t1","name":"submit_plan","input":{
				"status":"needs_info","summary":"前提不成立",
				"questions":["cost_test.go:12 已覆盖缓存计价，请确认增量"],
				"acceptance":[]
			}}],
			"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	defer srv.Close()

	p := &ModelPlanner{Client: llm.NewClient(srv.URL, "k"), Model: "m", MaxTokens: 10}
	_, err := p.Plan(context.Background(), Requirement{
		Text:             "为 Table.Cost 补充缓存计价测试",
		VerifyNames:      []string{"test"},
		ExistingCoverage: []CoverageHit{{Identifier: "Table.Cost", File: "internal/cost/cost_test.go", Line: 12}},
		CoverageFiles:    map[string]string{"internal/cost/cost_test.go": "usd, ok := tbl.Cost(\"m\", u)\n"},
	})
	if err != nil {
		t.Fatalf("规划失败: %v", err)
	}
	for _, want := range []string{"<existing_coverage", "internal/cost/cost_test.go:12", "tbl.Cost"} {
		if !strings.Contains(body, want) {
			t.Errorf("提示中缺少 %q，规划器无法判断前提是否成立", want)
		}
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
