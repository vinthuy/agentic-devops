package status

// Text 返回常见 HTTP 状态码的短语。
func Text(code int) string {
	switch code {
	case 200:
		return "OK"
	case 404:
		return "Not Found"
	case 500:
		return "Internal Server Error"
	}
	return "Unknown"
}
