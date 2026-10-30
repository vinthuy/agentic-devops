package config

import "testing"

func TestRoleModelFallsBackToDefault(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "models.yaml", `model: default-model
base_url: https://api.example.test/anthropic
api_key_env: DEMO_API_KEY
roles:
  coder: coder-x
`)
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.RoleModel("coder") != "coder-x" {
		t.Fatalf("编码应使用角色模型，实际 %s", cfg.Models.RoleModel("coder"))
	}
	for _, role := range []string{"triage", "planner", "reviewer", "other"} {
		if got := cfg.Models.RoleModel(role); got != "default-model" {
			t.Fatalf("未指定的角色 %s 应使用默认模型，实际 %s", role, got)
		}
	}
	if cfg.Models.BaseURL != "https://api.example.test/anthropic" || cfg.Models.APIKeyEnv != "DEMO_API_KEY" {
		t.Fatalf("角色模型仍应共用一个端点和密钥变量: %+v", cfg.Models)
	}
}

func TestMissingRolesUseTheDefaultModel(t *testing.T) {
	home := t.TempDir()
	writeFile(t, home, "models.yaml", "model: only-default\n")
	cfg, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.RoleModel("coder") != "only-default" || cfg.Models.RoleModel("planner") != "only-default" {
		t.Fatalf("没有 roles 时都应使用 model: %+v", cfg.Models)
	}
}
