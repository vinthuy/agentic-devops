package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenMigratesParentTaskID(t *testing.T) {
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
		review_attempts INTEGER NOT NULL DEFAULT 0,
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
		t.Fatalf("打开旧库并补 parent_task_id 失败: %v", err)
	}
	parent, err := s.CreateTask(NewTask{Repo: "acme/app", Requirement: "父任务", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	if parent.ParentTaskID != "" {
		t.Fatalf("旧行和新的独立任务都不应有父任务，实际 %q", parent.ParentTaskID)
	}
	child, err := s.CreateTask(NewTask{
		Repo: "acme/app", Requirement: "第一段", BudgetUSD: 1, ParentTaskID: parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("重复打开旧库失败: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.GetTask(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentTaskID != parent.ID {
		t.Fatalf("父任务编号应保留，实际 %q", got.ParentTaskID)
	}
	again, err := reopened.CreateTask(NewTask{Repo: "acme/app", Requirement: "仍可写入", BudgetUSD: 1})
	if err != nil {
		t.Fatalf("补列后应仍能写入: %v", err)
	}
	if again.ParentTaskID != "" {
		t.Fatalf("未指定父任务时应为空，实际 %q", again.ParentTaskID)
	}
}
