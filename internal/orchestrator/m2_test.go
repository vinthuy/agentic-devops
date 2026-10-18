package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentdev/internal/backend"
	"agentdev/internal/config"
	"agentdev/internal/delivery"
	"agentdev/internal/review"
	"agentdev/internal/state"
	"agentdev/internal/store"
)

// stubReviewer 按顺序返回预设结论，用于测试审查分支。
type stubReviewer struct {
	reviews []review.Review
	err     error
	calls   int
}

func (s *stubReviewer) Review(_ context.Context, _ review.Input) (review.Review, error) {
	s.calls++
	if s.err != nil {
		return review.Review{}, s.err
	}
	if len(s.reviews) == 0 {
		return review.Review{Decision: review.Approve, Summary: "默认通过"}, nil
	}
	idx := s.calls - 1
	if idx >= len(s.reviews) {
		idx = len(s.reviews) - 1
	}
	return s.reviews[idx], nil
}

func approveReview() review.Review {
	return review.Review{Decision: review.Approve, Summary: "改动满足需求"}
}

func changesReview() review.Review {
	return review.Review{
		Decision: review.RequestChanges,
		Summary:  "实现与需求不一致",
		Findings: []review.Finding{{
			Path: "hello.txt", Line: 1, Severity: review.SeverityHigh,
			Comment: "内容应为 hello，请按需求修正",
		}},
	}
}

func hasEventWithPayload(t *testing.T, st *store.SQLiteStore, taskID, kind, key string, want any) bool {
	t.Helper()
	for _, e := range eventsOfType(t, st, taskID, kind) {
		if e.Payload[key] == want {
			return true
		}
	}
	return false
}

// ---- REVIEWING ----

func TestReviewApproveReachesDone(t *testing.T) {
	f := setup(t)
	f.cfg.Review.Enabled = true
	task := f.task("S")
	rv := &stubReviewer{reviews: []review.Review{approveReview()}}

	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}), WithReviewer(rv)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("审查通过后应为 DONE，实际 %s", done.Status)
	}
	if rv.calls != 1 {
		t.Errorf("Reviewer 应被调用 1 次，实际 %d", rv.calls)
	}
	if !hasEventWithPayload(t, f.st, task.ID, "state_changed", "review", "approve") {
		t.Error("应记录审查通过的转换事件")
	}
}

// 审查不通过必须真的回到编码，并记录可用以修复的意见。
func TestReviewRequestChangesReturnsToCoding(t *testing.T) {
	f := setup(t)
	f.cfg.Review.Enabled = true
	f.cfg.Policy.MaxReviewAttempts = 1
	task := f.task("S")
	rv := &stubReviewer{reviews: []review.Review{changesReview(), approveReview()}}

	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}), WithReviewer(rv)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("第二次审查通过后应为 DONE，实际 %s", done.Status)
	}
	if done.ReviewAttempts != 1 {
		t.Errorf("审查次数应为 1，实际 %d", done.ReviewAttempts)
	}
	fixes := eventsOfType(t, f.st, task.ID, "fix_requested")
	if len(fixes) != 1 {
		t.Fatalf("应记录 1 次修复请求，实际 %d", len(fixes))
	}
	if fixes[0].Payload["source"] != "reviewer" {
		t.Errorf("修复请求来源应为 reviewer: %+v", fixes[0].Payload)
	}
	if s, _ := fixes[0].Payload["failure"].(string); !strings.Contains(s, "hello.txt") {
		t.Errorf("修复请求应包含具体文件位置: %q", s)
	}
}

func TestReviewLimitSuspendsForHuman(t *testing.T) {
	f := setup(t)
	f.cfg.Review.Enabled = true
	f.cfg.Policy.MaxReviewAttempts = 0
	task := f.task("S")

	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithReviewer(&stubReviewer{reviews: []review.Review{changesReview()}})).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("达上限后应挂起，实际 %s", done.Status)
	}
	if reason := lastReason(t, f.st, task.ID); !strings.Contains(reason, "审查未通过") {
		t.Errorf("挂起原因应说明审查未通过: %q", reason)
	}
}

