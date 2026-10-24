package planning

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRepo 在临时目录里建一个最小仓库，返回根目录。
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("写文件失败: %v", err)
		}
	}
	return root
}

// 复现 PR #1：需求要求补充缓存计价测试，而测试文件里已经有 tbl.Cost 的调用。
func TestExistingCoverageFindsSymbolAlreadyUsedInTests(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"internal/cost/cost.go": "package cost\n\nfunc (t Table) Cost(model string) float64 { return 0 }\n",
		"internal/cost/cost_test.go": "package cost\n\nimport \"testing\"\n\n" +
			"func TestTableComputesCostPerMillionTokens(t *testing.T) {\n" +
			"\tusd, ok := tbl.Cost(\"m\", llm.Usage{CacheReadTokens: 10_000})\n" +
			"\t_ = usd\n\t_ = ok\n}\n",
	})
	requirement := "为 internal/cost 的 `Table.Cost` 补充缓存计价测试：新增用例覆盖 cache_read 与 cache_write 同时命中时的计费"

	hits := ExistingCoverage(requirement, root, 8)
	if len(hits) == 0 {
		t.Fatal("应在现有测试中找到 Cost 的引用")
	}
	byIdent := map[string]CoverageHit{}
	for _, h := range hits {
		byIdent[h.Identifier] = h
	}
	hit, ok := byIdent["Table.Cost"]
	if !ok {
		t.Fatalf("应以需求原文中的 Table.Cost 为键，实际: %+v", hits)
	}
	if hit.File != "internal/cost/cost_test.go" {
		t.Errorf("命中文件错误: %s", hit.File)
	}
	if hit.Line != 6 {
		t.Errorf("命中行号错误: %d，期望 6", hit.Line)
	}
}

// 没有“缺失”类主张时不做核查：新增功能的任务不需要这份证据。
func TestExistingCoverageSkipsRequirementsWithoutMissingClaim(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"internal/cost/cost_test.go": "package cost\n\nfunc TestX(t *testing.T) { _ = tbl.Cost(0) }\n",
	})
	if hits := ExistingCoverage("把 Table.Cost 改为按百万 token 计价", root, 8); hits != nil {
		t.Errorf("无缺失主张时不应产出证据，实际: %+v", hits)
	}
}

// “补充”类主张但说的不是测试时不核查，避免为了无关需求遍历整个仓库。
func TestExistingCoverageSkipsNonTestRequirements(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"internal/cost/cost_test.go": "package cost\n\nfunc TestX(t *testing.T) { _ = tbl.Cost(0) }\n",
	})
	if hits := ExistingCoverage("补充 Cost 字段的日志输出", root, 8); hits != nil {
		t.Errorf("与测试无关的需求不应产出覆盖证据，实际: %+v", hits)
	}
}

// 只搜测试文件：生产代码里出现同名符号不能说明已有覆盖。
func TestExistingCoverageIgnoresNonTestFiles(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"internal/cost/cost.go":       "package cost\n\nfunc (t Table) Cost(m string) float64 { return 0 }\n",
		"internal/cost/helper.go":     "package cost\n\nvar useCost = Cost\n",
		"internal/cost/other_test.go": "package cost\n\nfunc TestOther(t *testing.T) {}\n",
	})
	if hits := ExistingCoverage("缺少 `Cost` 的测试", root, 8); hits != nil {
		t.Errorf("非测试文件不应计入覆盖证据，实际: %+v", hits)
	}
}

// 跳过 vendor / node_modules：那里的测试不是本仓库的覆盖。
func TestExistingCoverageSkipsVendoredTests(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"vendor/lib/lib_test.go":         "package lib\n\nvar _ = Cost(1)\n",
		"node_modules/pkg/thing.test.js": "test('x', () => { cost(1) })\n",
	})
	if hits := ExistingCoverage("缺少 `Cost` 的覆盖", root, 8); hits != nil {
		t.Errorf("依赖目录不应计入覆盖证据，实际: %+v", hits)
	}
}

// 需求点名的标识符超过上限时，按出现顺序截断，保证输出可控。
func TestExistingCoverageRespectsIdentifierLimit(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"pkg/a_test.go": "package pkg\n\nvar _ = alpha\nvar _ = beta\nvar _ = gamma\n",
	})
	hits := ExistingCoverage("缺少 `alpha`、`beta`、`gamma` 的覆盖", root, 2)
	for _, h := range hits {
		if h.Identifier == "gamma" {
			t.Errorf("超过上限的标识符不应被检索: %+v", hits)
		}
	}
	if len(hits) == 0 {
		t.Fatal("上限内应至少有一个命中")
	}
}

func TestExistingCoverageEmptyRootReturnsNil(t *testing.T) {
	if hits := ExistingCoverage("缺少 `Cost` 的测试", "", 8); hits != nil {
		t.Errorf("root 为空时应返回 nil，实际: %+v", hits)
	}
}

// 需求点名的文件不存在时不应影响检索，也不应报错。
func TestExistingCoverageToleratesMissingPaths(t *testing.T) {
	root := writeRepo(t, map[string]string{
		"pkg/a_test.go": "package pkg\n\nvar _ = alpha\n",
	})
	hits := ExistingCoverage("`alpha` 缺少测试，参考 `does/not/exist.go`", root, 8)
	if len(hits) != 1 || hits[0].Identifier != "alpha" {
		t.Errorf("应只命中 alpha，实际: %+v", hits)
	}
}
