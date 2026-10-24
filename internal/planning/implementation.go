package planning

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ADR-31 的前提核查只管“测试缺失”这类主张：需求必须在谈测试/覆盖，且只搜测试文件。
// 但更常见的一类需求是对**现有实现**提主张——“parse_port 没处理空字符串”“slugify
// 遇到全大写出错”。规划器看不到仓库，这类需求它只能凭原文猜，于是照样可能产出
// 基于错误前提的改动。
//
// 这里沿用同一套做法：不判断主张真假，只把被点名符号在实现代码中的位置与片段注入
// 规划器，由它对照判断。判不了“函数是否真的处理了空字符串”，但能把函数摆到它面前。

// ImplHit 是需求点名的标识符在实现代码中的一处位置。
type ImplHit struct {
	Identifier string
	File       string
	Line       int
	// Definition 表示该行看起来是定义（func/def/class/赋值）而非调用点。
	// 定义处更有判断价值：调用点只能说明“有地方用了它”。
	Definition bool
}

// premiseVerbs 是“对现有实现有主张”的表述集合。与 coverage 的 premiseClaim 不同，
// 这里包含“没处理/不支持/出错”这类否定性描述和“改为/修复”这类改动要求——
// 两者都要求先看到现有实现才能判断主张是否成立。
//
// 不要只枚举具体动作（没处理/没校验/没跳过…）：那样每遇到一个新动词就漏一次，
// 实测里“没有跳过 vendor 目录”“重复写事件”都因为词表不含该动词而被静默跳过。
// 因此以否定标记（没有/未能/不会/无法）为主，用少量改动类动词补充。
// 注意不要用裸“未”：它会匹配“未来”这类无关词。
const premiseVerbs = `没|未能|尚未|未处理|未支持|未校验|未验证|未覆盖|` +
	`不处理|不支持|不会|无法|缺乏|不足|` +
	`忽略|遗漏|漏了|重复|冲突|不生效|失效|出错|报错|崩溃|导致|` +
	`边界|为空|空字符串|踩到|` +
	`改为|改成|变更|调整|修复|修正|重构|替换|删除|移除`

// implClaim 匹配“需求在对现有实现提主张或要求改动”的说法。
//
// 触发条件刻意比 coverage 宽，但仍然有门：纯新增功能（“新增一个 export_csv 命令”）
// 不该触发仓库遍历——此时符号本来就不存在，遍历只会白跑。
var implClaim = regexp.MustCompile(`(?i)(` + premiseVerbs + `|` +
	`缺失|缺少|没有覆盖|未被覆盖|未覆盖|尚无|暂无|不存在|没有测试|补充|补齐|补上|` +
	`doesn'?t|didn'?t|isn'?t|fails?|broken|not handled|unsupported|ignores?|breaks?|` +
	`fix|change|refactor|remove|missing|wrong|incorrect` +
	`)`)

// sourceFileExtensions 是可作为实现证据的源码扩展名。只认源码：
// 需求提到某个词时，README 或配置里的同名字符串不能说明实现。
var sourceFileExtensions = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".rb": true, ".java": true, ".kt": true, ".rs": true, ".c": true, ".h": true,
	".cc": true, ".cpp": true, ".hpp": true, ".cs": true, ".php": true, ".swift": true,
	".scala": true, ".sh": true, ".lua": true, ".pl": true, ".ex": true, ".exs": true,
}

// implFileName 判断是否是实现代码（源码且不是测试文件）。
func implFileName(base string) bool {
	lower := strings.ToLower(base)
	if testFileName(lower) {
		return false
	}
	return sourceFileExtensions[strings.ToLower(filepath.Ext(lower))]
}

