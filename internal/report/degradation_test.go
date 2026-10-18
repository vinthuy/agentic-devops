package report

import (
	"strings"
	"testing"

	"agentdev/internal/store"
)

func TestDegradationsIncludesFailuresAndDetails(t *testing.T) {
	events := []store.Event{
		{Type: "state_changed"},
		{Type: "triage_model_failed", Payload: map[string]any{"error": "HTTP 400: tool_choice unsupported"}},
		{Type: "plan_failed", Payload: map[string]any{"error": "timeout"}},
		{Type: "cost_pricing_missing", Payload: map[string]any{"model": "unknown", "note": "budget is not enforced"}},
	}
	degs := Degradations(events)
	if len(degs) != 3 {
		t.Fatalf("want 3 degradations, got %d", len(degs))
	}
	if !strings.Contains(degs[0].SummaryLine(), "tool_choice unsupported") {
		t.Errorf("triage failure detail missing: %q", degs[0].SummaryLine())
	}
	if !strings.Contains(degs[1].SummaryLine(), "timeout") {
		t.Errorf("plan failure detail missing: %q", degs[1].SummaryLine())
	}
	if !strings.Contains(degs[2].SummaryLine(), "unknown") || !strings.Contains(degs[2].SummaryLine(), "budget is not enforced") {
		t.Errorf("pricing warning detail missing: %q", degs[2].SummaryLine())
	}
}

func TestReportRendersSourceAndDegradationProminently(t *testing.T) {
	task := store.Task{ID: "t1", Level: "S", Status: "DONE", Requirement: "fix bug", BudgetUSD: 2}
	events := []store.Event{
		{Type: "triage_model_failed", Payload: map[string]any{"error": "HTTP 400"}},
		{Type: "triage_ready", Payload: map[string]any{"source": "rules", "level": "S"}},
		{Type: "plan_failed", Payload: map[string]any{"error": "tool choice rejected"}},
		{Type: "plan_ready", Payload: map[string]any{"source": "static_fallback"}},
	}
	content := render(task, events, nil, nil, nil, "now")
	for _, expected := range []string{
		"分级来源: 规则",
		"计划来源: 静态降级",
		"## 降级与异常",
		"HTTP 400",
		"tool choice rejected",
	} {
		if !strings.Contains(content, expected) {
			t.Errorf("report missing %q:\n%s", expected, content)
		}
	}
}
