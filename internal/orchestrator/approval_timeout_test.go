package orchestrator

import (
	"strings"
	"testing"
	"time"

	"agentdev/internal/backend"
	"agentdev/internal/state"
)

// parkTask 把任务推到审批门。审批前不会调用后端，所以后端给空脚本即可。
func parkTask(t *testing.T, f *fixture) string {
	t.Helper()
	task := f.task("M")
	done := f.run(backend.NewFake(nil), task.ID)
	if done.Status != state.AwaitingApproval {
		t.Fatalf("应停在审批门，实际 %s", done.Status)
	}
	return task.ID
}

func TestExpireApprovalsUpgradesStaleGate(t *testing.T) {
	f := setup(t)
	f.cfg.Policy.ApprovalTimeoutMinutes = 60
	id := parkTask(t, f)

	// 刚停下不算超时。
	n, err := expireApprovals(f.st, f.cfg, time.Now())
	if err != nil || n != 0 {
		t.Fatalf("刚停下的任务不应被升级 (n=%d): %v", n, err)
	}

	// 两小时后再看：升级为 NEEDS_HUMAN，并且记住回得去的状态。
	n, err = expireApprovals(f.st, f.cfg, time.Now().Add(2*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("超时任务应被升级 (n=%d): %v", n, err)
	}
	got, err := f.st.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != state.NeedsHuman {
		t.Fatalf("应升级到 NEEDS_HUMAN，实际 %s", got.Status)
	}
	// resume 必须能把任务送回审批门——否则升级等于把任务扔进死胡同。
	if got.PreviousStatus != state.AwaitingApproval {
		t.Errorf("应记录挂起前状态为 AWAITING_APPROVAL，实际 %s", got.PreviousStatus)
	}
	if !state.CanTransition(state.NeedsHuman, got.PreviousStatus) {
		t.Error("resume 应允许回到审批门")
	}
	events, err := f.st.ListEvents(id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Payload["cause"] == "approval_timeout" {
			found = true
			if reason, _ := e.Payload["reason"].(string); !strings.Contains(reason, "审批超时") {
				t.Errorf("升级理由应说明超时: %v", e.Payload["reason"])
			}
		}
	}
	if !found {
		t.Error("应记录 approval_timeout 事件供审计")
	}
}

// 0 表示不超时：默认配置下停在审批门的任务必须一直等下去，
// 否则「人工审批」会变成「等一会儿自动升级」。
func TestExpireApprovalsDisabledByDefault(t *testing.T) {
	f := setup(t)
	id := parkTask(t, f)
	if f.cfg.Policy.ApprovalTimeoutMinutes != 0 {
		t.Fatalf("默认应为 0，实际 %d", f.cfg.Policy.ApprovalTimeoutMinutes)
	}
	n, err := expireApprovals(f.st, f.cfg, time.Now().Add(30*24*time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("未配置超时不应升级 (n=%d): %v", n, err)
	}
	got, _ := f.st.GetTask(id)
	if got.Status != state.AwaitingApproval {
		t.Errorf("状态应保持不变，实际 %s", got.Status)
	}
}

// 只碰审批门上的任务：已批准进入编码的任务不会因为这条策略被改动。
func TestExpireApprovalsOnlyTouchesApprovalGate(t *testing.T) {
	f := setup(t)
	f.cfg.Policy.ApprovalTimeoutMinutes = 1
	id := parkTask(t, f)

	cur, err := f.st.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Transition(id, cur.Version, state.Coding,
		map[string]any{"approved_by": "test"}, nil); err != nil {
		t.Fatal(err)
	}
	n, err := expireApprovals(f.st, f.cfg, time.Now().Add(10*time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("非审批门任务不应被升级 (n=%d): %v", n, err)
	}
	got, _ := f.st.GetTask(id)
	if got.Status != state.Coding {
		t.Errorf("CODING 任务不应被改动，实际 %s", got.Status)
	}
}

// 升级后必须能真的 resume 回审批门并继续等人，而不是卡在 NEEDS_HUMAN。
func TestExpiredApprovalCanResumeBackToGate(t *testing.T) {
	f := setup(t)
	f.cfg.Policy.ApprovalTimeoutMinutes = 60
	id := parkTask(t, f)
	if _, err := expireApprovals(f.st, f.cfg, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	cur, err := f.st.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Transition(id, cur.Version, cur.PreviousStatus,
		map[string]any{"reason": "人工确认后再决定"}, nil); err != nil {
		t.Fatalf("resume 回应成功: %v", err)
	}
	back, _ := f.st.GetTask(id)
	if back.Status != state.AwaitingApproval {
		t.Fatalf("应回到审批门，实际 %s", back.Status)
	}
	// 回到门上的任务重新计时：立刻检查不应再次升级。
	if n, err := expireApprovals(f.st, f.cfg, time.Now()); err != nil || n != 0 {
		t.Errorf("刚 resume 的任务不应立刻再次超时 (n=%d): %v", n, err)
	}
	// 但重新计时意味着它仍会在新的截止时间后再次升级，而不是被豁免一次。
	if n, err := expireApprovals(f.st, f.cfg, time.Now().Add(3*time.Hour)); err != nil || n != 1 {
		t.Errorf("重新计时后仍应再次升级 (n=%d): %v", n, err)
	}
}