// definitionMatchers 生成“看起来是定义”的匹配式。按语言各写一条，
// 因为定义语法差异大：Go 的 `func Name(`、Python 的 `def name`、JS 的
// `const name =` 无法用一条正则同时覆盖而不误匹配。
// 注意所有空白都用 `[ \t]` 而不是 `\s`：`\s` 会跨行匹配，
// `^\s*` 在多行模式下能把上一行末尾的换行吃掉，算出来的行号会偏一行。
func definitionMatchers(leaf string) []*regexp.Regexp {
	q := regexp.QuoteMeta(leaf)
	return []*regexp.Regexp{
		// Go：func Name( / func (r *T) Name(
		regexp.MustCompile(`(?m)^[ \t]*func[ \t]+(?:\([^)\n]*\)[ \t]*)?` + q + `[ \t]*[\[(]`),
		// Python / Ruby：def name / class Name
		regexp.MustCompile(`(?mi)^[ \t]*(?:def|class)[ \t]+` + q + `\b`),
		// JS / TS：function name( / const name =
		regexp.MustCompile(`(?m)^[ \t]*(?:export[ \t]+)?(?:default[ \t]+)?(?:async[ \t]+)?function[ \t]+` + q + `[ \t]*\(`),
		regexp.MustCompile(`(?mi)^[ \t]*(?:export[ \t]+)?(?:const|let|var)[ \t]+` + q + `[ \t]*=`),
		// 类型定义：type Name / interface Name / struct Name / enum Name
		regexp.MustCompile(`(?mi)^[ \t]*(?:export[ \t]+)?(?:type|interface|struct|enum)[ \t]+` + q + `\b`),
		// 方法简写：name(args) { —— 必须带函数体，否则会误把行首的调用语句
		// （如 `ParsePort("80")`）当成定义。
		regexp.MustCompile(`(?m)^[ \t]*` + q + `[ \t]*\([^)\n]*\)[ \t]*\{`),
		// 赋值或字段：name = / name:
		regexp.MustCompile(`(?m)^[ \t]*` + q + `[ \t]*(?:=|:)`),
	}
}

// ImplementationHits 在仓库的实现代码中查找需求点名的标识符。
//
// root 为空、需求没有对现有实现的主张、或没有命中源码时返回 nil。
// 返回值优先给定义处：每个标识符先收集定义命中，凑不够再补调用点。
func ImplementationHits(requirement, root string, maxIdentifiers int) []ImplHit {
	if root == "" || maxIdentifiers <= 0 {
		return nil
	}
	if !implClaim.MatchString(requirement) {
		return nil
	}
	identifiers := requirementIdentifiers(requirement)
	if len(identifiers) == 0 {
		return nil
	}
	if len(identifiers) > maxIdentifiers {
		identifiers = identifiers[:maxIdentifiers]
	}

	type matcher struct {
		leaf  string
		ref   *regexp.Regexp
		defs  []*regexp.Regexp
		found []scoredHit
	}
	matchers := make([]*matcher, 0, len(identifiers))
	seenLeaf := map[string]bool{}
	for _, id := range identifiers {
		leaf := id
		if i := strings.LastIndex(leaf, "."); i >= 0 {
			leaf = leaf[i+1:]
		}
		if leaf == "" || seenLeaf[leaf] {
			continue
		}
		ref, err := regexp.Compile(`\b` + regexp.QuoteMeta(leaf) + `\b`)
		if err != nil {
			continue
		}
		seenLeaf[leaf] = true
		matchers = append(matchers, &matcher{
			leaf: leaf,
			ref:  ref,
			defs: definitionMatchers(leaf),
		})
	}
	if len(matchers) == 0 {
		return nil
	}

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if coverageSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !implFileName(d.Name()) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() || info.Size() > coverageMaxScanBytes {
			return nil
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		text := string(content)
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		for _, m := range matchers {
			if len(m.found) >= implMaxCandidatesPerIdentifier {
				continue
			}
			if !m.ref.MatchString(text) {
				continue
			}
			// 记录每一条定义样式的命中，而不是遇到第一个就停：文件遍历顺序会让
			// “结构体字段同名赋值”排在真正的 `func` 定义前面，只取一条就会选错。
			matched := false
			for rank, def := range m.defs {
				loc := def.FindStringIndex(text)
				if loc == nil {
					continue
				}
				m.found = append(m.found, scoredHit{
					hit: ImplHit{
						Identifier: m.leaf,
						File:       rel,
						Line:       strings.Count(text[:loc[0]], "\n") + 1,
						Definition: true,
					},
					rank: rank,
				})
				matched = true
			}
			if matched || len(m.found) >= implMaxCandidatesPerIdentifier {
				continue
			}
			// 没有定义样式的行时退回调用点：至少能告诉规划器这个符号在哪被用到。
			if loc := m.ref.FindStringIndex(text); loc != nil {
				m.found = append(m.found, scoredHit{
					hit: ImplHit{
						Identifier: m.leaf,
						File:       rel,
						Line:       strings.Count(text[:loc[0]], "\n") + 1,
					},
					rank: refRank,
				})
			}
		}
		return nil
	})

	var hits []ImplHit
	for _, m := range matchers {
		// 按定义置信度（matcher 在上面的顺序）排序，再取前 N 条。
		// rank 越小越像真正的定义：`func Name(` 比 `Name:` 字段赋值可信得多。
		sort.SliceStable(m.found, func(i, j int) bool {
			if m.found[i].rank != m.found[j].rank {
				return m.found[i].rank < m.found[j].rank
			}
			if m.found[i].hit.File != m.found[j].hit.File {
				return m.found[i].hit.File < m.found[j].hit.File
			}
			return m.found[i].hit.Line < m.found[j].hit.Line
		})
		for i := 0; i < len(m.found) && i < implMaxHitsPerIdentifier; i++ {
			hits = append(hits, m.found[i].hit)
		}
	}
	if len(hits) == 0 {
		return nil
	}
	// 不再做全局重排：上面的 per-identifier 排序已经保证“定义优先”，
	// 若在这里按文件名再排一次，会把置信度顺序覆盖掉（实测让结构体字段赋值
	// 排到了真正的 func 定义前面）。标识符之间按需求出现顺序，已足够确定。
	return hits
}

