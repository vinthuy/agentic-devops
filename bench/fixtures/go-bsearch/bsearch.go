package bsearch

// Search 在升序切片中查找 target，返回下标；不存在返回 -1。
func Search(values []int, target int) int {
	low, high := 0, len(values)-1
	for low < high {
		mid := (low + high) / 2
		if values[mid] == target {
			return mid
		}
		if values[mid] < target {
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return -1
}
