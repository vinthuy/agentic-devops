package orchestrator

import (
	"strings"
	"testing"

	"agentdev/internal/state"
	"agentdev/internal/store"
)

// TestCIFailureEvidence 覆盖「CI 失败证据只在最后一次推送之后才注入」这条边界：
// 它必须覆盖整个回修循环（含审查打回后的再次修改），又不能为后续推送背书。
func TestCIFailureEvidence(t *testing.T) {
	push := func() store.Event {
		return store.Event{Type: "state_changed", FromStatus: string(state.Committing), ToStatus: string(state.Pushed)}
	}
	ciFix := func(failure string) store.Event {
		return store.Event{Type: "fix_requested", Payload: map[string]any{"source": "ci", "failure": failure}}
	}

	// 还没有任何推送：没有证据可注入。
	if got := ciFailureEvidence(nil); got != "" {
		t.Fatalf("未推送时不应注入证据，实际 %q", got)
	}

	events := []store.Event{
		{Type: "state_changed", FromStatus: string(state.Committing), ToStatus: string(state.Pushed)},
		{Type: "ci_failed", Payload: map[string]any{"summary": "1 项检查失败（test）"}},
		ciFix("CI 失败：1 项检查失败（test）\n\nAssertionError: ValueError not raised"),
	}

	got := ciFailureEvidence(events)
	if !strings.Contains(got, "ValueError not raised") {
		t.Fatalf("应注入 CI 失败输出，实际 %q", got)
	}

	// 审查再打回一轮：触发源变成 reviewer，但这次 CI 失败仍是当前 diff 要解决的
	// 问题，证据不能丢，否则审查者会把 CI 要求的修复当成超范围又要求回退。
	events = append(events, store.Event{Type: "fix_requested", Payload: map[string]any{
		"source": "reviewer", "failure": "Reviewer 判定不通过：超出需求范围",
	}})
	if got := ciFailureEvidence(events); !strings.Contains(got, "ValueError not raised") {
		t.Fatalf("审查打回后仍应保留 CI 证据，实际 %q", got)
	}

	// 重新推送后，旧失败不再代表当前 diff，证据清空。
	events = append(events, push())
	if got := ciFailureEvidence(events); got != "" {
		t.Fatalf("再次推送后应清空证据，实际 %q", got)
	}
}

// TestNoChangeReasonMentionsPendingCI 钉住「没有可提交的改动」的可诊断性。
//
// 实测故障：编码步骤因为日志被截断而判定「代码已正确、无需修改」，任务以一句
// 「没有可提交的改动」转人工——看报告的人无从知道还有一次 CI 失败悬着。
func TestNoChangeReasonMentionsPendingCI(t *testing.T) {
	if got := noChangeReasonText(""); got != "没有可提交的改动" {
		t.Fatalf("无待修 CI 时应保持原文，实际 %q", got)
	}
	got := noChangeReasonText("CI 失败：1 项检查失败（test）")
	if !strings.Contains(got, "CI 仍为失败") {
		t.Fatalf("有待修 CI 时必须写明，实际 %q", got)
	}
}
