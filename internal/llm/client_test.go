package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func fastRetries(c *Client) *Client {
	c.Retries = 3
	c.RetryBaseDelay = time.Millisecond
	return c
}

func TestMessagesSendsProtocolFields(t *testing.T) {
	var got struct {
		Model      string           `json:"model"`
		MaxTokens  int              `json:"max_tokens"`
		System     string           `json:"system"`
		Tools      []map[string]any `json:"tools"`
		ToolChoice map[string]any   `json:"tool_choice"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("路径异常: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") != Version {
			t.Errorf("请求头异常: %v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("请求体非法: %v", err)
		}
		io.WriteString(w, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":5,"cache_creation_input_tokens":6}}`)
	}))
	defer srv.Close()

	c := fastRetries(NewClient(srv.URL, "k"))
	resp, err := c.Messages(context.Background(), Request{
		Model: "m", MaxTokens: 100, System: "sys",
		Messages:   []any{map[string]any{"role": "user", "content": "hi"}},
		Tools:      []map[string]any{{"name": "t"}},
		ToolChoice: map[string]any{"type": "tool", "name": "t"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "m" || got.MaxTokens != 100 || got.System != "sys" {
		t.Errorf("请求字段缺失: %+v", got)
	}
	if len(got.Tools) != 1 || got.ToolChoice["name"] != "t" {
		t.Errorf("工具字段缺失: %+v", got)
	}
	if resp.StopReason != "end_turn" || len(resp.Content) != 1 {
		t.Errorf("响应解析错误: %+v", resp)
	}
	if resp.Usage.InputTokens != 3 || resp.Usage.CacheReadTokens != 5 || resp.Usage.CacheWriteTokens != 6 {
		t.Errorf("用量解析错误: %+v", resp.Usage)
	}
}

func TestMessagesRetriesOnServerError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`)
	}))
	defer srv.Close()

	c := fastRetries(NewClient(srv.URL, "k"))
	_, err := c.Messages(context.Background(), Request{Model: "m", MaxTokens: 1, Messages: []any{}})
	if err != nil {
		t.Fatalf("应重试成功: %v", err)
	}
	if calls != 3 {
		t.Errorf("应重试至第 3 次，实际 %d", calls)
	}
}

func TestMessagesDoesNotRetryClientError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"bad model"}`)
	}))
	defer srv.Close()

	c := fastRetries(NewClient(srv.URL, "k"))
	_, err := c.Messages(context.Background(), Request{Model: "m", MaxTokens: 1})
	if err == nil {
		t.Fatal("应返回错误")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Retryable || apiErr.Status != http.StatusBadRequest {
		t.Errorf("错误类型异常: %#v", err)
	}
	if calls != 1 {
		t.Errorf("4xx 不应重试，实际 %d 次", calls)
	}
}

func TestMessagesRespectsCanceledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := fastRetries(NewClient(srv.URL, "k"))
	if _, err := c.Messages(ctx, Request{Model: "m", MaxTokens: 1}); err == nil {
		t.Error("已取消的上下文应返回错误")
	}
}

// DeepSeek 的 thinking 模式会拒绝强制 tool_choice，客户端应去掉该参数重发并记住。
func TestMessagesFallsBackWhenToolChoiceUnsupported(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		if _, sent := b["tool_choice"]; sent {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Thinking mode does not support this tool_choice","type":"invalid_request_error"}}`)
			return
		}
		io.WriteString(w, `{"content":[{"type":"tool_use","name":"t","input":{"ok":true}}],"stop_reason":"tool_use","usage":{"input_tokens":1}}`)
	}))
	defer srv.Close()

	c := fastRetries(NewClient(srv.URL, "k"))
	req := Request{
		Model: "m", MaxTokens: 1, Messages: []any{map[string]any{"role": "user", "content": "hi"}},
		ToolChoice: map[string]any{"type": "tool", "name": "t"},
	}
	if _, err := c.Messages(context.Background(), req); err != nil {
		t.Fatalf("去掉 tool_choice 后应成功: %v", err)
	}
	if !c.ToolChoiceDisabled() {
		t.Error("应记住该端点不支持强制工具调用")
	}
	if len(bodies) != 2 {
		t.Fatalf("应请求两次，实际 %d", len(bodies))
	}
	if _, sent := bodies[1]["tool_choice"]; sent {
		t.Error("第二次请求不应再带 tool_choice")
	}
	// 记住能力后，后续请求一次就够，不再白跑。
	before := len(bodies)
	if _, err := c.Messages(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != before+1 {
		t.Errorf("已记住能力后应只请求一次，实际新增 %d", len(bodies)-before)
	}
	if _, sent := bodies[len(bodies)-1]["tool_choice"]; sent {
		t.Error("已记住能力后不应再带 tool_choice")
	}
}

