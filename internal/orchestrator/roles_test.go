package orchestrator

import (
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/cost"
	"agentdev/internal/llm"
)

func TestUnsetRoleUsesDefaultModel(t *testing.T) {
	f := setup(t)
	orch := New(f.st, f.cfg, backend.NewFake(nil),
		WithModel(nil, "default-model", 128, nil),
		WithRoleModels(map[string]string{"coder": "coder-x"}))
	if orch.modelFor("coder") != "coder-x" {
		t.Fatalf("编码模型应为 coder-x，实际 %s", orch.modelFor("coder"))
	}
	for _, role := range []string{"triage", "planner", "reviewer"} {
		if got := orch.modelFor(role); got != "default-model" {
			t.Fatalf("%s 应使用默认模型，实际 %s", role, got)
		}
	}
}

func TestUnpricedRoleModelIsNamedInTheLedger(t *testing.T) {
	f := setup(t)
	task := f.task("S")
	srv := usageServer(t, 1000)
	defer srv.Close()
	client := llm.NewClient(srv.URL, "k")
	orch := New(f.st, f.cfg, backend.NewAnthropic(client, "coder-x", 1024),
		WithModel(client, "default-model", 1024, cost.Table{
			"default-model": {Input: 1, Output: 1},
		}),
		WithRoleModels(map[string]string{"coder": "coder-x"}))
	if _, err := orch.RunTask(task.ID); err != nil {
		t.Fatal(err)
	}
	entries, err := f.st.ListCosts(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || entries[0].Model != "coder-x" {
		t.Fatalf("费用应记在实际调用的模型上: %+v", entries)
	}
	events := eventsOfType(t, f.st, task.ID, "cost_pricing_missing")
	if len(events) != 1 || events[0].Payload["model"] != "coder-x" {
		t.Fatalf("缺单价告警应点名 coder-x: %+v", events)
	}
}
