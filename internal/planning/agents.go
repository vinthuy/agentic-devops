package planning

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// AgentsMaxChars 是注入规划与审查的 AGENTS.md 正文上限。
const AgentsMaxChars = 8000

// LoadAgentsMD 读取仓库根目录的 AGENTS.md。文件不存在时返回空字符串。
// 超过上限时只保留前部，并标明来源与截断。
func LoadAgentsMD(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	if utf8.RuneCountInString(text) > AgentsMaxChars {
		text = string([]rune(text)[:AgentsMaxChars]) + "\n…（AGENTS.md 已截断）"
	}
	return "来源：仓库文件 AGENTS.md\n" + text, nil
}