// 与 tool_choice 无关的 4xx 必须原样暴露，不能被当成能力缺失绕过去。
func TestMessagesDoesNotSwallowUnrelatedToolChoiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"tool_choice: invalid tool name 'nope'","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()

	c := fastRetries(NewClient(srv.URL, "k"))
	_, err := c.Messages(context.Background(), Request{
		Model: "m", MaxTokens: 1, ToolChoice: map[string]any{"type": "tool", "name": "nope"},
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if c.ToolChoiceDisabled() {
		t.Error("不应把参数错误标记为能力缺失")
	}
}

func TestAPIErrorQuota(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{402, `{"error":"payment required"}`, true},
		{400, `{"message":"Insufficient Balance"}`, true},
		{429, `{"message":"rate limit exceeded"}`, false},
		{500, `{"message":"boom"}`, false},
	}
	for _, c := range cases {
		got := (&APIError{Status: c.status, Body: c.body}).Quota()
		if got != c.want {
			t.Errorf("Quota(%d, %q) = %v, 期望 %v", c.status, c.body, got, c.want)
		}
	}
}

func TestAPIErrorUnsupportedToolChoice(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{400, `Thinking mode does not support this tool_choice`, true},
		{400, `tool_choice is not supported`, true},
		{400, `unsupported tool_choice`, true},
		{400, `unknown model`, false},
		{400, `tool_choice: invalid tool name`, false},
		{500, `does not support this tool_choice`, false},
	}
	for _, c := range cases {
		got := (&APIError{Status: c.status, Body: c.body}).UnsupportedToolChoice()
		if got != c.want {
			t.Errorf("UnsupportedToolChoice(%d, %q) = %v, 期望 %v", c.status, c.body, got, c.want)
		}
	}
}

// 模型用文本回答时应追加一次提示，而不是直接判定失败。
func TestCallToolNudgesThenSucceeds(t *testing.T) {
	var calls int
	var lastBody struct {
		Messages []map[string]any `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &lastBody)
		if calls == 1 {
			io.WriteString(w, `{"content":[{"type":"text","text":"我打算这么做"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
			return
		}
		io.WriteString(w, `{"content":[{"type":"tool_use","name":"submit_plan","input":{"status":"ready"}}],"stop_reason":"tool_use","usage":{"input_tokens":20,"output_tokens":7}}`)
	}))
	defer srv.Close()

	c := fastRetries(NewClient(srv.URL, "k"))
	resp, input, err := c.CallTool(context.Background(), Request{
		Model: "m", MaxTokens: 1, Messages: []any{map[string]any{"role": "user", "content": "hi"}},
	}, "submit_plan")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("应先失败提示再成功，实际 %d 次", calls)
	}
	if input["status"] != "ready" {
		t.Errorf("工具入参错误: %+v", input)
	}
	// 第二次请求应带上模型上一轮的文本回答和提示。
	if len(lastBody.Messages) != 3 {
		t.Errorf("提示轮消息数应为 3，实际 %d", len(lastBody.Messages))
	}
	// 两次尝试的用量都要计入，避免降级路径漏账。
	if resp.Usage.InputTokens != 30 || resp.Usage.OutputTokens != 12 {
		t.Errorf("用量应累加: %+v", resp.Usage)
	}
}

func TestCallToolFailsWhenModelNeverCallsTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"content":[{"type":"text","text":"见上文"}],"stop_reason":"end_turn","usage":{"input_tokens":1}}`)
	}))
	defer srv.Close()

	c := fastRetries(NewClient(srv.URL, "k"))
	_, _, err := c.CallTool(context.Background(), Request{Model: "m", MaxTokens: 1}, "submit_plan")
	if err == nil {
		t.Fatal("应返回错误")
	}
}

func TestParseBlock(t *testing.T) {
	raw := json.RawMessage(`{"type":"tool_use","id":"t1","name":"write_file","input":{"path":"a"}}`)
	b, err := ParseBlock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if b.Type != "tool_use" || b.Name != "write_file" || b.Input["path"] != "a" {
		t.Errorf("块解析错误: %+v", b)
	}
}
