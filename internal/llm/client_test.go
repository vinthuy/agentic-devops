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