// 审查本身失败不等于代码有问题：应挂起等人工，而不是回炉改代码。
func TestReviewFailureSuspendsInsteadOfRejecting(t *testing.T) {
	f := setup(t)
	f.cfg.Review.Enabled = true
	task := f.task("S")

	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithReviewer(&stubReviewer{err: context.DeadlineExceeded})).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("审查失败应挂起，实际 %s", done.Status)
	}
	if len(eventsOfType(t, f.st, task.ID, "review_failed")) != 1 {
		t.Error("应记录 review_failed 事件")
	}
	if len(eventsOfType(t, f.st, task.ID, "fix_requested")) != 0 {
		t.Error("审查失败不应产生修复请求")
	}
}

func TestOrchestratorValidatesCustomReviewerVerdict(t *testing.T) {
	f := setup(t)
	f.cfg.Review.Enabled = true
	task := f.task("S")
	invalid := review.Review{
		Decision: review.Approve,
		Summary:  "通過",
		Findings: []review.Finding{{
			Path: "hello.txt", Line: 1, Severity: review.SeverityHigh, Comment: "存在阻塞问题",
		}},
	}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithReviewer(&stubReviewer{reviews: []review.Review{invalid}})).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("不一致的自定义 Reviewer 结果应挂起，实际 %s", done.Status)
	}
	if len(eventsOfType(t, f.st, task.ID, "review_failed")) != 1 {
		t.Error("应记录 Reviewer 结果校验失败")
	}
	if len(eventsOfType(t, f.st, task.ID, "review_result")) != 0 {
		t.Error("不合法的 verdict 不应被记录成正式审查结论")
	}
}

func TestReviewDisabledKeepsM1Flow(t *testing.T) {
	f := setup(t)
	f.cfg.Review.Enabled = false
	task := f.task("S")
	rv := &stubReviewer{}
	if _, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}), WithReviewer(rv)).RunTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if rv.calls != 0 {
		t.Errorf("未启用审查时不应调用 Reviewer，实际 %d 次", rv.calls)
	}
}

// ---- CI_WATCHING ----

func withDeliveryFixture(t *testing.T) *fixture {
	t.Helper()
	f := setup(t)
	addBareRemote(t, f.repo)
	f.cfg.Delivery = config.DeliveryConfig{
		Enabled: true, BranchPrefix: "agent/", DraftPR: true,
		CIWatch: true, MaxCIAttempts: 1, CITimeoutMinutes: 30, CIPollSeconds: 60,
	}
	return f
}

func TestCIPassedMarksDone(t *testing.T) {
	f := withDeliveryFixture(t)
	task := f.task("S")
	ci := &delivery.FakeCI{Snapshots: []delivery.CISnapshot{
		{State: delivery.CIPending, Summary: "1 项仍在运行"},
		{State: delivery.CIPassed, Summary: "1 项检查全部通过", Checks: 1},
	}}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithCodeHost(&delivery.Fake{}), WithCIWatcher(ci), WithCITiming(0, 30*time.Minute)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("CI 通过后应为 DONE，实际 %s", done.Status)
	}
	if !hasEventWithPayload(t, f.st, task.ID, "state_changed", "ci", "passed") {
		t.Error("应记录 ci=passed")
	}
	if ci.Calls != 2 {
		t.Errorf("应轮询到通过为止（2 次），实际 %d", ci.Calls)
	}
}

func TestCINoneMarksDoneWithExplicitNote(t *testing.T) {
	f := withDeliveryFixture(t)
	task := f.task("S")
	ci := &delivery.FakeCI{Snapshots: []delivery.CISnapshot{{State: delivery.CINone, Summary: "PR 上没有 CI 检查"}}}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithCodeHost(&delivery.Fake{}), WithCIWatcher(ci), WithCITiming(0, 30*time.Minute)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("无 CI 时应视为完成，实际 %s", done.Status)
	}
	if !hasEventWithPayload(t, f.st, task.ID, "state_changed", "ci", "none") {
		t.Error("应显式记录未运行 CI，而不是静默通过")
	}
}

