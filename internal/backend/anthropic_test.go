package backend

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/config"
	"agentdev/internal/llm"
	"agentdev/internal/policy"
	"agentdev/internal/tools"
)

func newTools(t *testing.T) (*tools.Runtime, string) {
	t.Helper()
	root := t.TempDir()
	repo := config.RepoConfig{LocalPath: root, VerifyCommands: config.VerifyCommands{{Name: "v", Command: "go version"}}}
	p := config.PolicyConfig{DenyPaths: config.DefaultDenyPaths, DenyCommandPatterns: config.DefaultDenyCommandPatterns}
	eng, err := policy.NewEngine(p, repo, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tools.New(eng, 8000, 60, nil), root
}

func TestUserMessageListsConfiguredCommands(t *testing.T) {
	rt, root := newTools(t)
	message := UserMessage(StepContext{Requirement: "check this", Plan: "verify", Worktree: root, Tools: rt})
	if !strings.Contains(message, "<allowed_commands trusted=\"true\">") ||
		!strings.Contains(message, "v: go version") ||
		!strings.Contains(message, "除上表之外") {
		t.Fatalf("allowed commands missing from model context:\n%s", message)
	}
}

func TestAnthropicRoundTripKeepsThinkingAndPairsToolResults(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("路径异常: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "k-test" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("认证头缺失")
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
			return
		}
		calls++
		w.Header().Set("content-type", "application/json")
		switch calls {
		case 1:
			io.WriteString(w, `{
				"content": [
					{"type":"thinking","thinking":"先写文件","signature":"sig-123"},
					{"type":"tool_use","id":"tu_1","name":"write_file","input":{"path":"hello.txt","content":"hello\n"}}
				],
				"stop_reason":"tool_use",
				"usage":{"input_tokens":10,"output_tokens":5}
			}`)
		default:
			// 第二次请求必须包含上一轮原样回传的思考块，以及配对的 tool_result。
			if len(req.Messages) != 3 {
				t.Errorf("第二次请求应有 3 条消息，实际 %d", len(req.Messages))
			}
			assistant := string(req.Messages[1].Content)
			if !containsStr(assistant, `"signature":"sig-123"`) || !containsStr(assistant, `"type":"thinking"`) {
				t.Errorf("思考块未原样回传: %s", assistant)
			}
			user := string(req.Messages[2].Content)
			if !containsStr(user, `"tool_use_id":"tu_1"`) || !containsStr(user, `"type":"tool_result"`) {
				t.Errorf("tool_result 未正确配对: %s", user)
			}
			io.WriteString(w, `{
				"content": [{"type":"text","text":"完成"}],
				"stop_reason":"end_turn",
				"usage":{"input_tokens":20,"output_tokens":3}
			}`)
		}
	}))
	defer srv.Close()

	rt, root := newTools(t)
	be := NewAnthropic(llm.NewClient(srv.URL, "k-test"), "deepseek-v4-pro", 1024)
	res, err := be.RunStep(StepContext{
		TaskID: "t1", Requirement: "写 hello", Plan: "写 hello", Worktree: root,
		Tools: rt, MaxTurns: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalText != "完成" || res.Turns != 2 {
		t.Errorf("结果异常: %+v", res)
	}
	if res.Usage.InputTokens != 30 || res.Usage.OutputTokens != 8 {
		t.Errorf("用量累计异常: %+v", res.Usage)
	}
	data, err := os.ReadFile(filepath.Join(root, "hello.txt"))
	if err != nil || string(data) != "hello\n" {
		t.Errorf("工具未实际执行: %v %q", err, data)
	}
}

func TestAnthropicRetriesOnRateLimit(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"rate limited"}`)
			return
		}
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	rt, root := newTools(t)
	be := NewAnthropic(llm.NewClient(srv.URL, "k"), "m", 10)
	res, err := be.RunStep(StepContext{TaskID: "t", Requirement: "x", Plan: "x", Worktree: root, Tools: rt, MaxTurns: 2})
	if err != nil {
		t.Fatalf("限流后应重试成功: %v", err)
	}
	if res.FinalText != "ok" || calls != 2 {
		t.Errorf("重试结果异常: text=%q calls=%d", res.FinalText, calls)
	}
}

func TestAnthropicClientErrorIsNotRetried(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"bad model"}`)
	}))
	defer srv.Close()

	rt, root := newTools(t)
	be := NewAnthropic(llm.NewClient(srv.URL, "k"), "bad", 10)
	_, err := be.RunStep(StepContext{TaskID: "t", Requirement: "x", Plan: "x", Worktree: root, Tools: rt, MaxTurns: 2})
	if !IsBackendError(err) {
		t.Errorf("400 应当返回可恢复的后端错误: %v", err)
	}
	if calls != 1 {
		t.Errorf("4xx 不应重试，实际调用 %d 次", calls)
	}
}

func TestAnthropicMaxTurnsExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"content":[{"type":"tool_use","id":"tu","name":"list_dir","input":{"path":"."}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	rt, root := newTools(t)
	be := NewAnthropic(llm.NewClient(srv.URL, "k"), "m", 10)
	_, err := be.RunStep(StepContext{TaskID: "t", Requirement: "x", Plan: "x", Worktree: root, Tools: rt, MaxTurns: 2})
	if !IsBackendError(err) {
		t.Errorf("超过最大轮数应当返回可恢复错误: %v", err)
	}
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}
