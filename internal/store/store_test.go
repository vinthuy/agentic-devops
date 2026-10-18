package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"agentdev/internal/state"
)

func openTemp(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// M1 已创建的库没有 review_attempts；Open 必须幂等地补列，且旧任务默认 0。
func TestOpenMigratesReviewAttemptsColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY, repo TEXT NOT NULL, title TEXT, requirement TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT 'cli', source_ref TEXT, level TEXT, level_override INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL, previous_status TEXT, plan_json TEXT, branch TEXT, worktree_path TEXT,
		base_sha TEXT, commit_sha TEXT, pr_number INTEGER, pr_url TEXT,
		fix_attempts INTEGER NOT NULL DEFAULT 0, ci_attempts INTEGER NOT NULL DEFAULT 0,
		budget_usd REAL NOT NULL, spent_usd REAL NOT NULL DEFAULT 0, version INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL, finished_at TEXT
	)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("打开旧库并迁移失败: %v", err)
	}
	defer s.Close()
	task, err := s.CreateTask(NewTask{Repo: "acme/app", Requirement: "x", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	if task.ReviewAttempts != 0 {
		t.Errorf("新列应默认为 0，实际 %d", task.ReviewAttempts)
	}
	// 再次打开是安全的，不得重复 ALTER。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重复迁移不幂等: %v", err)
	}
	_ = s2.Close()
}

// 并发执行前一次性取定任务批次，避免两个 worker 领到同一个任务。
func TestListRunnableReturnsDistinctBatch(t *testing.T) {
	s := openTemp(t)
	first := create(t, s, "acme/a")
	create(t, s, "acme/a")
	create(t, s, "acme/a")

	batch, err := s.ListRunnable(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 {
		t.Fatalf("应返回 2 个任务，实际 %d", len(batch))
	}
	if batch[0].ID != first.ID {
		t.Errorf("应按插入顺序返回，首个应为 %s，实际 %s", first.ID, batch[0].ID)
	}
	if batch[0].ID == batch[1].ID {
		t.Error("批次内不应出现重复任务")
	}
	if one, err := s.ListRunnable(0); err != nil || len(one) != 1 {
		t.Errorf("limit<=0 应按 1 处理: %v %d", err, len(one))
	}
}

func TestListAllCostsIncludesTaskIDs(t *testing.T) {
	s := openTemp(t)
	a := create(t, s, "acme/a")
	b := create(t, s, "acme/b")
	if err := s.RecordCost(a.ID, "coder", "model-a", CostEntry{
		InputTokens: 10, OutputTokens: 2, CostUSD: 0.01,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCost(b.ID, "reviewer", "model-b", CostEntry{
		InputTokens: 20, OutputTokens: 3, CostUSD: 0.02,
	}); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListAllCosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].TaskID != a.ID || all[1].TaskID != b.ID {
		t.Fatalf("成本看板应包含 task ID 且按写入顺序返回: %+v", all)
	}
	one, err := s.ListCosts(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].TaskID != a.ID {
		t.Errorf("任务成本明细应返回 task ID: %+v", one)
	}
}

func create(t *testing.T, s *SQLiteStore, repo string) Task {
	t.Helper()
	task, err := s.CreateTask(NewTask{Repo: repo, Requirement: "x", BudgetUSD: 2, Level: "S"})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestCreateTaskWritesQueuedAndEvent(t *testing.T) {
	s := openTemp(t)
	task := create(t, s, "acme/app")
	if task.Status != state.Queued || task.Version != 0 {
		t.Fatalf("初始状态异常: %+v", task)
	}
	events, err := s.ListEvents(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "task_created" {
		t.Errorf("事件异常: %+v", events)
	}
}

func TestTransitionUpdatesStatusVersionAndFields(t *testing.T) {
	s := openTemp(t)
	task := create(t, s, "acme/app")
	task, err := s.Transition(task.ID, task.Version, state.Triaging, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err = s.Transition(task.ID, task.Version, state.Planning, nil, map[string]any{"branch": "agent/t-1"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != state.Planning || task.Version != 2 || task.Branch != "agent/t-1" {
		t.Errorf("转换结果异常: %+v", task)
	}
	events, _ := s.ListEvents(task.ID)
	last := events[len(events)-1]
	if last.FromStatus != "TRIAGING" || last.ToStatus != "PLANNING" {
		t.Errorf("状态变化事件异常: %+v", last)
	}
}

func TestInvalidTransitionRejectedAndStateUnchanged(t *testing.T) {
	s := openTemp(t)
	task := create(t, s, "acme/app")
	if _, err := s.Transition(task.ID, task.Version, state.Done, nil, nil); err == nil {
		t.Fatal("非法转换应当失败")
	}
	got, _ := s.GetTask(task.ID)
	if got.Status != state.Queued {
		t.Errorf("失败的转换不应改变状态: %s", got.Status)
	}
}

func TestStaleVersionIsConcurrentModification(t *testing.T) {
	s := openTemp(t)
	task := create(t, s, "acme/app")
	if _, err := s.Transition(task.ID, task.Version, state.Triaging, nil, nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.Transition(task.ID, task.Version, state.Planning, nil, nil)
	if !errors.Is(err, ErrConcurrentModification) {
		t.Errorf("应当返回版本冲突，实际 %v", err)
	}
}

func TestSuspendRecordsPreviousStatus(t *testing.T) {
	s := openTemp(t)
	task := create(t, s, "acme/app")
	steps := []state.Status{state.Triaging, state.Planning, state.Coding, state.NeedsHuman}
	for _, to := range steps {
		var err error
		task, err = s.Transition(task.ID, task.Version, to, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if task.PreviousStatus != state.Coding {
		t.Errorf("previous_status 应为 CODING，实际 %s", task.PreviousStatus)
	}
}

func TestListAndNextQueuedFollowInsertionOrder(t *testing.T) {
	s := openTemp(t)
	first := create(t, s, "acme/app")
	create(t, s, "other/x")

	next, ok, err := s.NextQueued()
	if err != nil || !ok {
		t.Fatalf("应当有排队任务: %v %v", ok, err)
	}
	if next.ID != first.ID {
		t.Errorf("应按插入顺序取出，期望 %s，实际 %s", first.ID, next.ID)
	}
	others, _ := s.ListTasks("", "other/x")
	if len(others) != 1 {
		t.Errorf("按仓库过滤异常: %d", len(others))
	}
	done, _ := s.ListTasks(state.Done, "")
	if len(done) != 0 {
		t.Errorf("按状态过滤异常: %d", len(done))
	}
}

func TestGetMissingTask(t *testing.T) {
	s := openTemp(t)
	if _, err := s.GetTask("t_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("应当返回 ErrNotFound，实际 %v", err)
	}
}

func TestWALModeEnabled(t *testing.T) {
	s := openTemp(t)
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %s", mode)
	}
}

func TestVerificationAndViolationRecords(t *testing.T) {
	s := openTemp(t)
	task := create(t, s, "acme/app")
	if err := s.RecordVerification(task.ID, 0, true, map[string]any{"passed": true}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordViolation(task.ID, "command", "rm -rf", true); err != nil {
		t.Fatal(err)
	}
	vs, _ := s.ListVerifications(task.ID)
	if len(vs) != 1 || !vs[0].Passed {
		t.Errorf("验证记录异常: %+v", vs)
	}
	vio, _ := s.ListViolations(task.ID)
	if len(vio) != 1 || vio[0].Kind != "command" || !vio[0].Blocked {
		t.Errorf("违规记录异常: %+v", vio)
	}
}