func TestCIFailedReturnsToCodingAndCounts(t *testing.T) {
	f := withDeliveryFixture(t)
	task := f.task("S")
	ci := &delivery.FakeCI{Snapshots: []delivery.CISnapshot{{
		State: delivery.CIFailed, Summary: "1 项检查失败（build）",
		Logs: "build failed: undefined symbol", Checks: 1,
	}}}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithCodeHost(&delivery.Fake{}), WithCIWatcher(ci), WithCITiming(0, 30*time.Minute)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.CIAttempts != 1 {
		t.Errorf("CI 失败次数应为 1，实际 %d", done.CIAttempts)
	}
	if len(eventsOfType(t, f.st, task.ID, "ci_failed")) != 1 {
		t.Fatal("应记录 ci_failed 事件")
	}
	fixes := eventsOfType(t, f.st, task.ID, "fix_requested")
	if len(fixes) != 1 || fixes[0].Payload["source"] != "ci" {
		t.Fatalf("应产生来自 CI 的修复请求: %+v", fixes)
	}
	if s, _ := fixes[0].Payload["failure"].(string); !strings.Contains(s, "undefined symbol") {
		t.Errorf("修复请求应包含 CI 日志: %q", s)
	}
}

func TestCIExceedingLimitSuspends(t *testing.T) {
	f := withDeliveryFixture(t)
	f.cfg.Delivery.MaxCIAttempts = 0
	task := f.task("S")
	ci := &delivery.FakeCI{Snapshots: []delivery.CISnapshot{{State: delivery.CIFailed, Summary: "build 失败"}}}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithCodeHost(&delivery.Fake{}), WithCIWatcher(ci), WithCITiming(0, 30*time.Minute)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("超过 CI 上限应挂起，实际 %s", done.Status)
	}
	if reason := lastReason(t, f.st, task.ID); !strings.Contains(reason, "CI 失败") {
		t.Errorf("挂起原因应说明 CI 失败: %q", reason)
	}
}

func TestCIPendingTimesOut(t *testing.T) {
	f := withDeliveryFixture(t)
	task := f.task("S")
	ci := &delivery.FakeCI{Snapshots: []delivery.CISnapshot{{State: delivery.CIPending, Summary: "仍在运行"}}}
	// 轮询间隔为 0，超时为负 → 立即判定超时。
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithCodeHost(&delivery.Fake{}), WithCIWatcher(ci), WithCITiming(0, -time.Nanosecond)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("CI 超时应挂起，实际 %s", done.Status)
	}
	if reason := lastReason(t, f.st, task.ID); !strings.Contains(reason, "CI") {
		t.Errorf("挂起原因应说明 CI 超时: %q", reason)
	}
}

// ---- request_info ----

func TestRequestInfoPausesForClarification(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	request := []backend.ToolCall{
		{Tool: "write_file", Args: map[string]any{"path": "hello.txt", "content": "hello\n"}},
		{
			Tool: "request_info",
			Args: map[string]any{"questions": []any{"目标分支是哪个？", "是否需要兼容旧格式？"}},
		},
	}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{request})).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsInfo {
		t.Fatalf("请求澄清后应进入 NEEDS_INFO，实际 %s", done.Status)
	}
	if _, err := os.Stat(filepath.Join(done.WorktreePath, "hello.txt")); !os.IsNotExist(err) {
		t.Errorf("request_info 与写操作同批出现时，必须优先澄清、不得先写文件: %v", err)
	}
	questions := eventsOfType(t, f.st, task.ID, "state_changed")
	var found bool
	for _, e := range questions {
		if qs, ok := e.Payload["questions"].([]any); ok && len(qs) == 2 {
			found = true
		}
	}
	if !found {
		t.Error("状态转换应携带澄清问题")
	}
	interactions, err := f.st.ListInteractions(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(interactions) != 1 || interactions[0].Kind != "question" {
		t.Errorf("应把澄清问题写入 interactions: %+v", interactions)
	}
}
