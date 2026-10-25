package cli

import (
	"testing"
	"time"

	"agentdev/internal/state"
)

// 挂起状态不会自己往前走，所以列表必须让「等了多久」可见：
// 否则「刚停下」和「停了三天」在列表里长得一模一样。
func TestWaitLabelOnlyForSuspendedStates(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	updated := now.Add(-3 * time.Hour).Format(time.RFC3339)

	cases := []struct {
		status state.Status
		want   string
	}{
		{state.AwaitingApproval, "3h"},
		{state.NeedsInfo, "3h"},
		{state.NeedsHuman, "3h"},
		{state.Coding, "-"},
		{state.Done, "-"},
		{state.Queued, "-"},
	}
	for _, c := range cases {
		if got := waitLabel(c.status, updated, now); got != c.want {
			t.Errorf("%s: 期望 %q，实际 %q", c.status, c.want, got)
		}
	}
}

func TestWaitLabelFormats(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{0, "0m"},
		{12 * time.Minute, "12m"},
		{90 * time.Minute, "1h"},
		{47 * time.Hour, "47h"},
		{50 * time.Hour, "2d"},
		{73 * time.Hour, "3d"},
	}
	for _, c := range cases {
		got := waitLabel(state.AwaitingApproval, now.Add(-c.ago).Format(time.RFC3339), now)
		if got != c.want {
			t.Errorf("等待 %s: 期望 %q，实际 %q", c.ago, c.want, got)
		}
	}
}

// 时间戳坏掉时不能把任务误报成「等了很久」，退化成 "-" 交给时间线去查。
func TestWaitLabelHandlesBadTimestamp(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if got := waitLabel(state.AwaitingApproval, "not-a-time", now); got != "-" {
		t.Errorf("非法时间戳应返回 \"-\"，实际 %q", got)
	}
	// 时钟漂移导致的未来时间不应出现负数等待。
	future := now.Add(2 * time.Hour).Format(time.RFC3339)
	if got := waitLabel(state.AwaitingApproval, future, now); got != "0m" {
		t.Errorf("未来时间应钳到 0m，实际 %q", got)
	}
}
