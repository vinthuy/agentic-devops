// Package cost 负责把 token 用量换算成费用，并与预算上限比较。
package cost

import (
	"errors"

	"agentdev/internal/llm"
)

// ErrBudgetExceeded 表示任务花费已达到预算上限，编排器应停止调用模型。
var ErrBudgetExceeded = errors.New("任务预算已用尽")

// Rates 为每百万 token 的单价（美元）。
type Rates struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Table 按模型名索引单价。
type Table map[string]Rates

// Cost 计算一次用量的费用。第二个返回值表示该模型是否配置了单价；
// 未配置时返回 0，调用方应显式提示，避免把“未知”误当成“免费”。
func (t Table) Cost(model string, u llm.Usage) (float64, bool) {
	r, ok := t[model]
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000.0
	total := float64(u.InputTokens)*r.Input +
		float64(u.OutputTokens)*r.Output +
		float64(u.CacheReadTokens)*r.CacheRead +
		float64(u.CacheWriteTokens)*r.CacheWrite
	return total / perMillion, true
}

// WarningThreshold 是发出预算预警的比例。
const WarningThreshold = 0.8