const (
	// implMaxHitsPerIdentifier 限制单个标识符最终进入上下文的证据量。
	implMaxHitsPerIdentifier = 2
	// implMaxCandidatesPerIdentifier 是排序前的候选上限，宽于最终上限，
	// 以免先遇到的低置信命中把后面的真正定义挤掉。
	implMaxCandidatesPerIdentifier = 24
	// refRank 表示“仅引用”的置信度，排在所有定义样式之后。
	refRank = 100
	// implSnippetBefore / implSnippetAfter 是命中行前后各取的行数。
	// 取窗口而不是整个文件：大文件整个塞进去会挤掉需求本身，
	// 而函数体通常在定义行附近。
	implSnippetBefore = 25
	implSnippetAfter  = 55
)

// scoredHit 在扫描阶段附带定义置信度，仅在包内用于排序。
type scoredHit struct {
	hit  ImplHit
	rank int
}

// ImplementationSnippets 读取命中位置附近的代码窗口。
//
// 为什么不给整个文件：实现文件通常比测试文件大，`internal/cost/cost.go` 这类文件
// 整篇塞进上下文会稀释需求文本；而判断“是否处理了某种输入”只需要函数体。
func ImplementationSnippets(root string, hits []ImplHit, maxFiles, maxBytes int) map[string]string {
	if root == "" || maxFiles <= 0 || maxBytes <= 0 {
		return nil
	}
	linesByFile := make(map[string][]int, maxFiles)
	order := make([]string, 0, maxFiles)
	for _, h := range hits {
		if _, ok := linesByFile[h.File]; !ok {
			if len(order) >= maxFiles {
				continue
			}
			order = append(order, h.File)
		}
		linesByFile[h.File] = append(linesByFile[h.File], h.Line)
	}

	out := make(map[string]string, len(order))
	remaining := maxBytes
	for _, rel := range order {
		if remaining <= 0 {
			break
		}
		clean := filepath.Clean(filepath.FromSlash(rel))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		full := filepath.Join(root, clean)
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() || info.Size() > coverageMaxScanBytes {
			continue
		}
		content, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		lines := strings.Split(string(content), "\n")
		window := snippetWindow(lines, linesByFile[rel])
		if window == "" || len(window) > remaining {
			continue
		}
		out[rel] = window
		remaining -= len(window)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// snippetWindow 合并若干命中行的前后窗口，返回带真实行号的代码片段。
// 带行号是为了让规划器能引用具体位置，也便于把证据与搜索结果对上。
func snippetWindow(lines []string, hitLines []int) string {
	if len(lines) == 0 || len(hitLines) == 0 {
		return ""
	}
	sorted := append([]int(nil), hitLines...)
	sort.Ints(sorted)

	type span struct{ from, to int }
	var spans []span
	for _, line := range sorted {
		from := line - implSnippetBefore
		if from < 1 {
			from = 1
		}
		to := line + implSnippetAfter
		if to > len(lines) {
			to = len(lines)
		}
		if from > to {
			continue
		}
		if n := len(spans); n > 0 && from <= spans[n-1].to+1 {
			if to > spans[n-1].to {
				spans[n-1].to = to
			}
			continue
		}
		spans = append(spans, span{from, to})
	}

	var b strings.Builder
	for _, s := range spans {
		for i := s.from; i <= s.to; i++ {
			text := strings.TrimRight(lines[i-1], "\r")
			b.WriteString(strconv.Itoa(i))
			b.WriteString("| ")
			b.WriteString(text)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
