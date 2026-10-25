package bench

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/store"
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

// 重复采样：同一个用例跑多次，Summary 按运行次数统计，ByCase 给出通过/运行。
func TestRunManifestRepeatsAndAggregatesByCase(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)
	manifest := Manifest{Cases: []Case{{
		ID:          "pass",
		Fixture:     "fixture",
		Requirement: "在仓库根目录创建 hello.txt，内容为 hello",
		Verify:      map[string]string{"check": "go run check.go"},
		BudgetUSD:   1,
	}}}

	summary, outcomes, err := RunManifest(context.Background(), dir, manifest, Options{
		WorkDir:    t.TempDir(),
		Repeat:     2,
		BackendFor: fakeWriting("hello\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 2 || outcomes[0].Run != 1 || outcomes[1].Run != 2 {
		t.Fatalf("应产生两次运行: %+v", outcomes)
	}
	if summary.Trials != 2 || summary.Total != 1 || summary.Passed != 2 || summary.SuccessRate != 1 {
		t.Errorf("汇总应按运行次数统计: %+v", summary)
	}
	stats := ByCase(outcomes)
	if len(stats) != 1 || stats[0].Runs != 2 || stats[0].Passed != 2 {
		t.Errorf("按用例统计异常: %+v", stats)
	}
}

// 共用一个数据目录时，后续用例能看到前面用例的花费样本；
// 反之，独立目录下每个用例都从零开始。
func TestSharedHomeAccumulatesSpendSamples(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)
	manifest := Manifest{Cases: []Case{{
		ID:          "pass",
		Fixture:     "fixture",
		Requirement: "在仓库根目录创建 hello.txt，内容为 hello",
		Verify:      map[string]string{"check": "go run check.go"},
		BudgetUSD:   1,
	}}}

	shared := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RunManifest(context.Background(), dir, manifest, Options{
		WorkDir:    t.TempDir(),
		Repeat:     3,
		SharedHome: shared,
		BackendFor: fakeWriting("hello\n"),
	}); err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(filepath.Join(shared, "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tasks, err := s.ListTasks("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Fatalf("共用数据目录应累积 3 个任务，实际 %d 个", len(tasks))
	}

	// 对照：不共用时每个用例独立目录，结束后数据目录被清理，不会累积。
	isolated := filepath.Join(t.TempDir(), "isolated")
	if _, _, err := RunManifest(context.Background(), dir, manifest, Options{
		WorkDir:    isolated,
		Repeat:     2,
		BackendFor: fakeWriting("hello\n"),
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(isolated)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "home" {
			t.Errorf("未指定 SharedHome 时不应在 workdir 下留下共享数据目录: %s", e.Name())
		}
	}
}

// 基础设施故障（余额不足）不得计入成功率分母。
func TestSummarizeExcludesBlockedRuns(t *testing.T) {
	outcomes := []Outcome{
		{ID: "a", Run: 1, Status: "DONE", Passed: true, SpentUSD: 0.01},
		{ID: "b", Run: 1, Status: "NEEDS_HUMAN", Blocked: true, SpentUSD: 0.02},
	}
	summary := summarize(outcomes, 2)
	if summary.Trials != 2 || summary.Blocked != 1 || summary.Passed != 1 {
		t.Fatalf("汇总异常: %+v", summary)
	}
	if summary.SuccessRate != 1 {
		t.Errorf("分母应排除被阻断的运行，成功率=%v", summary.SuccessRate)
	}
	// 被阻断的运行也花了钱，总花费必须如实反映。
	if summary.TotalCostUSD != 0.03 {
		t.Errorf("总花费应包含被阻断运行的实际支出，实际 %v", summary.TotalCostUSD)
	}
	if summary.MeanCostUSD != 0.01 {
		t.Errorf("平均花费应按执行次数计算，实际 %v", summary.MeanCostUSD)
	}
	stats := ByCase(outcomes)
	if len(stats) != 2 || stats[1].Blocked != 1 || stats[1].Runs != 0 {
		t.Errorf("按用例统计应标记阻断: %+v", stats)
	}
}

// 提交到仓库的清单必须合法，且每个 fixture 目录都存在，避免文档与代码漂移。
func TestCommittedManifestIsValid(t *testing.T) {
	root := filepath.Join("..", "..", "bench")
	manifest, err := Load(filepath.Join(root, "tasks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Cases) < 20 {
		t.Errorf("基准集应至少 20 个用例，实际 %d", len(manifest.Cases))
	}
	for _, c := range manifest.Cases {
		if _, err := os.Stat(filepath.Join(root, c.Fixture)); err != nil {
			t.Errorf("%s: fixture 不存在: %v", c.ID, err)
		}
	}
	// 全是 S 级的基准会饱和在 100%，再也测不出质量退化。至少保留若干 M 级用例，
	// 让「审批门之后还能不能跑完」这条路径一直有样本。
	levels := map[string]int{}
	for _, c := range manifest.Cases {
		levels[c.ExpectLevel]++
	}
	if levels["M"] < 4 {
		t.Errorf("基准集应至少含 4 个 M 级用例（当前 S=%d M=%d L=%d）", levels["S"], levels["M"], levels["L"])
	}
	// 单函数改动的 M 用例只检验「审批门之后流水线能不能跑完」。至少要有 1 个
	// 跨文件的用例，否则规划质量（能否同时改对多处）始终没有样本。
	multiFile := false
	for _, c := range manifest.Cases {
		if c.ID == "py-session-refresh" {
			multiFile = true
		}
	}
	if !multiFile {
		t.Error("基准集缺少跨文件 M 级用例 py-session-refresh：单文件改动测不出规划质量")
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

// M 级用例的需求措辞会命中 triage 的高风险信号，流水线在编码前停下来等人审批。
// 无人值守的基准里这是死路：不加 --auto-approve 时它会以 AWAITING_APPROVAL 收场，
// 而不是被误判成「产品做不出来」。
func TestMLevelCaseParksWithoutAutoApprove(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)
	manifest := Manifest{Cases: []Case{{
		ID:          "m-park",
		Fixture:     "fixture",
		Requirement: "调整鉴权头解析，并更新访问令牌的过期判断",
		Verify:      map[string]string{"check": "go run check.go"},
		ExpectLevel: "M",
		BudgetUSD:   1,
	}}}

	_, outcomes, err := RunManifest(context.Background(), dir, manifest, Options{
		WorkDir:    t.TempDir(),
		BackendFor: fakeWriting("hello\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	o := outcomes[0]
	if o.Status != "AWAITING_APPROVAL" {
		t.Fatalf("M 级应停在 AWAITING_APPROVAL，实际 %s", o.Status)
	}
	if o.Passed {
		t.Error("停在审批门的运行不应算通过")
	}
	if o.SyntheticApproval {
		t.Error("未开启 AutoApprove 时不应记录合成审批")
	}
}

// 开启 AutoApprove 后，M 级用例用合成审批继续跑完，且必须在结果里留痕：
// 否则「M 级通过」会被读成「审批门验证通过」，而门其实是被基准绕开的。
func TestMLevelCaseCompletesWithSyntheticApproval(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir)
	manifest := Manifest{Cases: []Case{{
		ID:          "m-pass",
		Fixture:     "fixture",
		Requirement: "调整鉴权头解析，并更新访问令牌的过期判断",
		Verify:      map[string]string{"check": "go run check.go"},
		ExpectLevel: "M",
		BudgetUSD:   1,
	}}}

	summary, outcomes, err := RunManifest(context.Background(), dir, manifest, Options{
		WorkDir:     t.TempDir(),
		AutoApprove: true,
		BackendFor:  fakeWriting("hello\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	o := outcomes[0]
	if o.Status != "DONE" || !o.Passed {
		t.Fatalf("合成审批后应完成，实际 %s（%s）", o.Status, o.Err)
	}
	if o.Level != "M" || !o.LevelMatched {
		t.Errorf("分级应为 M 且匹配预期: %+v", o)
	}
	if !o.SyntheticApproval {
		t.Error("必须记录合成审批，否则报告会高估审批门的覆盖")
	}
	if summary.Passed != 1 {
		t.Errorf("汇总应计为通过: %+v", summary)
	}
}
