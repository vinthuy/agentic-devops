package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDefaultsWhenFilesMissing(t *testing.T) {
	home := t.TempDir()
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repos) != 0 {
		t.Errorf("默认不应有仓库")
	}
	if cfg.Delivery.Enabled {
		t.Error("delivery 默认应关闭")
	}
	if cfg.Policy.MaxFixAttempts != 3 {
		t.Errorf("max_fix_attempts 默认值异常: %d", cfg.Policy.MaxFixAttempts)
	}
	if cfg.Models.APIKeyEnv != "DEEPSEEK_API_KEY" || !strings.Contains(cfg.Models.BaseURL, "deepseek") {
		t.Errorf("模型默认配置应为 DeepSeek: %+v", cfg.Models)
	}
	if cfg.DBPath() != filepath.Join(home, "agentdev.db") {
		t.Errorf("数据库路径异常: %s", cfg.DBPath())
	}
}

func TestLoadRepoPreservesVerifyOrderAndDefaultsBranch(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "repos.yaml", `repos:
  acme/app:
    local_path: /tmp/app
    verify_commands:
      lint: "npm run lint"
      typecheck: "npm run typecheck"
      test: "npm test"
    budget_usd: 3.5
`)
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	repo := cfg.Repos["acme/app"]
	if repo.DefaultBranch != "main" {
		t.Errorf("default_branch 应默认为 main，实际 %q", repo.DefaultBranch)
	}
	var names []string
	for _, c := range repo.VerifyCommands {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "lint,typecheck,test" {
		t.Errorf("验证命令顺序丢失: %v", names)
	}
	if repo.BudgetUSD != 3.5 {
		t.Errorf("budget_usd: %v", repo.BudgetUSD)
	}
}

func TestRepoBudgetDefaultsToTwoDollars(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "repos.yaml", `repos:
  acme/app:
    local_path: /tmp/app
    verify_commands:
      test: "go test ./..."
`)
	cfg, err := Load(home)
	if err != nil {
		t.Fatalf("未配置 budget_usd 应当使用默认值: %v", err)
	}
	if cfg.Repos["acme/app"].BudgetUSD != DefaultBudgetUSD {
		t.Errorf("默认预算异常: %v", cfg.Repos["acme/app"].BudgetUSD)
	}
}

func TestValidateRejectsEmptyVerifyAndBadBudget(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "repos.yaml", `repos:
  acme/app:
    local_path: /tmp/app
    verify_commands: {}
    budget_usd: -1
`)
	_, err := Load(home)
	if err == nil {
		t.Fatal("应当校验失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "verify_commands 不能为空") || !strings.Contains(msg, "budget_usd 必须大于 0") {
		t.Errorf("错误信息不完整: %s", msg)
	}
}

func TestDeliveryEnabledRejectedInM0(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "config.yaml", "delivery:\n  enabled: true\n")
	if _, err := Load(home); err == nil || !strings.Contains(err.Error(), "尚未实现") {
		t.Errorf("delivery.enabled=true 应当被拒绝: %v", err)
	}
}

func TestInvalidYAMLReported(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "policy.yaml", "deny_paths: [\n")
	if _, err := Load(home); err == nil || !strings.Contains(err.Error(), "YAML") {
		t.Errorf("应当报告 YAML 错误: %v", err)
	}
}

func TestResolveHomePriority(t *testing.T) {
	t.Setenv("AGENTDEV_HOME", "/from/env")
	if got, _ := ResolveHome("/explicit"); got != "/explicit" {
		t.Errorf("显式参数优先: %s", got)
	}
	if got, _ := ResolveHome(""); got != "/from/env" {
		t.Errorf("环境变量次之: %s", got)
	}
}
