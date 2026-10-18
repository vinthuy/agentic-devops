package planning

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// mentionedFile 匹配需求文本中直接点名的文件，例如 user.go、src/app.py、docs/README.md。
var mentionedFile = regexp.MustCompile(
	`[A-Za-z0-9_][A-Za-z0-9_./\-]*\.(?:go|py|js|ts|tsx|jsx|java|rs|rb|php|cs|cpp|cc|h|hpp|md|yaml|yml|json|toml|sql)`,
)

// MentionedFiles 读取需求中直接点名的文件内容，供规划器参考。
//
// 规划器没有工具，只能看到文件列表。如果需求已经点名了某个文件，却不让它看到内容，
// 它就会退化成“请提供 file 内容”的假澄清——基准集实测出现过这种情况。
// 这里只读需求明确提到的文件，并限制数量与大小，避免把整个仓库塞进上下文。
func MentionedFiles(requirement, root string, maxFiles, maxBytes int) map[string]string {
	if root == "" || maxFiles <= 0 || maxBytes <= 0 {
		return nil
	}
	out := make(map[string]string, maxFiles)
	seen := map[string]bool{}
	for _, candidate := range mentionedFile.FindAllString(requirement, -1) {
		if len(out) >= maxFiles {
			break
		}
		if seen[candidate] {
			continue
		}
		seen[candidate] = true

		clean := filepath.Clean(filepath.FromSlash(candidate))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			continue
		}
		full := filepath.Join(root, clean)
		// 只读仓库内、需求直接点名的普通文件。
		if rel, err := filepath.Rel(root, full); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() || info.Size() > int64(maxBytes*8) {
			continue
		}
		content, err := readLimited(full, maxBytes)
		if err != nil {
			continue
		}
		out[filepath.ToSlash(clean)] = content
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func readLimited(path string, maxBytes int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)))
	if err != nil {
		return "", err
	}
	if len(buf) == maxBytes {
		return string(buf) + "\n…（内容过长，已截断）", nil
	}
	return string(buf), nil
}
