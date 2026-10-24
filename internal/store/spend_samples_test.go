package store

import (
	"testing"

	"agentdev/internal/state"
)

// walkTo 把任务推进到目标状态，返回最新版本。
func walkTo(t *testing.T, s *SQLiteStore, id string, version int64, path []state.Status) (Task, int64) {
	t.Helper()
	var task Task
	for _, next := range path {
		var err error
		task, err = s.Transition(id, version, next, nil, nil)
		if err != nil {
			t.Fatalf("转换到 %s 失败: %v", next, err)
		}
		version = task.Version
	}
	return task, version
}

// 成本预测只应使用“已结算且真的花过钱”的任务：
// 中间态的花费是部分金额，0 花费通常是还没调模型就结束了。
func TestSpendSamplesOnlySettledTasksWithSpend(t *testing.T) {
	s := openTemp(t)

	// 1) 已结算且有花费 → 计入
	done, err := s.CreateTask(NewTask{Repo: "r", Requirement: "x", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	done, _ = walkTo(t, s, done.ID, done.Version, []state.Status{
		state.Triaging, state.Planning, state.Coding, state.Verifying,
		state.Reviewing, state.Committing, state.Done,
	})
	if err := s.AddSpend(done.ID, 0.02); err != nil {
		t.Fatal(err)
	}

	// 2) 卡在 CODING（还在跑）→ 部分花费不应计入
	running, err := s.CreateTask(NewTask{Repo: "r", Requirement: "x", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	walkTo(t, s, running.ID, running.Version, []state.Status{state.Triaging, state.Planning, state.Coding})
	if err := s.AddSpend(running.ID, 0.05); err != nil {
		t.Fatal(err)
	}

	// 3) 已结算但 0 花费 → 不计入
	zero, err := s.CreateTask(NewTask{Repo: "r", Requirement: "x", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	walkTo(t, s, zero.ID, zero.Version, []state.Status{state.Triaging, state.Rejected})

	samples, err := s.SpendSamples()
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("应只返回 1 条样本，实际 %d 条: %+v", len(samples), samples)
	}
	if samples[0].TaskID != done.ID {
		t.Errorf("样本任务错误: %s", samples[0].TaskID)
	}
	if samples[0].CostUSD != 0.02 {
		t.Errorf("样本花费错误: %v", samples[0].CostUSD)
	}
	if samples[0].Level != "" {
		t.Errorf("未分级任务的 level 应为空串，实际 %q", samples[0].Level)
	}
}

// NEEDS_HUMAN 的任务确实花过钱且不会自己继续，应计入样本。
func TestSpendSamplesIncludesNeedsHuman(t *testing.T) {
	s := openTemp(t)
	task, err := s.CreateTask(NewTask{Repo: "r", Requirement: "x", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	task, _ = walkTo(t, s, task.ID, task.Version, []state.Status{state.Triaging, state.Planning, state.Coding})
	if _, err := s.Transition(task.ID, task.Version, state.NeedsHuman,
		map[string]any{"reason": "test"}, map[string]any{"level": "S"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSpend(task.ID, 0.3); err != nil {
		t.Fatal(err)
	}
	samples, err := s.SpendSamples()
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Level != "S" {
		t.Fatalf("NEEDS_HUMAN 任务应计入样本且带级别: %+v", samples)
	}
}

func TestSpendSamplesEmptyStore(t *testing.T) {
	samples, err := openTemp(t).SpendSamples()
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 0 {
		t.Errorf("空库应返回 0 条，实际 %d", len(samples))
	}
}
