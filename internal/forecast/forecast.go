// Package forecast 用历史任务的实际花费估计一次任务的花费区间。
//
// 为什么不报“平均值”：单次任务花费强偏态。同一批 20 个 S 级基准用例里，
// 最便宜的 $0.0058、最贵的 $0.0213，相差近 4 倍；均值会被少数长尾任务抬高，
// 用它做“预计花费”会让大多数任务看起来比实际贵。因此报中位数（P50）与 P90：
// 前者是“典型一次”，后者是“多数情况下不会超过”。
//
// 样本不足时不给数字。1～2 条历史记录算出来的“区间”没有意义，
// 却会让人误以为已经量化过风险。
package forecast

import (
	"math"
	"sort"
)

// Sample 是一条历史任务的花费记录。
type Sample struct {
	Repo    string
	Level   string
	CostUSD float64
}

// MinSamples 是给出估计所需的最少历史样本数。
// 取 5 是因为再少时 P90 基本等于最大值，等于把单次异常当成了上限。
const MinSamples = 5

// Estimate 是一次花费估计。
type Estimate struct {
	Samples int
	Basis   string // 用了哪一层筛选：repo+level / repo / level / all
	P50     float64
	P90     float64
	Min     float64
	Max     float64
}

// EstimateFrom 按 repo 与 level 逐步放宽条件挑选样本，返回估计。
//
// 放宽顺序是有意的：同仓库同级别的任务最可比；样本不够时先放宽级别保留仓库
// （技术栈与验证命令更接近），再放宽仓库保留级别。最终仍不足则返回 ok=false，
// 而不是给一个基于个位数样本的数字。
func EstimateFrom(samples []Sample, repo, level string) (Estimate, bool) {
	tiers := []struct {
		basis string
		keep  func(Sample) bool
	}{
		{"repo+level", func(s Sample) bool { return s.Repo == repo && s.Level == level }},
		{"repo", func(s Sample) bool { return s.Repo == repo }},
		{"level", func(s Sample) bool { return s.Level == level }},
		{"all", func(Sample) bool { return true }},
	}
	if repo == "" && level == "" {
		// 没有筛选条件时只保留“全部”这一层，避免用空条件返回一个看似精确的分层标签。
		tiers = []struct {
			basis string
			keep  func(Sample) bool
		}{{"all", func(Sample) bool { return true }}}
	}
	for _, tier := range tiers {
		if tier.basis == "repo+level" && (repo == "" || level == "") {
			continue
		}
		if tier.basis == "repo" && repo == "" {
			continue
		}
		if tier.basis == "level" && level == "" {
			continue
		}
		costs := make([]float64, 0, len(samples))
		for _, s := range samples {
			// 0 花费通常是“还没调用模型就结束”，计入会把中位数拉到 0。
			if s.CostUSD <= 0 || !tier.keep(s) {
				continue
			}
			costs = append(costs, s.CostUSD)
		}
		if len(costs) < MinSamples {
			continue
		}
		sort.Float64s(costs)
		return Estimate{
			Samples: len(costs),
			Basis:   tier.basis,
			P50:     median(costs),
			P90:     upperQuantile(costs, 0.90),
			Min:     costs[0],
			Max:     costs[len(costs)-1],
		}, true
	}
	return Estimate{}, false
}

// percentile 用线性插值计算分位数；调用方保证 values 已升序且非空。
//
// 注释保留以说明历史：v0.5.0 的 P90 用这个规则（numpy/R 默认的 type 7）。
// 实测 15 个真实任务的 P90 覆盖率只有 73%，低于名义 90%——小样本下线性插值
// 取的位次偏低，系统性欠覆盖。现在 P90 改用 upperQuantile，见其注释。
func percentile(values []float64, q float64) float64 {
	if len(values) == 1 {
		return values[0]
	}
	if q <= 0 {
		return values[0]
	}
	if q >= 1 {
		return values[len(values)-1]
	}
	pos := q * float64(len(values)-1)
	lower := int(pos)
	if lower >= len(values)-1 {
		return values[len(values)-1]
	}
	frac := pos - float64(lower)
	return values[lower] + frac*(values[lower+1]-values[lower])
}

// median 返回中位数；偶数个取中间两个的平均。调用方保证 values 已升序且非空。
func median(values []float64) float64 {
	n := len(values)
	if n%2 == 1 {
		return values[n/2]
	}
	return (values[n/2-1] + values[n/2]) / 2
}

// upperQuantile 用 (n+1)p 的顺序统计量估计上分位，缺省回退到最大值。
//
// 为什么不用线性插值：第 k 个顺序统计量对一次新观测的期望覆盖率是 k/(n+1)。
// 插值把结果放在两个顺序统计量之间，实际覆盖率低于名义分位——实测 15 个真实
// 任务只有 73%。取 ceil((n+1)p) 个值可消除这个向下偏差。
//
// 代价是样本少时它会退化成最大值（n=5 时 ceil(0.9×6)=6 > 5，直接取最大值）。
// 这是应当保留的性质：5 个样本估不出 90 分位，把结果插到最大值以下只是虚假精度。
// 输出里同时给出 P90 与 Max，读者能看出二者何时相等。
func upperQuantile(values []float64, q float64) float64 {
	n := len(values)
	pos := q * float64(n+1)
	k := int(math.Ceil(pos - 1e-9))
	if k >= n {
		return values[n-1]
	}
	if k < 1 {
		return values[0]
	}
	return values[k-1]
}
