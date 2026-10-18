package port

import (
	"errors"
	"strconv"
)

// Parse 把配置中的端口字符串解析为 1..65535 的整数。
func Parse(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if n < 1 || n > 65535 {
		return 0, errors.New("端口超出范围")
	}
	return n, nil
}
