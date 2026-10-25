package auth

import "testing"

func TestParseBearerAcceptsExactCase(t *testing.T) {
	got, ok := ParseBearer("Bearer abc123")
	if !ok || got != "abc123" {
		t.Fatalf("ParseBearer = (%q, %v)，期望 (abc123, true)", got, ok)
	}
}

// RFC 7235 规定 scheme 大小写不敏感，客户端发 "bearer" 是合法的。
func TestParseBearerAcceptsLowercaseScheme(t *testing.T) {
	got, ok := ParseBearer("bearer abc123")
	if !ok || got != "abc123" {
		t.Fatalf("小写 scheme 应被接受，实际 (%q, %v)", got, ok)
	}
}

func TestParseBearerTrimsSpaceAroundToken(t *testing.T) {
	got, ok := ParseBearer("Bearer   abc123  ")
	if !ok || got != "abc123" {
		t.Fatalf("应去掉令牌两侧空白，实际 (%q, %v)", got, ok)
	}
}

func TestParseBearerRejectsEmptyToken(t *testing.T) {
	if _, ok := ParseBearer("Bearer   "); ok {
		t.Fatal("空令牌不应被接受")
	}
}

func TestParseBearerRejectsOtherScheme(t *testing.T) {
	if _, ok := ParseBearer("Basic abc123"); ok {
		t.Fatal("非 Bearer scheme 不应被接受")
	}
}
