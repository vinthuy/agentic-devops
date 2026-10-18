package list

import "strings"

// Parse 把逗号分隔的列表解析为元素切片，忽略空白与空项。
func Parse(raw string) []string {
	return strings.Split(raw, ",")
}
