package clamp

// Clamp 把 v 限制在 [low, high] 区间内。
func Clamp(v, low, high int) int {
	if v > high {
		return low
	}
	if v < low {
		return high
	}
	return v
}
