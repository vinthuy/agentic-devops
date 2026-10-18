package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/state"
	"agentdev/internal/store"
	"agentdev/internal/testutil"
	"agentdev/internal/workspace"
)

// driveToCommitting 把任务推进到 COMMITTING，并在工作区写好 hello.txt。
// 用于模拟“副作用已完成、状态尚未落库”的崩溃点。
func driveToCommitting(t *testing.T, f *fixture, taskID string, withChange bool) (store.Task, workspace.Info) {
	t.Helper()
	task, err := f.st.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	repo := f.cfg.Repos["acme/app"]
	info, err := workspace.New(f.cfg).Create("acme/app", repo, task.ID, "recovery")
	if err != nil {
		t.Fatal(err)
	}
	if withChange {
		if err := os.WriteFile(filepath.Join(info.Path, "hello.txt"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, next := range []state.Status{
		state.Triaging, state.Planning, state.Coding, state.Verifying, state.Committing,
	} {
		fields := map[string]any{}
		if next == state.Coding {
			fields = map[string]any{
				"branch":        info.Branch,
				"worktree_path": filepath.ToSlash(info.Path),
				"base_sha":      info.BaseSHA,
			}
		}
		if task, err = f.st.Transition(task.ID, task.Version, next, nil, fields); err != nil {
			t.Fatal(err)
		}
	}
	return task, info
}

// G-10：commit 成功但状态未落库时崩溃，恢复必须复用已有提交，而不是判成“没有改动”。
func TestCommitStepRecoversCommitFromCrashedRun(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	task, info := driveToCommitting(t, f, task.ID, true)

	// 模拟崩溃前已经完成的提交。
	committed, _, err := workspace.New(f.cfg).Commit(info, commitMessage(task))
	if err != nil {
		t.Fatal(err)
	}
	if committed == "" {
		t.Fatal("测试前置条件失败：应先产生一次提交")
	}

	done, err := New(f.st, f.cfg, backend.NewFake(nil)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("恢复后应完成，实际 %s", done.Status)
	}
	if done.CommitSHA != committed {
		t.Errorf("应复用崩溃前的提交 %s，实际 %s", committed, done.CommitSHA)
	}
	if len(eventsOfType(t, f.st, task.ID, "commit_recovered")) != 1 {
		t.Error("应记录 commit_recovered 事件，让审计能看出复用了旧提交")
	}
	// 不能出现第二个“任务提交”。
	log := testutil.Git(t, info.Path, "log", "--format=%s")
	if got := strings.Count(log, "agent("+task.ID+")"); got != 1 {
		t.Errorf("任务提交应恰好 1 个，实际 %d:\n%s", got, log)
	}
}

// 没有改动、也没有属于本任务的提交时，仍然应转人工，不能被恢复逻辑掩盖。
func TestCommitStepWithoutChangesStillSuspends(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	task, _ = driveToCommitting(t, f, task.ID, false)

	done, err := New(f.st, f.cfg, backend.NewFake(nil)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("没有改动时应转人工，实际 %s", done.Status)
	}
	if reason := lastReason(t, f.st, task.ID); !strings.Contains(reason, "没有可提交的改动") {
		t.Errorf("原因应为没有可提交的改动: %q", reason)
	}
}
