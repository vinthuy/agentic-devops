package triage

import (
	"testing"

	"agentdev/internal/config"
)

func TestRulesClassifyBySignals(t *testing.T) {
	repo := config.RepoConfig{SensitivePaths: []string{"**/auth/**", "src/secrets/**"}}
	cases := []struct {
		name string
		text string
		want Level
	}{
		{"普通小改动", "修正 validators.py 中邮箱校验的正则", S},
		{"高风险领域", "修改 auth 模块的 token 校验逻辑", M},
		{"敏感路径", "调整 src/secrets 下的配置读取", M},
		{"大范围改动", "重构整个仓库的分层结构", L},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Rules(c.text, repo)
			if got.Level != c.want {
				t.Errorf("分级应为 %s，实际 %s（理由 %v）", c.want, got.Level, got.Reasons)
			}
			if len(got.Reasons) == 0 {
				t.Error("应给出分级理由")
			}
			if got.ModelUsed {
				t.Error("规则分级不应标记为使用了模型")
			}
		})
	}
}

func TestCombineTakesMoreConservativeLevel(t *testing.T) {
	rule := Result{Level: S, Reasons: []string{"规则理由"}}
	model := Result{Level: M, Reasons: []string{"模型理由"}, ModelUsed: true}
	got := Combine(rule, model)
	if got.Level != M || !got.ModelUsed {
		t.Errorf("应取更保守分级并保留模型标记: %+v", got)
	}
	if len(got.Reasons) != 2 {
		t.Errorf("应保留双方理由: %v", got.Reasons)
	}

	// 反向：模型给 S 不应把规则给出的 M 降级。
	got = Combine(Result{Level: M}, Result{Level: S, ModelUsed: true})
	if got.Level != M {
		t.Errorf("不应降级: %v", got.Level)
	}
}

func TestParseLevel(t *testing.T) {
	if _, set, err := Parse(""); err != nil || set {
		t.Error("空字符串应表示未指定")
	}
	if lvl, set, err := Parse(" m "); err != nil || !set || lvl != M {
		t.Errorf("应解析出 M: %v %v %v", lvl, set, err)
	}
	if _, _, err := Parse("X"); err == nil {
		t.Error("非法分级应当报错")
	}
}

func TestLevelString(t *testing.T) {
	if S.String() != "S" || M.String() != "M" || L.String() != "L" {
		t.Error("分级字符串错误")
	}
}
