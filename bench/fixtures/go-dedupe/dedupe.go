package dedupe

import "sort"

// Dedupe 去掉重复项，并保持元素第一次出现的顺序。
func Dedupe(items []string) []string {
	seen := make(map[string]bool, len(items))
	var out []string
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}
