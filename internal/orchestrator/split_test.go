package orchestrator

import (
	"context"
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/planning"
	"agentdev/internal/state"
	"agentdev/internal/store"
)

type stubSplitter struct {
	plan planning.SplitPlan
	err  error
}

func (s stubSplitter) Split(context.Context, planning.Requirement) (planning.SplitPlan, error) {
	return s.plan, s.err
}

func twoSlices() planning.SplitPlan {
	file := func(id, path string) []planning.Acceptance {
		return []planning.Acceptance{{
			ID: id, Description: path + " 存在",
			Check: planning.Check{Type: planning.CheckFileExists, Path: path},
		}}
	}
	return planning.SplitPlan{Slices: []planning.Slice{
		{Summary: "第一段：写 hello.txt", AllowedPaths: []string{"hello.txt"}, Acceptance: file("a1", "hello.txt")},
		{Summary: "第二段：写 bye.txt", AllowedPaths: []string{"bye.txt"}, Acceptance: file("b1", "bye.txt")},
	}}
}

func childrenOf(t *testing.T, st *store.SQLiteStore, parentID string) []store.Task {
	t.Helper()
	all, err := st.ListTasks("", "acme/app")
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Task
	for _, task := range all {
		if task.ParentTaskID == parentID {
			out = append(out, task)
		}
	}
	return out
}

func TestLevelLCreatesOnlyTheFirstChild(t *testing.T) {
	f := setup(t)
	task := f.task("L")
	be := backend.NewFake([][]backend.ToolCall{good})
	parent, err := New(f.st, f.cfg, be, WithSplitter(stubSplitter{plan: twoSlices()})).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Status != state.NeedsHuman {
		t.Fatalf("父任务应停在待人工，实际 %s（%s）", parent.Status, lastReason(t, f.st, parent.ID))
	}
	if parent.CommitSHA != "" || parent.WorktreePath != "" {
		t.Fatalf("父任务不应有提交或工作区: sha=%q worktree=%q", parent.CommitSHA, parent.WorktreePath)
	}
	if lastReason(t, f.st, parent.ID) != "l_split" {
		t.Fatalf("原因应为 l_split，实际 %s", lastReason(t, f.st, parent.ID))
	}
	kids := childrenOf(t, f.st, parent.ID)
	if len(kids) != 1 {
		t.Fatalf("应只有第一段子任务，实际 %d", len(kids))
	}
	if kids[0].Level != "M" || kids[0].Status != state.AwaitingApproval || kids[0].ParentTaskID != parent.ID {
		t.Fatalf("第一段应为等待审批的 M 级子任务: %+v", kids[0])
	}
	if len(be.Calls) != 0 {
		t.Fatalf("父任务不应进入编码: %v", be.Calls)
	}
	events := eventsOfType(t, f.st, parent.ID, "l_split")
	if len(events) != 1 || events[0].Payload["child_task_id"] != kids[0].ID {
		t.Fatalf("应记录子任务 id: %+v", events)
	}
}

func TestApprovingTheFirstSliceDoesNotStartTheNext(t *testing.T) {
	f := setup(t)
	task := f.task("L")
	parent, err := New(f.st, f.cfg, backend.NewFake(nil), WithSplitter(stubSplitter{plan: twoSlices()})).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	kids := childrenOf(t, f.st, parent.ID)
	if len(kids) != 1 {
		t.Fatalf("批准前应只有一段，实际 %d", len(kids))
	}
	approved, err := f.st.Transition(kids[0].ID, kids[0].Version, state.Coding, map[string]any{"approved_by": "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != state.Coding {
		t.Fatalf("第一段应离开审批门，实际 %s", approved.Status)
	}
	again := childrenOf(t, f.st, parent.ID)
	if len(again) != 1 || again[0].Status == state.AwaitingApproval && again[0].ID != kids[0].ID {
		t.Fatalf("批准第一段不应创建或批准第二段: %+v", again)
	}
	if len(again) != 1 || again[0].ID != kids[0].ID || again[0].Status != state.Coding {
		t.Fatalf("第二段不应出现: %+v", again)
	}
}

func TestRejectingTheFirstSliceCreatesNoNext(t *testing.T) {
	f := setup(t)
	task := f.task("L")
	parent, err := New(f.st, f.cfg, backend.NewFake(nil), WithSplitter(stubSplitter{plan: twoSlices()})).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	kids := childrenOf(t, f.st, parent.ID)
	if _, err := f.st.Transition(kids[0].ID, kids[0].Version, state.Rejected, map[string]any{"reason": "这段不做"}, nil); err != nil {
		t.Fatal(err)
	}
	again := childrenOf(t, f.st, parent.ID)
	if len(again) != 1 || again[0].Status != state.Rejected {
		t.Fatalf("驳回后不应再创建后续段: %+v", again)
	}
}

func TestNextSliceWaitsUntilTheFirstIsDone(t *testing.T) {
	f := setup(t)
	task := f.task("L")
	be := backend.NewFake([][]backend.ToolCall{good})
	orch := New(f.st, f.cfg, be, WithSplitter(stubSplitter{plan: twoSlices()}))
	parent, err := orch.RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	kids := childrenOf(t, f.st, parent.ID)
	if len(kids) != 1 {
		t.Fatalf("开始前应只有一段，实际 %d", len(kids))
	}
	if _, err := f.st.Transition(kids[0].ID, kids[0].Version, state.Coding, map[string]any{"approved_by": "test"}, nil); err != nil {
		t.Fatal(err)
	}
	done, err := orch.RunTask(kids[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("第一段应完成，实际 %s（%s）", done.Status, lastReason(t, f.st, kids[0].ID))
	}
	again := childrenOf(t, f.st, parent.ID)
	if len(again) != 2 {
		t.Fatalf("第一段完成后才应创建第二段，实际 %d", len(again))
	}
	var second store.Task
	for _, kid := range again {
		if kid.ID != kids[0].ID {
			second = kid
		}
	}
	if second.Status != state.AwaitingApproval || second.Level != "M" {
		t.Fatalf("第二段应自己停在审批门: %+v", second)
	}
	parent, err = f.st.GetTask(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parent.CommitSHA != "" {
		t.Fatalf("父任务不应有提交: %s", parent.CommitSHA)
	}
}

func TestInvalidSplitStopsWithoutAChild(t *testing.T) {
	f := setup(t)
	task := f.task("L")
	be := backend.NewFake([][]backend.ToolCall{good})
	parent, err := New(f.st, f.cfg, be, WithSplitter(stubSplitter{plan: planning.SplitPlan{}})).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Status != state.NeedsHuman || lastReason(t, f.st, parent.ID) != "拆分失败" {
		t.Fatalf("无法拆开时应待人工，实际 %s（%s）", parent.Status, lastReason(t, f.st, parent.ID))
	}
	if len(childrenOf(t, f.st, parent.ID)) != 0 {
		t.Fatal("拆分失败时不应创建子任务")
	}
	if len(be.Calls) != 0 {
		t.Fatal("拆分失败时不应进入编码")
	}
}
