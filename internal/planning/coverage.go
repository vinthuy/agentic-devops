package planning

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 规划器看不到仓库，只能看到文件列表与需求点名的文件。于是它能判断“要改成什么样”，
// 却无法判断“需求说的问题是否真的存在”。PR #1 就是这样产出的：需求要求补充缓存计价
// 测试，而同名断言在基线提交上早已存在，规划、编码、验证、审查四步全部通过。
//
// 这里在规划前做一次确定性的覆盖核查：需求声称某标识符“缺失/未覆盖”时，去现有测试
// 文件里查该标识符是否已被引用。命中就把证据交给规划器，由它判断前提是否成立。
// 只做证据注入、不做判定，因为“引用了同名符号”不等于“断言了要求的行为”。
type CoverageHit struct {
	Identifier string
	File       string
	Line       int
}

// premiseClaim 匹配“某能力/测试尚不存在或需要补上”的说法。没有这类主张时不做核查：
// 新增功能与修 bug 的任务不需要这份证据，也不该被它干扰。
// 注意“补充/补齐”也要算进来——PR #1 的需求原文就是“补充缓存计价测试”，
// 只认“缺少”会漏掉最常见的表述。
var premiseClaim = regexp.MustCompile(`(?i)(缺失|缺少|没有覆盖|未被覆盖|未覆盖|尚无|暂无|不存在|缺乏|没有测试|没有断言|补充|补齐|补上|missing|absent|untested|not covered|no tests?|lacks?)`)

// testContext 要求需求确实在说测试/覆盖。只为了避免“补充日志字段”这类普通需求
// 触发一次仓库遍历——那不是覆盖核查关心的事。
var testContext = regexp.MustCompile(`(?i)(测试|用例|断言|覆盖|test|coverage|assert|spec)`)

var (
	backtickedName = regexp.MustCompile("`([^`\n]{2,60})`")
	dottedSymbol   = regexp.MustCompile(`\b[A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)+\b`)
	snakeSymbol    = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)
	plainSymbol    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
)

// testFileName 判断是否是测试文件。只搜测试文件：覆盖核查关心的是“有没有测试”，
// 生产代码里出现同名符号不能说明已有覆盖。
func testFileName(base string) bool {
	lower := strings.ToLower(base)
	switch {
	case strings.HasSuffix(lower, "_test.go"),
		strings.HasSuffix(lower, "_test.py"),
		strings.HasSuffix(lower, "_test.rb"),
		strings.HasSuffix(lower, "_test.js"),
		strings.HasSuffix(lower, "_test.ts"),
		strings.HasSuffix(lower, ".test.js"),
		strings.HasSuffix(lower, ".test.ts"),
		strings.HasSuffix(lower, ".test.tsx"),
		strings.HasSuffix(lower, ".spec.js"),
		strings.HasSuffix(lower, ".spec.ts"),
		strings.HasSuffix(lower, ".spec.tsx"):
		return true
	}
	return strings.HasPrefix(lower, "test_") && strings.HasSuffix(lower, ".py")
}

var coverageSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, "out": true, ".venv": true, "venv": true, "__pycache__": true,
	".idea": true, ".vscode": true, "testdata": true, ".agentdev": true,
}

const (
	coverageMaxScanBytes = 512 << 10
	coverageMaxBytesHits = 3
)

// ExistingCoverage 在仓库的测试文件中查找需求点名的标识符，返回命中位置。
// root 为空、需求没有“缺失”类主张、或没有命中时返回 nil。
func ExistingCoverage(requirement, root string, maxIdentifiers int) []CoverageHit {
	if root == "" || maxIdentifiers <= 0 || !premiseClaim.MatchString(requirement) {
		return nil
	}
	if !testContext.MatchString(requirement) {
		return nil
	}
	identifiers := requirementIdentifiers(requirement)
	if len(identifiers) == 0 {
		return nil
	}
	if len(identifiers) > maxIdentifiers {
		identifiers = identifiers[:maxIdentifiers]
	}

	// 每个标识符用其最后一段去匹配：`Table.Cost` 在测试里通常写作 `tbl.Cost(...)`。
	matchers := make(map[string]*regexp.Regexp, len(identifiers))
	order := make([]string, 0, len(identifiers))
	for _, id := range identifiers {
		leaf := id
		if i := strings.LastIndex(leaf, "."); i >= 0 {
			leaf = leaf[i+1:]
		}
		if leaf == "" {
			continue
		}
		re, err := regexp.Compile(`\b` + regexp.QuoteMeta(leaf) + `\b`)
		if err != nil {
			continue
		}
		matchers[id] = re
		order = append(order, id)
	}

	perIdentifier := make(map[string]int, len(order))
	var hits []CoverageHit
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
		base := d.Name()
		if !testFileName(base) {
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
		for _, id := range order {
			if perIdentifier[id] >= coverageMaxBytesHits {
				continue
			}
			loc := matchers[id].FindStringIndex(text)
			if loc == nil {
				continue
			}
			hits = append(hits, CoverageHit{
				Identifier: id,
				File:       rel,
				Line:       strings.Count(text[:loc[0]], "\n") + 1,
			})
			perIdentifier[id]++
		}
		return nil
	})

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Identifier != hits[j].Identifier {
			return hits[i].Identifier < hits[j].Identifier
		}
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Line < hits[j].Line
	})
	if len(hits) == 0 {
		return nil
	}
	return hits
}

// CoverageSnippets 读取命中的测试文件内容。只给“文件:行号”不够：规划器看不到仓库，
// 没有内容就无法判断已有断言到底覆盖了什么，只能猜。
func CoverageSnippets(root string, hits []CoverageHit, maxFiles, maxBytes int) map[string]string {
	if root == "" || maxFiles <= 0 || maxBytes <= 0 {
		return nil
	}
	out := make(map[string]string, maxFiles)
	for _, h := range hits {
		if len(out) >= maxFiles {
			break
		}
		if _, ok := out[h.File]; ok {
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(h.File))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		full := filepath.Join(root, clean)
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() || info.Size() > int64(maxBytes*8) {
			continue
		}
		content, err := readLimited(full, maxBytes)
		if err != nil {
			continue
		}
		out[h.File] = content
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// requirementIdentifiers 抽取需求中点名的符号：反引号内容、Go 风格 Type.Method、snake_case。
// 顺序按首次出现，便于输出稳定。
func requirementIdentifiers(requirement string) []string {
	type span struct {
		text string
		at   int
	}
	var found []span
	for _, m := range backtickedName.FindAllStringSubmatchIndex(requirement, -1) {
		name := strings.TrimSpace(requirement[m[2]:m[3]])
		if plainSymbol.MatchString(name) {
			found = append(found, span{name, m[0]})
		}
	}
	for _, re := range []*regexp.Regexp{dottedSymbol, snakeSymbol} {
		for _, m := range re.FindAllStringIndex(requirement, -1) {
			found = append(found, span{requirement[m[0]:m[1]], m[0]})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].at < found[j].at })

	seen := map[string]bool{}
	out := make([]string, 0, len(found))
	for _, s := range found {
		if seen[s.text] {
			continue
		}
		seen[s.text] = true
		out = append(out, s.text)
	}
	return out
}
