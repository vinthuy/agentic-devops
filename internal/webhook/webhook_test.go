package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/config"
	"agentdev/internal/state"
	"agentdev/internal/store"
)

func TestBadSignatureLeavesTheTaskAlone(t *testing.T) {
	st := openStore(t)
	task := park(t, st, "https://github.com/acme/app/issues/7")
	h := handler(st, allow(true))

	rec := post(t, h, "issue_comment", "d1", "wrong-secret", commentBody("/approve", 7, 99, "vinthuy"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("签名错误应返回 401，实际 %d", rec.Code)
	}
	got := mustTask(t, st, task.ID)
	if got.Status != state.AwaitingApproval {
		t.Fatalf("签名错误不得改状态: %s", got.Status)
	}
}

func TestApproveCommentEntersCodingAndDuplicateDeliveryDoesNot(t *testing.T) {
	st := openStore(t)
	task := park(t, st, "https://github.com/acme/app/issues/7")
	h := handler(st, allow(true))
	body := commentBody("/approve", 7, 99, "vinthuy")

	rec := post(t, h, "issue_comment", "d2", secret, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("批准应被接受，实际 %d %s", rec.Code, rec.Body.String())
	}
	got := mustTask(t, st, task.ID)
	if got.Status != state.Coding {
		t.Fatalf("协作者 /approve 应进入 CODING: %s", got.Status)
	}
	version := got.Version

	again := post(t, h, "issue_comment", "d2", secret, body)
	if again.Code != http.StatusOK {
		t.Fatalf("重复投递应返回 200，实际 %d", again.Code)
	}
	got = mustTask(t, st, task.ID)
	if got.Status != state.Coding || got.Version != version {
		t.Fatalf("重复投递不得再次转移: status=%s version=%d", got.Status, got.Version)
	}
}

func TestAgentReadyQueuesOneTaskAndDoesNotCode(t *testing.T) {
	st := openStore(t)
	h := handler(st, allow(true))
	body := labelBody("agent-ready", 8, "修好登录", "邮箱加号失败")

	rec := post(t, h, "issues", "d3", secret, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("接入应返回 200，实际 %d %s", rec.Code, rec.Body.String())
	}
	tasks, err := st.ListTasks("", "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("应只创建 1 个任务，实际 %d", len(tasks))
	}
	task := tasks[0]
	if task.Status != state.Queued || task.Source != "issue" {
		t.Fatalf("标签只应入队: status=%s source=%s", task.Status, task.Source)
	}
	if !strings.Contains(task.Requirement, "修好登录") || !strings.Contains(task.Requirement, "邮箱加号失败") {
		t.Fatalf("需求应含标题和正文: %q", task.Requirement)
	}
	if task.SourceRef != "https://github.com/acme/app/issues/8" {
		t.Fatalf("来源引用: %s", task.SourceRef)
	}

	again := post(t, h, "issues", "d4", secret, body)
	if again.Code != http.StatusOK {
		t.Fatalf("再次打标签应返回 200，实际 %d", again.Code)
	}
	tasks, err = st.ListTasks("", "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Status != state.Queued {
		t.Fatalf("已有未结束任务时不得再创建或进入编码: %+v", tasks)
	}
}

func TestOtherLabelDoesNotCreateATask(t *testing.T) {
	st := openStore(t)
	h := handler(st, allow(true))
	rec := post(t, h, "issues", "d5", secret, labelBody("bug", 8, "修好登录", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("其他标签应被忽略，实际 %d", rec.Code)
	}
	tasks, err := st.ListTasks("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("bug 标签不得创建任务: %+v", tasks)
	}
}

const secret = "test-secret"

func handler(st *store.SQLiteStore, allow func(context.Context, string, string) (bool, error)) *Handler {
	return &Handler{
		Store: st,
		Repos: map[string]config.RepoConfig{
			"acme/app": {BudgetUSD: 2},
		},
		Secret: secret,
		Allow:  allow,
	}
}

func allow(ok bool) func(context.Context, string, string) (bool, error) {
	return func(context.Context, string, string) (bool, error) { return ok, nil }
}

func openStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func park(t *testing.T, st *store.SQLiteStore, ref string) store.Task {
	t.Helper()
	task, err := st.CreateTask(store.NewTask{
		Repo: "acme/app", Requirement: "修好登录", BudgetUSD: 2,
		Source: "issue", SourceRef: ref, Title: "修好登录",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []state.Status{state.Triaging, state.Planning, state.AwaitingApproval} {
		task, err = st.Transition(task.ID, task.Version, next, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	return task
}

func mustTask(t *testing.T, st *store.SQLiteStore, id string) store.Task {
	t.Helper()
	task, err := st.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func commentBody(text string, number, commentID int64, login string) []byte {
	payload := map[string]any{
		"action": "created",
		"issue": map[string]any{
			"number":   number,
			"html_url": "https://github.com/acme/app/issues/7",
		},
		"comment": map[string]any{
			"id":   commentID,
			"body": text,
			"user": map[string]any{"login": login},
		},
		"repository": map[string]any{"full_name": "acme/app"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return raw
}

func labelBody(label string, number int64, title, issueBody string) []byte {
	payload := map[string]any{
		"action": "labeled",
		"label":  map[string]any{"name": label},
		"issue": map[string]any{
			"number":   number,
			"title":    title,
			"body":     issueBody,
			"html_url": "https://github.com/acme/app/issues/8",
		},
		"repository": map[string]any{"full_name": "acme/app"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return raw
}

func post(t *testing.T, h http.Handler, event, delivery, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", sign(body, key))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sign(body []byte, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
