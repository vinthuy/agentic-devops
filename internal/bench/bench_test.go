package bench

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/testutil"
)

func writeFixture(t *testing.T, dir string) string {
	t.Helper()
	fixture := filepath.Join(dir, "fixture")
	if err := os.MkdirAll(fixture, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "check.go"), []byte(testutil.CheckSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func fakeWriting(content string) func(Case) (backend.ExecutionBackend, error) {
	return func(Case) (backend.ExecutionBackend, error) {
		return backend.NewFake([][]backend.ToolCall{{{
			Tool: "write_file",
			Args: map[string]any{"path": "hello.txt", "content": content},
		}}}), nil
	}
}

func TestRunManifestPassesAndReportsMetrics(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)
	manifest := Manifest{Cases: []Case{{
		ID:          "pass",
		Fixture:     "fixture",
		Requirement: "在仓库根目录创建 hello.txt，内容为 hello",
		Verify:      map[string]string{"check": "go run check.go"},
		ExpectLevel: "S",
		BudgetUSD:   1,
	}}}

	summary, outcomes, err := RunManifest(context.Background(), dir, manifest, Options{
		WorkDir:    t.TempDir(),
		BackendFor: fakeWriting("hello\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("应产生 1 条结果，实际 %d", len(outcomes))
	}
	o := outcomes[0]
	if !o.Passed || o.Status != "DONE" {
		t.Fatalf("用例应通过: %+v", o)
	}
	if o.Level != "S" || !o.LevelMatched {
		t.Errorf("分级应为 S 且匹配预期: %+v", o)
	}
	if summary.Total != 1 || summary.Passed != 1 || summary.SuccessRate != 1 {
		t.Errorf("汇总异常: %+v", summary)
	}
}

func TestRunManifestCountsHumanIntervention(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)
	manifest := Manifest{Cases: []Case{{
		ID:          "fail",
		Fixture:     "fixture",
		Requirement: "在仓库根目录创建 hello.txt，内容为 hello",
		Verify:      map[string]string{"check": "go run check.go"},
		BudgetUSD:   1,
	}}}

	summary, outcomes, err := RunManifest(context.Background(), dir, manifest, Options{
		WorkDir:    t.TempDir(),
		BackendFor: fakeWriting("bye\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes[0].Passed {
		t.Fatalf("验证失败时不应判为通过: %+v", outcomes[0])
	}
	if outcomes[0].Status != "NEEDS_HUMAN" {
		t.Errorf("持续验证失败应转人工，实际 %s", outcomes[0].Status)
	}
	if summary.HumanRate != 1 {
		t.Errorf("人工介入率应为 1，实际 %.2f", summary.HumanRate)
	}
}

func TestLoadRejectsInvalidManifests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.yaml")
	cases := []string{
		"cases: []\n",
		"cases:\n  - id: a\n    fixture: f\n    verify: {test: x}\n",
		"cases:\n  - id: a\n    fixture: f\n    requirement: r\n",
		"cases:\n  - id: a\n    fixture: f\n    requirement: r\n    verify: {test: x}\n  - id: a\n    fixture: g\n    requirement: r\n    verify: {test: x}\n",
	}
	for _, content := range cases {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("应拒绝非法清单:\n%s", content)
		}
	}
}
