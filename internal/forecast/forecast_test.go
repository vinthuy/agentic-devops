package forecast

import (
	"math"
	"testing"
)

func closeEnough(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// 20 个 S 级基准用例的真实花费，用于验证分位数落在合理位置。
var benchCosts = []float64{
	0.0170, 0.0062, 0.0083, 0.0103, 0.0077, 0.0084, 0.0063, 0.0072, 0.0104, 0.0094,
	0.0058, 0.0067, 0.0092, 0.0213, 0.0152, 0.0109, 0.0064, 0.0076, 0.0083, 0.0069,
}

func benchSamples() []Sample {
	out := make([]Sample, 0, len(benchCosts))
	for _, c := range benchCosts {
		out = append(out, Sample{Repo: "r", Level: "S", CostUSD: c})
	}
	return out
}

func TestEstimateUsesSortedPercentiles(t *testing.T) {
	est, ok := EstimateFrom(benchSamples(), "r", "S")
	if !ok {
		t.Fatal("样本充足时应给出估计")
	}
	if est.Samples != 20 || est.Basis != "repo+level" {
		t.Fatalf("样本数或来源错误: %+v", est)
	}
	// 20 个值升序后中位数是第 10、11 个 (0.0083、0.0083) 的平均。
	if !closeEnough(est.P50, 0.0083) {
		t.Errorf("P50 错误: %v", est.P50)
	}
	// P90 取 ceil(0.9×21)=19 个顺序统计量，即 0.0170。
	// 线性插值会给出 0.01538，实测覆盖率只有 73%，因此改用顺序统计量。
	if !closeEnough(est.P90, 0.0170) {
		t.Errorf("P90 错误: %v", est.P90)
	}
	if !closeEnough(est.Min, 0.0058) || !closeEnough(est.Max, 0.0213) {
		t.Errorf("范围错误: %+v", est)
	}
	if est.P50 > est.P90 {
		t.Error("P50 不应大于 P90")
	}
}

// P90 必须严格低于最大值，否则等于把单次异常当成上限。
func TestP90IsBelowMaxOnSkewedData(t *testing.T) {
	est, ok := EstimateFrom(benchSamples(), "r", "S")
	if !ok {
		t.Fatal("应给出估计")
	}
	if est.P90 >= est.Max {
		t.Errorf("P90(%v) 不应等于最大值(%v)", est.P90, est.Max)
	}
}

func TestEstimateRefusesSmallSamples(t *testing.T) {
	if _, ok := EstimateFrom(benchSamples()[:MinSamples-1], "r", "S"); ok {
		t.Errorf("样本少于 %d 时不应给出估计", MinSamples)
	}
}

// 同仓库同级别不足时，先放宽级别、保留仓库。
func TestEstimateFallsBackToRepoThenLevel(t *testing.T) {
	var samples []Sample
	for i := 0; i < MinSamples; i++ {
		samples = append(samples, Sample{Repo: "r", Level: "M", CostUSD: 0.5})
	}
	est, ok := EstimateFrom(samples, "r", "S")
	if !ok {
		t.Fatal("同仓库的其他级别样本足够时应回退到 repo")
	}
	if est.Basis != "repo" {
		t.Errorf("应回退到 repo 层，实际 %s", est.Basis)
	}

	samples = nil
	for i := 0; i < MinSamples; i++ {
		samples = append(samples, Sample{Repo: "other", Level: "S", CostUSD: 0.01})
	}
	est, ok = EstimateFrom(samples, "r", "S")
	if !ok {
		t.Fatal("同级别的其他仓库样本足够时应回退到 level")
	}
	if est.Basis != "level" {
		t.Errorf("应回退到 level 层，实际 %s", est.Basis)
	}
}

// 回退到全量时，来源必须如实标为 all，不能冒充“同仓库同级别”。
func TestEstimateReportsAllBasisWhenNothingMatches(t *testing.T) {
	var samples []Sample
	for i := 0; i < MinSamples; i++ {
		samples = append(samples, Sample{Repo: "other", Level: "L", CostUSD: 0.02})
	}
	est, ok := EstimateFrom(samples, "r", "S")
	if !ok {
		t.Fatal("全量样本足够时应给出估计")
	}
	if est.Basis != "all" {
		t.Errorf("来源应标为 all，实际 %s", est.Basis)
	}
}

func TestEstimateIgnoresZeroCostSamples(t *testing.T) {
	samples := []Sample{
		{Repo: "r", Level: "S", CostUSD: 0},
		{Repo: "r", Level: "S", CostUSD: 0},
	}
	for i := 0; i < MinSamples; i++ {
		samples = append(samples, Sample{Repo: "r", Level: "S", CostUSD: 0.01})
	}
	est, ok := EstimateFrom(samples, "r", "S")
	if !ok {
		t.Fatal("应给出估计")
	}
	if est.Samples != MinSamples {
		t.Errorf("0 花费样本不应计入，实际样本数 %d", est.Samples)
	}
	if !closeEnough(est.P50, 0.01) {
		t.Errorf("中位数被 0 值拉低: %v", est.P50)
	}
}

func TestEstimateWithoutFiltersUsesAllSamples(t *testing.T) {
	est, ok := EstimateFrom(benchSamples(), "", "")
	if !ok {
		t.Fatal("无筛选条件时应使用全量")
	}
	if est.Basis != "all" || est.Samples != len(benchCosts) {
		t.Errorf("无筛选条件时的来源或样本数错误: %+v", est)
	}
}

func TestPercentileEdges(t *testing.T) {
	values := []float64{1, 2, 3, 4}
	if !closeEnough(median(values), 2.5) {
		t.Error("偶数个的中位数应为中间两个的平均")
	}
	if !closeEnough(median([]float64{1, 2, 3}), 2) {
		t.Error("奇数个的中位数应为中间值")
	}
	if !closeEnough(upperQuantile([]float64{7}, 0.9), 7) {
		t.Error("单值样本应返回该值")
	}
	if !closeEnough(upperQuantile(values, 0.9), 4) {
		t.Error("小样本上分位应回退到最大值")
	}
	// 大样本时上分位不应该是最大值：否则等于把所有异常都当成上限。
	wide := make([]float64, 20)
	for i := range wide {
		wide[i] = float64(i + 1)
	}
	if !closeEnough(upperQuantile(wide, 0.9), 19) {
		t.Errorf("n=20 时 P90 应为第 19 个值，实际 %v", upperQuantile(wide, 0.9))
	}
}

// 样本刚好等于下限时，P90 必然等于最大值——5 个点估不出 90 分位，
// 插值到最大值以下只会制造虚假精度。这个行为要有测试固定住。
func TestP90EqualsMaxAtMinimumSamples(t *testing.T) {
	est, ok := EstimateFrom(benchSamples()[:MinSamples], "r", "S")
	if !ok {
		t.Fatal("样本刚好达标时应给出估计")
	}
	if est.P90 != est.Max {
		t.Errorf("样本等于下限时 P90 应等于最大值: p90=%v max=%v", est.P90, est.Max)
	}
}

// P90 必须随样本增加而收敛到真实上分位，不会因为样本多而虚高。
func TestP90ShrinksBelowMaxAsSamplesGrow(t *testing.T) {
	est, ok := EstimateFrom(benchSamples(), "r", "S")
	if !ok {
		t.Fatal("应给出估计")
	}
	if est.P90 >= est.Max {
		t.Errorf("20 个样本时 P90(%v) 应低于最大值(%v)", est.P90, est.Max)
	}
}

// 空样本不能返回“0 元”这种看似确定的估计。
func TestEstimateEmptySamples(t *testing.T) {
	if _, ok := EstimateFrom(nil, "r", "S"); ok {
		t.Error("空样本不应给出估计")
	}
}
