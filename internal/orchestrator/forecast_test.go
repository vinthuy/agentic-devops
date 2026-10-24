package orchestrator

import (
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/state"
	"agentdev/internal/store"
)

// seedSettledSpend 造一个已结算（DONE）任务并写入花费，作为预测样本。
func seedSettledSpend(t *testing.T, st *store.SQLiteStore, repo, level string, cost float64) {
	t.Helper()
	task, err := st.CreateTask(store.NewTask{Repo: repo, Requirement: "x", BudgetUSD: 5})
	if err != nil {
		t.Fatal(err)
	}
	path := []state.Status{
		state.Triaging, state.Planning, state.Coding, state.Verifying,
		state.Reviewing, state.Committing, state.Done,
	}
	for i, next := range path {
		var fields map[string]any
		if i == 0 {
			fields = map[string]any{"level": level}
		}
		task, err = st.Transition(task.ID, task.Version, next, nil, fields)
		if err != nil {
			t.Fatalf("转换到 %s 失败: %v", next, err)
		}
	}
	if err := st.AddSpend(task.ID, cost); err != nil {
		t.Fatal(err)
	}
}

func findEvent(t *testing.T, st *store.SQLiteStore, taskID, eventType string) (store.Event, bool) {
	t.Helper()
	events, err := st.ListEvents(taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == eventType {
			return e, true
		}
	}
	return store.Event{}, false
}

// 样本足够时，计划阶段应写入带 P50/P90 的预测事件。
func TestPlanRecordsCostForecastWhenSamplesSuffice(t *testing.T) {
	f := setup(t)
	for _, c := range []float64{0.01, 0.02, 0.03, 0.04, 0.05, 0.06} {
		seedSettledSpend(t, f.st, "acme/app", "S", c)
	}
	task := f.task("S")
	done := f.run(backend.NewFake([][]backend.ToolCall{good}), task.ID)
	if done.Status != state.Done {
		t.Fatalf("任务应完成，实际 %s", done.Status)
	}

	event, ok := findEvent(t, f.st, task.ID, "cost_forecast")
	if !ok {
		t.Fatal("应写入 cost_forecast 事件")
	}
	p50, _ := event.Payload["p50_usd"].(float64)
	p90, _ := event.Payload["p90_usd"].(float64)
	if p50 <= 0 || p90 < p50 {
		t.Errorf("预测值不合理: p50=%v p90=%v", p50, p90)
	}
	if basis, _ := event.Payload["basis"].(string); basis != "repo+level" {
		t.Errorf("预测来源应为 repo+level，实际 %q", basis)
	}
}

// 样本不足时不写预测值，但仍留一条记录说明原因，避免事后误以为漏记。
func TestPlanRecordsSkippedForecastWhenSamplesInsufficient(t *testing.T) {
	f := setup(t)
	seedSettledSpend(t, f.st, "acme/app", "S", 0.01)
	task := f.task("S")
	f.run(backend.NewFake([][]backend.ToolCall{good}), task.ID)

	if _, ok := findEvent(t, f.st, task.ID, "cost_forecast"); ok {
		t.Error("样本不足时不应写入预测值")
	}
	event, ok := findEvent(t, f.st, task.ID, "cost_forecast_skipped")
	if !ok {
		t.Fatal("应记录预测被跳过及其原因")
	}
	if reason, _ := event.Payload["reason"].(string); reason == "" {
		t.Error("跳过记录应包含原因")
	}
}

// 重规划不能覆盖首次预测：否则事后校准会拿一个已经看到花费的预测去对账。
func TestRecordCostForecastIsNotOverwritten(t *testing.T) {
	f := setup(t)
	for _, c := range []float64{0.01, 0.02, 0.03, 0.04, 0.05} {
		seedSettledSpend(t, f.st, "acme/app", "S", c)
	}
	task := f.task("S")
	o := New(f.st, f.cfg, nil)
	o.recordCostForecast(task)

	first, ok := findEvent(t, f.st, task.ID, "cost_forecast")
	if !ok {
		t.Fatal("首次应写入预测")
	}
	// 再补一批更贵的样本，模拟“任务进行中又积累了历史”。
	for _, c := range []float64{0.9, 0.9, 0.9} {
		seedSettledSpend(t, f.st, "acme/app", "S", c)
	}
	o.recordCostForecast(task)

	events, err := f.st.ListEvents(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	var p50 float64
	for _, e := range events {
		if e.Type == "cost_forecast" {
			count++
			p50, _ = e.Payload["p50_usd"].(float64)
		}
	}
	if count != 1 {
		t.Fatalf("预测事件应恰好 1 条，实际 %d 条", count)
	}
	firstP50, _ := first.Payload["p50_usd"].(float64)
	if p50 != firstP50 {
		t.Errorf("预测被覆盖: 首次 %v，现在 %v", firstP50, p50)
	}
}
