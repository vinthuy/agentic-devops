package cost

import (
	"errors"
	"testing"

	"agentdev/internal/llm"
)

func TestTableComputesCostPerMillionTokens(t *testing.T) {
	tbl := Table{"m": {Input: 1_000_000, Output: 2_000_000, CacheRead: 100, CacheWrite: 200}}
	usd, ok := tbl.Cost("m", llm.Usage{
		InputTokens: 1, OutputTokens: 1, CacheReadTokens: 10_000, CacheWriteTokens: 5_000,
	})
	if !ok {
		t.Fatal("已配置单价的模型应返回 ok")
	}
	// 1*1e6/1e6 + 1*2e6/1e6 + 10000*100/1e6 + 5000*200/1e6 = 1 + 2 + 1 + 1 = 5
	if usd != 5 {
		t.Errorf("费用计算错误: %v", usd)
	}
}

func TestTableUnknownModelReturnsNotOK(t *testing.T) {
	tbl := Table{}
	if usd, ok := tbl.Cost("ghost", llm.Usage{InputTokens: 1000}); ok || usd != 0 {
		t.Errorf("未配置单价应返回 (0,false)，实际 (%v,%v)", usd, ok)
	}
}

func TestUsageAddAndEmpty(t *testing.T) {
	var zero llm.Usage
	if !zero.Empty() {
		t.Error("零用量应为空")
	}
	sum := llm.Usage{InputTokens: 1, OutputTokens: 2}.Add(llm.Usage{InputTokens: 3, CacheReadTokens: 4})
	if sum.InputTokens != 4 || sum.OutputTokens != 2 || sum.CacheReadTokens != 4 {
		t.Errorf("用量累加错误: %+v", sum)
	}
}

func TestErrBudgetExceededIsSentinel(t *testing.T) {
	wrapped := errors.New("x")
	if errors.Is(wrapped, ErrBudgetExceeded) {
		t.Error("不应误判为预算错误")
	}
	if !errors.Is(ErrBudgetExceeded, ErrBudgetExceeded) {
		t.Error("哨兵错误应可被 errors.Is 命中")
	}
}
