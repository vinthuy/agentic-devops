package window

import "errors"

// MaxSum 返回长度固定为 size 的连续子数组最大和。
func MaxSum(values []int, size int) (int, error) {
	if size <= 0 || size > len(values) {
		return 0, errors.New("size 非法")
	}
	current := 0
	for _, v := range values[:size-1] {
		current += v
	}
	best := current
	for i := size; i < len(values); i++ {
		current += values[i] - values[i-size]
		if current > best {
			best = current
		}
	}
	return best, nil
}
