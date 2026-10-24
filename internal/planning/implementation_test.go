package planning

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const parsePortSource = `package main

import "strconv"

// ParsePort 把字符串端口转成整数。
func ParsePort(s string) (int, error) {
	// 空字符串在这里被拒绝。
	if s == "" {
		return 0, errInvalid
	}
	return strconv.Atoi(s)
}
`

// 需求对现有实现提主张时，应定位到定义处并标明这是定义。
func TestImplementationHitsPrefersDefinition(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "port.go", parsePortSource)
	writeFile(t, root, "main.go", "package main\n\nfunc main() {\n\tParsePort(\"80\")\n}\n")

	hits := ImplementationHits("`ParsePort` 没处理空字符串", root, 4)
	if len(hits) == 0 {
		t.Fatal("应定位到 ParsePort 的实现")
	}
	first := hits[0]
	if !first.Definition {
		t.Errorf("定义处应排在最前，实际 %+v", first)
	}
	if first.File != "port.go" {
		t.Errorf("应命中 port.go，实际 %s", first.File)
	}
	if first.Line != 6 {
		t.Errorf("ParsePort 定义在第 6 行，实际 %d", first.Line)
	}
}

// 结构体字段的同名赋值（`ExistingCoverage: coverage,`）不是定义：真正的 `func`
// 定义必须排在它前面。这是实测里踩到的排序 bug——字段赋值按文件名排序时
// 排到了 func 前面，规划器会看到一段无关的赋值而不是实现。
func TestImplementationHitsPrefersFuncOverFieldAssignment(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "aaa_use.go", "package main\n\nfunc build() Req {\n\treturn Req{\n\t\tParsePort: p,\n\t}\n}\n")
	writeFile(t, root, "zzz_def.go", "package main\n\nfunc ParsePort(s string) (int, error) {\n\treturn 0, nil\n}\n")

	hits := ImplementationHits("`ParsePort` 没有校验空字符串", root, 4)
	if len(hits) < 2 {
		t.Fatalf("应同时给出定义与引用，实际 %+v", hits)
	}
	if hits[0].File != "zzz_def.go" || hits[0].Line != 3 {
		t.Errorf("func 定义应排在字段赋值之前，实际首个为 %s:%d", hits[0].File, hits[0].Line)
	}
	if !hits[0].Definition {
		t.Error("首个命中应标记为定义")
	}
}

// 只有调用点时也要给出证据，但标记为非定义。
func TestImplementationHitsFallsBackToReference(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n\nfunc main() {\n\tx := helper_call()\n\t_ = x\n}\n")

	hits := ImplementationHits("`helper_call` 没处理 nil", root, 4)
	if len(hits) != 1 {
		t.Fatalf("应有一个引用命中，实际 %d 个", len(hits))
	}
	if hits[0].Definition {
		t.Error("调用点不应被标成定义")
	}
	if hits[0].Line != 4 {
		t.Errorf("应在第 4 行，实际 %d", hits[0].Line)
	}
}

// 纯新增功能不该触发仓库遍历：符号本来就不存在，遍历只会白跑。
func TestImplementationHitsSkipsAdditiveRequirements(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "port.go", parsePortSource)

	if hits := ImplementationHits("新增 `ParsePort` 函数用于解析端口", root, 4); hits != nil {
		t.Errorf("纯新增需求不应触发实现核查: %+v", hits)
	}
}

func TestImplementationHitsSkipsTestFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "port_test.go", "package main\n\nfunc TestParsePort() {\n\tParsePort(\"\")\n}\n")

	if hits := ImplementationHits("`ParsePort` 没处理空字符串", root, 4); hits != nil {
		t.Errorf("实现核查只应看非测试文件: %+v", hits)
	}
}

func TestImplementationHitsSkipsNonSourceFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "ParsePort 没处理空字符串。\n")
	writeFile(t, root, "config.yaml", "parse_port: 8080\n")

	if hits := ImplementationHits("`ParsePort` 没处理空字符串", root, 4); hits != nil {
		t.Errorf("文档与配置不应作为实现证据: %+v", hits)
	}
}

