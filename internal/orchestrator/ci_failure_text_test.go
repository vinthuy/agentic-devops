package orchestrator

import (
	"strings"
	"testing"
)

// TestCIFailureTextKeepsTail 钉住回传给编码步骤的截断方向。
//
// 实测故障：CI 失败文本按头部截断，GitHub Actions 的样板日志（步骤名、环境
// 变量）把 2000 字预算占满，真正的 AssertionError 被切掉，编码步骤因此得出
// 「代码已正确、无需修改」，任务最终因为"没有可提交的改动"转人工。
func TestCIFailureTextKeepsTail(t *testing.T) {
	noise := strings.Repeat("test\tUNKNOWN STEP  2026-10-24T17:24:52Z   LD_LIBRARY_PATH: /opt/hostedtoolcache/Python\n", 200)
	assertion := "FAIL: test_none_is_rejected_with_value_error\nAssertionError: ValueError not raised"
	got := ciFailureText("1 项检查失败（test）", noise+assertion)

	if !strings.Contains(got, "ValueError not raised") {
		t.Fatalf("断言行必须保留，实际内容尾部为 %q", tailOf(got, 200))
	}
	if !strings.HasPrefix(got, "CI 失败：") {
		t.Fatalf("摘要必须始终在开头，实际 %q", headOf(got, 60))
	}
	if len([]rune(got)) > ciFailureLimit {
		t.Fatalf("超长：%d > %d", len([]rune(got)), ciFailureLimit)
	}
	if !strings.Contains(got, "前部已截断") {
		t.Errorf("截断后应标注，避免把半截日志当成全部证据")
	}
}

func TestCIFailureTextWithoutLogs(t *testing.T) {
	got := ciFailureText("2 项检查失败（lint, test）", "")
	if got != "CI 失败：2 项检查失败（lint, test）" {
		t.Fatalf("无日志时只回传摘要，实际 %q", got)
	}
}

func tailOf(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func headOf(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
