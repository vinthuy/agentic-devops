package planning

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// RetrievalMaxReads 是一次规划前检索最多打开的文件数。
	RetrievalMaxReads = 8
	// RetrievalMaxChars 是单个文件摘录的字符上限。
	RetrievalMaxChars = 4000
	// RetrievalMaxTotal 是全部摘录的字符上限。
	RetrievalMaxTotal = 16000
)

// PathChecker 用现有路径策略判断一个相对路径能否被读取。
type PathChecker interface {
	CheckPath(raw string, write bool) (string, error)
}

// Retrieval 是一次只读检索的结果。Files 的键是仓库内相对路径。
type Retrieval struct {
	Files  map[string]string
	Reads  int
	Bytes  int
	Denied []string
}

// BehaviorClaim 表示需求在描述仓库里已有的行为，而不是单纯新增一个文件。
// 系统追加的补充说明和驳回标记不参与判断，避免“补充说明”这几个字本身触发检索。
func BehaviorClaim(requirement string) bool {
	return implClaim.MatchString(behaviorText(requirement))
}

func behaviorText(requirement string) string {
	for _, marker := range []string{"[补充说明]", "[审批驳回]"} {
		if i := strings.Index(requirement, marker); i >= 0 {
			requirement = requirement[:i]
		}
	}
	return requirement
}

// Unlocated 表示确定性查找和只读检索都没有把需求对上仓库内容。
func (r Requirement) Unlocated() bool {
	if !BehaviorClaim(r.Text) {
		return false
	}
	return len(r.Implementation) == 0 && len(r.FileSnippets) == 0 &&
		len(r.ExistingCoverage) == 0 && len(r.RetrievalFiles) == 0
}

// Retrieve 在符号查找没有命中时，按路径策略只读地找候选文件。
// 它不写文件，也不运行命令。被拒绝的路径不会打开。
func Retrieve(requirement, root string, checker PathChecker) Retrieval {
	out := Retrieval{}
	if root == "" || checker == nil || !BehaviorClaim(requirement) {
		return out
	}
	denied := map[string]bool{}
	remember := func(path string) {
		path = filepath.ToSlash(path)
		if path == "" || denied[path] {
			return
		}
		denied[path] = true
		out.Denied = append(out.Denied, path)
	}
	for _, path := range explicitPaths(requirement) {
		if _, err := checker.CheckPath(path, false); err != nil {
			remember(path)
		}
	}

	terms := searchTerms(requirement)
	if len(terms) == 0 {
		return out
	}
	var paths []string
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
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() || info.Size() > coverageMaxScanBytes {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if _, err := checker.CheckPath(rel, false); err != nil {
			remember(rel)
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	sort.Slice(paths, func(i, j int) bool {
		return pathRank(paths[i], terms) < pathRank(paths[j], terms)
	})

	files := map[string]string{}
	for _, rel := range paths {
		if out.Reads >= RetrievalMaxReads || out.Bytes >= RetrievalMaxTotal {
			break
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		out.Reads++
		text := string(content)
		if !containsTerm(text, terms) {
			continue
		}
		budget := RetrievalMaxTotal - out.Bytes
		if budget > RetrievalMaxChars {
			budget = RetrievalMaxChars
		}
		excerpt, n := takeRunes(text, budget)
		if n == 0 {
			break
		}
		files[rel] = excerpt
		out.Bytes += n
	}
	if len(files) > 0 {
		out.Files = files
	}
	return out
}

func pathRank(rel string, terms []string) string {
	base := strings.ToLower(filepath.Base(rel))
	for _, term := range terms {
		if strings.Contains(strings.ToLower(base), strings.ToLower(term)) {
			return "0" + rel
		}
	}
	return "1" + rel
}

func containsTerm(text string, terms []string) bool {
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func takeRunes(text string, limit int) (string, int) {
	if limit <= 0 || text == "" {
		return "", 0
	}
	if utf8.RuneCountInString(text) <= limit {
		return text, utf8.RuneCountInString(text)
	}
	runes := []rune(text)
	return string(runes[:limit]) + "\n…（内容过长，已截断）", limit
}

var explicitPathRE = regexp.MustCompile(`(?:\.\./)+[^\s,，、]+|\.[A-Za-z0-9][\w.-]*|[\w.-]+(?:/[\w.-]+)+`)

func explicitPaths(text string) []string {
	raw := explicitPathRE.FindAllString(text, -1)
	seen := map[string]bool{}
	var out []string
	for _, path := range raw {
		path = strings.Trim(path, "`\"'，、。")
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, filepath.ToSlash(path))
	}
	return out
}

func searchTerms(requirement string) []string {
	seen := map[string]bool{}
	var terms []string
	add := func(term string) {
		if term == "" || seen[term] {
			return
		}
		seen[term] = true
		terms = append(terms, term)
	}
	for _, word := range englishWord.FindAllString(requirement, -1) {
		add(word)
	}
	for _, seq := range hanSequence.FindAllString(requirement, -1) {
		runes := []rune(seq)
		if len(runes) >= 2 && len(runes) <= 12 {
			add(seq)
		}
		for i := 0; i+1 < len(runes); i++ {
			if stopRune(runes[i]) || stopRune(runes[i+1]) {
				continue
			}
			add(string(runes[i : i+2]))
		}
	}
	return terms
}

var (
	englishWord = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]{3,}`)
	hanSequence = regexp.MustCompile(`\p{Han}{2,}`)
)

func stopRune(r rune) bool {
	return unicode.Is(unicode.Han, r) && strings.ContainsRune("的了在和与并或是为把被将对从到中下上及", r)
}
