package orchestrator

import (
	"testing"
	"time"

	"agentdev/internal/state"
)

func TestExpirePendingReusesApprovalTimeout(t *testing.T) {
	f := setup(t)
	f.cfg.Policy.ApprovalTimeoutMinutes = 60
	id := parkTask(t, f)

	got, err := expirePending(f.st, f.cfg, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiredApprovals != 1 || got.ExpiredLocks != 0 {
		t.Fatalf("应只升级到期审批: %+v", got)
	}
	task, err := f.st.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != state.NeedsHuman {
		t.Fatalf("应升级为 NEEDS_HUMAN，实际 %s", task.Status)
	}
	events, err := f.st.ListEvents(id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Payload["cause"] == "approval_timeout" {
			found = true
		}
	}
	if !found {
		t.Fatal("应使用现有审批超时原因")
	}
}

func TestExpirePendingReleasesStaleLock(t *testing.T) {
	f := setup(t)
	f.cfg.Policy.TaskTimeoutMinutes = 30
	if err := f.st.PutLock("acme/app", "t1", time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutLock("other/app", "t2", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	got, err := expirePending(f.st, f.cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiredLocks != 1 {
		t.Fatalf("应释放 1 把过期锁，实际 %+v", got)
	}
	locks, err := f.st.ListLocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 || locks[0].Resource != "other/app" {
		t.Fatalf("未过期的锁应保留: %+v", locks)
	}
}