func TestImplementationHitsSkipsVendored(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "vendor/lib/port.go", parsePortSource)

	if hits := ImplementationHits("`ParsePort` 没处理空字符串", root, 4); hits != nil {
		t.Errorf("vendor 目录应跳过: %+v", hits)
	}
}

func TestImplementationHitsEmptyRootReturnsNil(t *testing.T) {
	if hits := ImplementationHits("`ParsePort` 没处理空字符串", "", 4); hits != nil {
		t.Errorf("root 为空应返回 nil: %+v", hits)
	}
}

func TestImplementationHitsRespectsIdentifierLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package main\n\nfunc alpha_fn() {}\n\nfunc beta_fn() {}\n\nfunc gamma_fn() {}\n")

	hits := ImplementationHits("`alpha_fn` 与 `beta_fn` 和 `gamma_fn` 都没处理空值", root, 2)
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.Identifier] = true
	}
	if len(seen) > 2 {
		t.Errorf("标识符数量应受上限约束，实际 %v", seen)
	}
}

// Python 的定义语法与 Go 不同，必须同样能定位到。
func TestImplementationHitsRecognizesPythonDefinition(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "parser.py", "def parse_port(s):\n    if s == \"\":\n        raise ValueError\n    return int(s)\n")

	hits := ImplementationHits("`parse_port` 没处理空字符串", root, 4)
	if len(hits) == 0 || !hits[0].Definition {
		t.Fatalf("应把 Python def 认成定义: %+v", hits)
	}
	if hits[0].Line != 1 {
		t.Errorf("应在第 1 行，实际 %d", hits[0].Line)
	}
}

// 片段要带行号且只取窗口，不是整个文件。
func TestImplementationSnippetsExtractsWindowWithLineNumbers(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	b.WriteString("package main\n\n")
	for i := 0; i < 200; i++ {
		b.WriteString("// filler\n")
	}
	b.WriteString("func Target() {}\n")
	writeFile(t, root, "big.go", b.String())

	hits := ImplementationHits("`Target` 没有校验", root, 4)
	if len(hits) == 0 {
		t.Fatal("应定位到 Target")
	}
	snips := ImplementationSnippets(root, hits, 1, 4000)
	text, ok := snips["big.go"]
	if !ok {
		t.Fatalf("应给出 big.go 的片段: %v", snips)
	}
	if !strings.Contains(text, "func Target() {}") {
		t.Error("片段应包含定义行")
	}
	if !strings.Contains(text, "| ") {
		t.Error("片段应带行号前缀")
	}
	if len(text) > 4000 {
		t.Errorf("片段不应超过预算，实际 %d 字节", len(text))
	}
	if strings.Count(text, "\n") > implSnippetBefore+implSnippetAfter+2 {
		t.Errorf("应只取窗口而非整个文件，实际 %d 行", strings.Count(text, "\n"))
	}
}

func TestImplementationSnippetsRespectsFileBudget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.go", "package main\n\nfunc alpha_fn() {}\n")
	writeFile(t, root, "b.go", "package main\n\nfunc beta_fn() {}\n")

	hits := []ImplHit{
		{Identifier: "alpha_fn", File: "a.go", Line: 3, Definition: true},
		{Identifier: "beta_fn", File: "b.go", Line: 3, Definition: true},
	}
	snips := ImplementationSnippets(root, hits, 1, 4000)
	if len(snips) != 1 {
		t.Errorf("文件数应受上限约束，实际 %d 个", len(snips))
	}
}

func TestImplementationSnippetsRejectsEscapingPaths(t *testing.T) {
	root := t.TempDir()
	snips := ImplementationSnippets(root, []ImplHit{{Identifier: "x", File: "../outside.go", Line: 1}}, 1, 4000)
	if len(snips) != 0 {
		t.Errorf("越界路径应被拒绝: %v", snips)
	}
}
