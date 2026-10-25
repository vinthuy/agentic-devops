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
	if cfg.Policy.MaxReviewAttempts != 2 || cfg.Delivery.MaxCIAttempts != 2 || cfg.Delivery.CITimeoutMinutes != 30 || cfg.Delivery.CIPollSeconds != 60 {
		t.Errorf("M2 默认值异常: policy=%+v delivery=%+v", cfg.Policy, cfg.Delivery)
	}
	if !cfg.Review.Enabled || cfg.Delivery.CIWatch {
		t.Errorf("Reviewer 默认应开启、CI watch 默认应关闭: review=%+v delivery=%+v", cfg.Review, cfg.Delivery)
	}
	if cfg.Models.APIKeyEnv != "DEEPSEEK_API_KEY" || !strings.Contains(cfg.Models.BaseURL, "deepseek") {
		t.Errorf("模型默认配置应为 DeepSeek: %+v", cfg.Models)
	}
	if cfg.DBPath() != filepath.Join(home, "agentdev.db") {
		t.Errorf("数据库路径异常: %s", cfg.DBPath())
	}
}

func TestLoadM2SettingsAndValidateCIWatch(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "config.yaml", `delivery:
  ci_watch: true
  max_ci_attempts: 4
  ci_timeout_minutes: 12
  ci_poll_seconds: 3
review:
  enabled: false
`)
	writeFile(t, home, "policy.yaml", "max_review_attempts: 5\n")
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Delivery.CIWatch || cfg.Delivery.MaxCIAttempts != 4 || cfg.Delivery.CITimeoutMinutes != 12 || cfg.Delivery.CIPollSeconds != 3 {
		t.Errorf("M2 delivery 覆盖未生效: %+v", cfg.Delivery)
	}
	if cfg.Review.Enabled || cfg.Policy.MaxReviewAttempts != 5 {
		t.Errorf("M2 review/policy 覆盖未生效: review=%+v policy=%+v", cfg.Review, cfg.Policy)
	}

	cfg.Delivery.CIPollSeconds = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "ci_poll_seconds") {
		t.Errorf("ci_watch=true 时 poll 不能为 0，实际: %v", err)
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

func TestApprovalTimeoutConfig(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "repos.yaml", "repos:\n  acme/app:\n    local_path: /tmp/app\n    verify_commands:\n      test: \"true\"\n")
	writeFile(t, home, "policy.yaml", "approval_timeout_minutes: 90\n")
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.ApprovalTimeoutMinutes != 90 {
		t.Fatalf("应读到 90，实际 %d", cfg.Policy.ApprovalTimeoutMinutes)
	}

	// 默认必须是 0：审批门默认不会自己失效。
	empty := t.TempDir()
	writeFile(t, empty, "repos.yaml", "repos:\n  acme/app:\n    local_path: /tmp/app\n    verify_commands:\n      test: \"true\"\n")
	cfg, err = Load(empty)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.ApprovalTimeoutMinutes != 0 {
		t.Errorf("默认应为 0（不超时），实际 %d", cfg.Policy.ApprovalTimeoutMinutes)
	}

	// 负数没有意义：既不是「不超时」也不是一个时长。
	bad := t.TempDir()
	writeFile(t, bad, "repos.yaml", "repos:\n  acme/app:\n    local_path: /tmp/app\n    verify_commands:\n      test: \"true\"\n")
	writeFile(t, bad, "policy.yaml", "approval_timeout_minutes: -5\n")
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "approval_timeout_minutes") {
		t.Errorf("负数应被拒绝，实际 %v", err)
	}
}

func TestDeliveryEnabledKeepsDefaults(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "config.yaml", "delivery:\n  enabled: true\n")
	cfg, err := Load(home)
	if err != nil {
		t.Fatalf("delivery.enabled=true 现在应当被接受: %v", err)
	}
	if !cfg.Delivery.Enabled {
		t.Error("delivery.enabled 应为 true")
	}
	// 未在配置中出现的字段必须保留默认值，否则分支命名会变成空前缀。
	if cfg.Delivery.BranchPrefix != "agent/" {
		t.Errorf("branch_prefix 应保留默认值，实际 %q", cfg.Delivery.BranchPrefix)
	}
	if !cfg.Delivery.DraftPR {
		t.Error("draft_pr 应默认为 true")
	}
}

func TestDeliveryRejectsEmptyBranchPrefix(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "repos.yaml", "repos:\n  acme/app:\n    local_path: /tmp/x\n    verify_commands:\n      test: echo ok\n")
	writeFile(t, home, "config.yaml", "delivery:\n  enabled: true\n  branch_prefix: \"\"\n")
	_, err := Load(home)
	if err == nil || !strings.Contains(err.Error(), "branch_prefix") {
		t.Errorf("空 branch_prefix 应当被拒绝: %v", err)
	}
}

func TestPlanningModeValidated(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "config.yaml", "planning:\n  mode: nonsense\n")
	_, err := Load(home)
	if err == nil || !strings.Contains(err.Error(), "planning.mode") {
		t.Errorf("非法 planning.mode 应当被拒绝: %v", err)
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
