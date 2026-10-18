package planning

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMentionedFilesReadsNamedSources(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "status.go"), []byte("package status\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "calc.py"), []byte("def add():\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 未被点名的文件不应被读取。
	if err := os.WriteFile(filepath.Join(root, "secret.md"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := MentionedFiles("修复 status.go 中的 Text，并参考 internal/calc.py", root, 3, 1000)
	if len(got) != 2 {
		t.Fatalf("应读取 2 个被点名的文件，实际 %#v", got)
	}
	if !strings.Contains(got["status.go"], "package status") {
		t.Errorf("status.go 内容缺失: %q", got["status.go"])
	}
	if !strings.Contains(got["internal/calc.py"], "def add") {
		t.Errorf("internal/calc.py 内容缺失: %q", got["internal/calc.py"])
	}
	if _, ok := got["secret.md"]; ok {
		t.Error("未点名的文件不应被读取")
	}
}

func TestMentionedFilesRejectsEscapesAndRespectsLimits(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "..", "outside.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(root, "big.go")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 500)), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := MentionedFiles("看 ../outside.md", root, 3, 100); len(got) != 0 {
		t.Errorf("越界路径不应被读取: %#v", got)
	}
	got := MentionedFiles("看 big.go", root, 3, 100)
	if len(got) != 1 {
		t.Fatalf("应读取 big.go: %#v", got)
	}
	if !strings.Contains(got["big.go"], "已截断") {
		t.Errorf("超长内容应标注截断: %q", got["big.go"])
	}
	if n := len([]byte(got["big.go"])); n > 200 {
		t.Errorf("截断后长度异常: %d", n)
	}
}

func TestMentionedFilesLimitsFileCount(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := MentionedFiles("a.go b.go c.go d.go", root, 2, 1000)
	if len(got) != 2 {
		t.Errorf("最多读取 2 个文件，实际 %d", len(got))
	}
}
