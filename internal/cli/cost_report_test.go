package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/store"
)

func TestCostReportFiltersByRepoAndDay(t *testing.T) {
	home := writeHome(t, "not-used")
	s, err := store.Open(filepath.Join(home, "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.CreateTask(store.NewTask{Repo: "acme/app", Requirement: "a", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateTask(store.NewTask{Repo: "other/app", Requirement: "b", BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCost(app.ID, "coder", "model-a", store.CostEntry{TS: "2026-10-30T01:00:00Z", CostUSD: 0.01}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCost(app.ID, "coder", "model-a", store.CostEntry{TS: "2026-10-31T02:00:00Z", CostUSD: 0.04}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCost(other.ID, "reviewer", "model-b", store.CostEntry{TS: "2026-10-31T03:00:00Z", CostUSD: 0.50}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := invoke(t, "cost", "report", "--home", home, "--repo", "acme/app", "--day", "2026-10-31")
	if code != ExitOK {
		t.Fatalf("筛选失败 (%d): %s", code, errOut)
	}
	for _, want := range []string{"1 个任务", "1 次模型调用", "$0.040000"} {
		if !strings.Contains(out, want) {
			t.Errorf("筛选结果缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "$0.500000") || strings.Contains(out, "$0.010000") || strings.Contains(out, "other/app") {
		t.Errorf("筛进了其他仓库或其他日期:\n%s", out)
	}

	code, _, errOut = invoke(t, "cost", "report", "--home", home, "--day", "31-10-2026")
	if code != ExitUsage {
		t.Fatalf("非法日期应拒绝 (%d): %s", code, errOut)
	}
}
