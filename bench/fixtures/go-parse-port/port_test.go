package port

import "testing"

func TestParseTrimsSurroundingSpaces(t *testing.T) {
	for _, raw := range []string{"8080", " 8080", "8080 ", " 8080 "} {
		got, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q) 返回错误: %v", raw, err)
		}
		if got != 8080 {
			t.Fatalf("Parse(%q) = %d，期望 8080", raw, got)
		}
	}
}

func TestParseRejectsOutOfRange(t *testing.T) {
	for _, raw := range []string{"0", "65536", "-1"} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) 应当返回错误", raw)
		}
	}
}
