package window

import "testing"

func TestMaxSum(t *testing.T) {
	got, err := MaxSum([]int{1, 4, 2, 10, 2, 3, 1, 0, 20}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got != 24 {
		t.Fatalf("MaxSum = %d，期望 24", got)
	}
}

func TestMaxSumWholeSlice(t *testing.T) {
	got, err := MaxSum([]int{1, 2, 3}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got != 6 {
		t.Fatalf("MaxSum = %d，期望 6", got)
	}
}

func TestMaxSumInvalidSize(t *testing.T) {
	if _, err := MaxSum([]int{1}, 0); err == nil {
		t.Fatal("size=0 应返回错误")
	}
}
