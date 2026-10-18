package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultBudgetUSD 是未配置 budget_usd 时的单任务预算上限。
const DefaultBudgetUSD = 2.0

var DefaultDenyPaths = []string{".env*", "**/.env*", "**/*.pem", "**/*.key", ".git/**", "**/secrets/**"}

var DefaultDenyCommandPatterns = []string{
	`rm\s+-rf`,
	`curl\s+.*\|\s*(sh|bash)`,
	`git\s+(push|config|remote)`,
	`npm\s+publish`,
	`\bsudo\b`,
}

// VerifyCommand 保留 YAML 中的声明顺序，验证按此顺序执行。
type VerifyCommand struct {
	Name    string
	Command string
}

type VerifyCommands []VerifyCommand

func (v *VerifyCommands) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("verify_commands 必须是映射")
	}
	out := make(VerifyCommands, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		out = append(out, VerifyCommand{Name: node.Content[i].Value, Command: node.Content[i+1].Value})
	}
	*v = out
	return nil
}

type RepoConfig struct {
	LocalPath         string         `yaml:"local_path"`
	DefaultBranch     string         `yaml:"default_branch"`
	VerifyCommands    VerifyCommands `yaml:"verify_commands"`
	AllowedCommands   []string       `yaml:"allowed_commands"`
	SensitivePaths    []string       `yaml:"sensitive_paths"`
	VerifyConfigFiles []string       `yaml:"verify_config_files"`
	BudgetUSD         float64        `yaml:"budget_usd"`
}

type PolicyConfig struct {
	DenyPaths           []string `yaml:"deny_paths"`
	DenyCommandPatterns []string `yaml:"deny_command_patterns"`
	MaxFixAttempts      int      `yaml:"max_fix_attempts"`
	MaxTurnsPerStep     int      `yaml:"max_turns_per_step"`
	ToolOutputMaxChars  int      `yaml:"tool_output_max_chars"`
	CommandTimeoutS     int      `yaml:"command_timeout_s"`
	TaskTimeoutMinutes  int      `yaml:"task_timeout_minutes"`
}

type ModelsConfig struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
}

type DeliveryConfig struct {
	Enabled      bool   `yaml:"enabled"`
	BranchPrefix string `yaml:"branch_prefix"`
}

// App 是加载后的全部配置。
type App struct {
	Home           string
	Repos          map[string]RepoConfig
	Policy         PolicyConfig
	Models         ModelsConfig
	Delivery       DeliveryConfig
	GitAuthorName  string
	GitAuthorEmail string
}

func (a *App) DBPath() string        { return filepath.Join(a.Home, "agentdev.db") }
func (a *App) WorkspacesDir() string { return filepath.Join(a.Home, "workspaces") }
func (a *App) RunsDir() string       { return filepath.Join(a.Home, "runs") }

// ResolveHome 优先使用显式参数，其次 AGENTDEV_HOME，最后 ~/.agentdev。
func ResolveHome(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if env := os.Getenv("AGENTDEV_HOME"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agentdev"), nil
}

func defaults(home string) *App {
	return &App{
		Home:  home,
		Repos: map[string]RepoConfig{},
		Policy: PolicyConfig{
			DenyPaths:           append([]string(nil), DefaultDenyPaths...),
			DenyCommandPatterns: append([]string(nil), DefaultDenyCommandPatterns...),
			MaxFixAttempts:      3,
			MaxTurnsPerStep:     30,
			ToolOutputMaxChars:  8000,
			CommandTimeoutS:     300,
			TaskTimeoutMinutes:  45,
		},
		Models: ModelsConfig{
			Provider:  "anthropic",
			Model:     "deepseek-v4-pro",
			MaxTokens: 8192,
			BaseURL:   "https://api.deepseek.com/anthropic",
			APIKeyEnv: "DEEPSEEK_API_KEY",
		},
		Delivery:       DeliveryConfig{Enabled: false, BranchPrefix: "agent/"},
		GitAuthorName:  "agentdev",
		GitAuthorEmail: "agentdev@localhost",
	}
}

// Load 读取 home 下的 repos.yaml、policy.yaml、models.yaml、config.yaml，缺失时使用默认值。
func Load(home string) (*App, error) {
	app := defaults(home)

	var repos struct {
		Repos map[string]RepoConfig `yaml:"repos"`
	}
	if err := readYAML(filepath.Join(home, "repos.yaml"), &repos); err != nil {
		return nil, err
	}
	for name, repo := range repos.Repos {
		if repo.DefaultBranch == "" {
			repo.DefaultBranch = "main"
		}
		if repo.BudgetUSD == 0 {
			repo.BudgetUSD = DefaultBudgetUSD
		}
		repos.Repos[name] = repo
	}
	if repos.Repos != nil {
		app.Repos = repos.Repos
	}
	if err := readYAML(filepath.Join(home, "policy.yaml"), &app.Policy); err != nil {
		return nil, err
	}
	if err := readYAML(filepath.Join(home, "models.yaml"), &app.Models); err != nil {
		return nil, err
	}

	var top struct {
		Delivery       *DeliveryConfig `yaml:"delivery"`
		GitAuthorName  string          `yaml:"git_author_name"`
		GitAuthorEmail string          `yaml:"git_author_email"`
	}
	if err := readYAML(filepath.Join(home, "config.yaml"), &top); err != nil {
		return nil, err
	}
	if top.Delivery != nil {
		app.Delivery = *top.Delivery
	}
	if top.GitAuthorName != "" {
		app.GitAuthorName = top.GitAuthorName
	}
	if top.GitAuthorEmail != "" {
		app.GitAuthorEmail = top.GitAuthorEmail
	}

	if err := app.Validate(); err != nil {
		return nil, err
	}
	return app, nil
}

func readYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: YAML 解析失败: %w", path, err)
	}
	return nil
}

// Validate 检查字段约束，返回所有问题的汇总。
func (a *App) Validate() error {
	var problems []string
	for name, repo := range a.Repos {
		if len(repo.VerifyCommands) == 0 {
			problems = append(problems, fmt.Sprintf("%s: verify_commands 不能为空", name))
		}
		if repo.BudgetUSD <= 0 {
			problems = append(problems, fmt.Sprintf("%s: budget_usd 必须大于 0", name))
		}
		if repo.LocalPath == "" {
			problems = append(problems, fmt.Sprintf("%s: local_path 不能为空", name))
		}
	}
	if a.Policy.MaxFixAttempts < 0 {
		problems = append(problems, "max_fix_attempts 不能为负数")
	}
	if a.Delivery.Enabled {
		problems = append(problems, "delivery.enabled=true 尚未实现，请保持为 false")
	}
	if len(problems) > 0 {
		return fmt.Errorf("配置校验失败:\n- %s", strings.Join(problems, "\n- "))
	}
	return nil
}

// CheckRepoPaths 返回仓库路径相关的问题，空切片表示通过。
func CheckRepoPaths(a *App) []string {
	var problems []string
	for name, repo := range a.Repos {
		if _, err := os.Stat(repo.LocalPath); err != nil {
			problems = append(problems, fmt.Sprintf("%s: local_path 不存在: %s", name, repo.LocalPath))
			continue
		}
		cmd := exec.Command("git", "-C", repo.LocalPath, "rev-parse", "--is-inside-work-tree")
		if err := cmd.Run(); err != nil {
			problems = append(problems, fmt.Sprintf("%s: local_path 不是 git 仓库: %s", name, repo.LocalPath))
		}
	}
	return problems
}
