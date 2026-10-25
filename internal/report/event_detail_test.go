package report

import (
	"strings"
	"testing"

	"agentdev/internal/store"
)

// 审批门的两侧（放行 / 驳回）都必须把人是谁显示在时间线上。
// approved_by 此前只写进 payload，`task status` 看不到——审计时等于没有。
func TestEventDetailShowsApprovalActors(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    []string
	}{
		{
			name:    "放行",
			payload: map[string]any{"reason": "计划已核对", "approved_by": "leo"},
			want:    []string{"计划已核对", "审批人 leo"},
		},
		{
			name:    "驳回",
			payload: map[string]any{"reason": "会动到公开 API", "rejected_by": "leo"},
			want:    []string{"会动到公开 API", "驳回人 leo"},
		},
		{
			name:    "只有理由",
			payload: map[string]any{"reason": "模型限流"},
			want:    []string{"模型限流"},
		},
		{
			name:    "没有理由也要显示审批人",
			payload: map[string]any{"approved_by": "leo"},
			want:    []string{"审批人 leo"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EventDetail(store.Event{Type: "state_changed", Payload: c.payload})
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("应包含 %q，实际 %q", w, got)
				}
			}
		})
	}
}
