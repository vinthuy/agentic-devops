package bench

import "testing"

// TestInfraBlocked 钉住「基础设施故障不算产品能力」的边界。
//
// 实测背景：本机到 GitHub 的连接会成段中断（TLS 握手被重置、443 连不上）。
// 若把这些运行算成失败，一次 6 用例的测量会报出 17% 成功率，而实际产品行为
// 一条都没被测到。它们必须从成功率与人工介入率的分母里剔除。
func TestInfraBlocked(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "基线分支推不上去",
			text: "准备远端基线失败: 推送基线分支失败: 推送分支失败: git push origin bench/x 失败: " +
				"fatal: unable to access 'https://github.com/o/r.git/': Failed to connect to github.com port 443 after 21043 ms: Couldn't connect to server",
			want: true,
		},
		{
			name: "TLS 握手被重置",
			text: "推送分支失败: git push --set-upstream origin agent/x 失败: fatal: unable to access " +
				"'https://github.com/o/r.git/': schannel: failed to receive handshake, SSL/TLS connection failed",
			want: true,
		},
		{
			name: "认证失败是配置问题，不是基础设施",
			text: "推送分支失败: fatal: Authentication failed for 'https://github.com/o/r.git/'",
			want: false,
		},
		{
			name: "任务自身的阻塞原因（非网络）不算阻断",
			text: "验收标准不可执行且已达重规划上限",
			want: false,
		},
		{
			name: "空文本",
			text: "",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := infraBlocked(c.text); got != c.want {
				t.Fatalf("infraBlocked(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

// TestSummaryExcludesBlocked 确认阻断运行不进入任何能力分母。
func TestSummaryExcludesBlocked(t *testing.T) {
	s := summarize([]Outcome{
		{Status: "DONE", Passed: true, SpentUSD: 0.01, CIVerdict: "passed", CIFirstPass: true},
		{Status: "ERROR", Blocked: true, SpentUSD: 0.0, Err: "准备远端基线失败: unable to access"},
	}, 2)
	if s.Trials != 2 {
		t.Fatalf("Trials 应统计全部运行，实际 %d", s.Trials)
	}
	if s.Blocked != 1 {
		t.Fatalf("应记录 1 次阻断，实际 %d", s.Blocked)
	}
	if s.SuccessRate != 1 {
		t.Fatalf("阻断运行不应拉低成功率，实际 %.2f", s.SuccessRate)
	}
	if s.HumanRate != 0 {
		t.Fatalf("阻断运行不应算作人工介入，实际 %.2f", s.HumanRate)
	}
}
