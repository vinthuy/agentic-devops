// Package testutil 提供测试用的临时 git 仓库。
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// CheckSource 是验证命令执行的程序：hello.txt 必须内容为 hello。
const CheckSource = `package main

import (
	"os"
	"strings"
)

func main() {
	b, err := os.ReadFile("hello.txt")
	if err != nil || strings.TrimSpace(string(b)) != "hello" {
		os.Exit(1)
	}
}
`

// Git 在 dir 中执行 git 命令，失败时测试直接终止。
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v 失败: %v\n%s", args, err, out)
	}
	return string(out)
}

// NewRepo 创建一个 main 分支、含 README.md 与 check.go 的临时仓库。
func NewRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "upstream")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	Git(t, path, "init", "-b", "main")
	Git(t, path, "config", "user.name", "test")
	Git(t, path, "config", "user.email", "test@example.com")
	write(t, filepath.Join(path, "README.md"), "# demo\n")
	write(t, filepath.Join(path, "check.go"), CheckSource)
	Git(t, path, "add", "-A")
	Git(t, path, "commit", "-m", "init")
	return path
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
