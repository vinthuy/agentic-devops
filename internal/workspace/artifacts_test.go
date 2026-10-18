package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/config"
)

func TestIsGeneratedArtifact(t *testing.T) {
	for _, path := range []string{
		"__pycache__/module.cpython-313.pyc",
		"src/__pycache__/module.pyc",
		".pytest_cache/v/cache/nodeids",
		"web/node_modules/pkg/index.js",
		"build/Example.class",
	} {
		if !IsGeneratedArtifact(path) {
			t.Errorf("%q should be treated as generated", path)
		}
	}
	for _, path := range []string{
		"src/main.go",
		"tests/test_cache.py",
		"docs/node_modules.md",
		"target/release-notes.md",
	} {
		if IsGeneratedArtifact(path) {
			t.Errorf("%q should not be treated as generated", path)
		}
	}
}

func TestStageAllUnstagesGeneratedArtifactsButKeepsThemOnDisk(t *testing.T) {
	root := t.TempDir()
	if _, err := Git(root, "init", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Git(root, "add", "README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := Git(root, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	base, err := Git(root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	base = strings.TrimSpace(base)

	if err := os.MkdirAll(filepath.Join(root, "src", "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(root, "src", "__pycache__", "module.cpython-313.pyc")
	if err := os.WriteFile(generated, []byte("bytecode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := New(&config.App{})
	excluded, err := mgr.StageAll(Info{Path: root, BaseSHA: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(excluded) != 1 || excluded[0] != "src/__pycache__/module.cpython-313.pyc" {
		t.Fatalf("unexpected exclusions: %#v", excluded)
	}
	staged, err := Git(root, "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(staged) != "src/main.go" {
		t.Fatalf("only source should be staged, got %q", staged)
	}
	if _, err := os.Stat(generated); err != nil {
		t.Fatalf("excluded artifact should remain on disk: %v", err)
	}
	status, err := Git(root, "status", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "?? src/__pycache__/") {
		t.Errorf("generated artifact should remain untracked for audit, status=%q", status)
	}
}
