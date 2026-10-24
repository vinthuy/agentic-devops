package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/state"
	"agentdev/internal/store"
	"agentdev/internal/testutil"
)

// seedSettled 造出 N 个已结算（DONE）任务并写入花费，用于测试成本预测。
// 直接改 spent_usd 走 AddSpend，避免依赖真实模型调用。
func seedSettled(t *testing.T, home, repo, level string, costs []float64) []string {
	t.Helper()
	s, err := store.Open(filepath.Join(home, "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var ids []string
	for _, cost := range costs {
		task, err := s.CreateTask(store.NewTask{Repo: repo, Requirement: "x", BudgetUSD: 5})
		if err != nil {
			t.Fatal(err)
		}
		// CreateTask 后为 QUEUED；逐跳走到 DONE，保证 SpendSamples 的已结算筛选命中。
		path := []state.Status{
			state.Triaging, state.Planning, state.Coding, state.Verifying,
			state.Reviewing, state.Committing, state.Done,
		}
		for i, next := range path {
			var fields map[string]any
			if i == 0 {
				fields = map[string]any{"level": level}
			}
			task, err = s.Transition(task.ID, task.Version, next, nil, fields)
			if err != nil {
				t.Fatalf("转换到 %s 失败: %v", next, err)
			}
		}
		if err := s.AddSpend(task.ID, cost); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	return ids
}

// 20 个 S 级基准用例的真实花费，升序后的中位数 0.0083。
// P90 用 (n+1)p 顺序统计量取第 19 个值 0.0170（线性插值会给 0.01538，
// 但实测覆盖率只有 73%，见 forecast.upperQuantile 的注释）。
var forecastFixtureCosts = []float64{
	0.0170, 0.0062, 0.0083, 0.0103, 0.0077, 0.0084, 0.0063, 0.0072, 0.0104, 0.0094,
	0.0058, 0.0067, 0.0092, 0.0213, 0.0152, 0.0109, 0.0064, 0.0076, 0.0083, 0.0069,
}

func forecastHome(t *testing.T) string {
	t.Helper()
	home := writeHome(t, testutil.NewRepo(t))
	seedSettled(t, home, "acme/app", "S", forecastFixtureCosts)
	return home
}

func TestCostForecastReportsPercentiles(t *testing.T) {
	home := forecastHome(t)
	code, out, errOut := invoke(t, "cost", "forecast", "--repo", "acme/app", "--level", "S", "--home", home)
	if code != ExitOK {
		t.Fatalf("退出码 %d，stderr: %s", code, errOut)
	}
	for _, want := range []string{"样本 20", "同仓库同级别", "$0.0083", "$0.0170"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q：\n%s", want, out)
		}
	}
	// 范围列必须同时给出最低与最高，避免只给区间中段让人误判尾部风险。
	if !strings.Contains(out, "$0.0058") || !strings.Contains(out, "$0.0213") {
		t.Errorf("输出应包含完整范围：\n%s", out)
	}
}

// 样本不足时必须拒绝给数字，而不是用两条记录编一个区间。
func TestCostForecastRefusesSmallSamples(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	seedSettled(t, home, "acme/app", "S", []float64{0.01, 0.02})
	code, out, errOut := invoke(t, "cost", "forecast", "--repo", "acme/app", "--level", "S", "--home", home)
	if code != ExitOK {
		t.Fatalf("退出码 %d，stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "样本不足") {
		t.Errorf("样本不足时应明确说明：\n%s", out)
	}
	if strings.Contains(out, "P50") {
		t.Errorf("样本不足时不应给出分位数：\n%s", out)
	}
}

func TestCostForecastFallsBackAndSaysSo(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	// 只有同仓库的其他级别样本充足。
	seedSettled(t, home, "acme/app", "M", []float64{0.4, 0.41, 0.42, 0.43, 0.44})
	code, out, errOut := invoke(t, "cost", "forecast", "--repo", "acme/app", "--level", "S", "--home", home)
	if code != ExitOK {
		t.Fatalf("退出码 %d，stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "同仓库（跨级别）") {
		t.Errorf("应如实标注回退层级：\n%s", out)
	}
	if !strings.Contains(out, "已放宽筛选条件") {
		t.Errorf("回退时应提示可比性下降：\n%s", out)
	}
}

func TestCostForecastJSON(t *testing.T) {
	home := forecastHome(t)
	code, out, errOut := invoke(t, "cost", "forecast", "--repo", "acme/app", "--level", "S", "--json", "--home", home)
	if code != ExitOK {
		t.Fatalf("退出码 %d，stderr: %s", code, errOut)
	}
	var payload struct {
		Available bool    `json:"available"`
		Basis     string  `json:"basis"`
		Samples   int     `json:"samples"`
		P50       float64 `json:"p50_usd"`
		P90       float64 `json:"p90_usd"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, out)
	}
	if !payload.Available || payload.Basis != "repo+level" || payload.Samples != 20 {
		t.Errorf("元信息错误: %+v", payload)
	}
	if payload.P50 != 0.0083 || payload.P90 < 0.0169 || payload.P90 > 0.0171 {
		t.Errorf("分位数错误: %+v", payload)
	}
}

func TestCostForecastJSONUnavailable(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	code, out, errOut := invoke(t, "cost", "forecast", "--json", "--home", home)
	if code != ExitOK {
		t.Fatalf("退出码 %d，stderr: %s", code, errOut)
	}
	var payload struct {
		Available bool `json:"available"`
		Minimum   int  `json:"minimum_samples"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("JSON 解析失败: %v\n%s", err, out)
	}
	if payload.Available {
		t.Error("无样本时 available 必须为 false")
	}
	if payload.Minimum <= 0 {
		t.Error("应暴露最低样本数，便于判断还差多少")
	}
}

func TestCostForecastRejectsBadLevel(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	code, _, errOut := invoke(t, "cost", "forecast", "--level", "X", "--home", home)
	if code != ExitUsage {
		t.Fatalf("非法级别应返回用法错误，实际 %d，stderr: %s", code, errOut)
	}
}

// 没有记录过预测时，校准命令应说明原因，而不是打印 0%。
func TestCostForecastAccuracyWithoutForecasts(t *testing.T) {
	home := forecastHome(t)
	code, out, errOut := invoke(t, "cost", "forecast", "--accuracy", "--home", home)
	if code != ExitOK {
		t.Fatalf("退出码 %d，stderr: %s", code, errOut)
	}
	if !strings.Contains(out, "尚无") {
		t.Errorf("无预测记录时应明确说明：\n%s", out)
	}
}

// 有预测记录时，按实际花费是否超过 P90 判定，并统计命中率。
func TestCostForecastAccuracyComparesActualToP90(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	ids := seedSettled(t, home, "acme/app", "S", []float64{0.005, 0.05})

	s, err := store.Open(filepath.Join(home, "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	// 第一个任务的预测覆盖实际（0.005 <= 0.02），第二个超出（0.05 > 0.02）。
	for _, id := range ids {
		if err := s.AppendEvent(id, "cost_forecast", map[string]any{
			"basis": "repo+level", "samples": 20,
			"p50_usd": 0.01, "p90_usd": 0.02, "min_usd": 0.005, "max_usd": 0.05,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := invoke(t, "cost", "forecast", "--accuracy", "--home", home)
	if code != ExitOK {
		t.Fatalf("退出码 %d，stderr: %s", code, errOut)
	}
	for _, want := range []string{"2 个任务", "50%", "在 P90 内", "超出 P90"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q：\n%s", want, out)
		}
	}
}
