package bsearch

import "testing"

func TestSearchFindsEachElement(t *testing.T) {
	values := []int{1, 3, 5, 7, 9}
	for want, target := range values {
		if got := Search(values, target); got != want {
			t.Errorf("Search(%v, %d) = %d，期望 %d", values, target, got, want)
		}
	}
}

func TestSearchMissing(t *testing.T) {
	if got := Search([]int{1, 3, 5}, 4); got != -1 {
		t.Errorf("Search 缺失值应为 -1，实际 %d", got)
	}
}

func TestSearchEmpty(t *testing.T) {
	if got := Search(nil, 1); got != -1 {
		t.Errorf("Search(nil) 应为 -1，实际 %d", got)
	}
}
