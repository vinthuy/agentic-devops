// Package bench 提供可复现的基准集运行器。
//
// 目标（PRD §10）：用一组固定的 S 级任务测量端到端成功率、人工介入率、修复轮数与成本，
// 而不是用零散的临时仓库“感觉能跑”。每个用例都在独立的临时目录中从 fixture 重建
// git 仓库，因此结果不受上一次运行的残留影响。
package bench

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"agentdev/internal/backend"
	"agentdev/internal/config"
	"agentdev/internal/cost"
	"agentdev/internal/llm"
	"agentdev/internal/orchestrator"
	"agentdev/internal/report"
	"agentdev/internal/review"
	"agentdev/internal/state"
	"agentdev/internal/store"
	"agentdev/internal/workspace"
)

// Case 是基准集中的一个任务。
type Case struct {
	ID string `yaml:"id"`
	// Fixture 是相对 manifest 所在目录的模板目录。运行时会被复制并重新 git init。
	Fixture string `yaml:"fixture"`
	// Requirement 是提交给流水线的自然语言需求。
	Requirement string `yaml:"requirement"`
	// Verify 是仓库的验证命令（名称 -> 命令），按声明顺序执行。
	Verify map[string]string `yaml:"verify"`
	// VerifyConfigFiles 是防作弊保护的文件（可选）。
	VerifyConfigFiles []string `yaml:"verify_config_files"`
	BudgetUSD         float64  `yaml:"budget_usd"`
	// ExpectLevel 非空时校验分级结果（S/M/L）。
	ExpectLevel string `yaml:"expect_level"`
}

// Manifest 是基准集清单。
type Manifest struct {
	Cases []Case `yaml:"cases"`
}

// Outcome 是单个用例单次运行的结果。
type Outcome struct {
	ID string `json:"id"`
	// Run 是第几次重复运行（从 1 开始）；未启用重复采样时恒为 1。
	Run            int     `json:"run"`
	Status         string  `json:"status"`
	Level          string  `json:"level"`
	ExpectLevel    string  `json:"expect_level,omitempty"`
	LevelMatched   bool    `json:"level_matched"`
	Passed         bool    `json:"passed"`
	SpentUSD       float64 `json:"spent_usd"`
	FixAttempts    int     `json:"fix_attempts"`
	ReviewAttempts int     `json:"review_attempts"`
	CIAttempts     int     `json:"ci_attempts"`
	Degradations   int     `json:"degradations"`
	DurationS      float64 `json:"duration_s"`
	// CIVerdict 是最终 CI 判定：passed / failed / none / unknown（空表示未启用真实 CI）。
	CIVerdict string `json:"ci_verdict,omitempty"`
	// CIFirstPass 为 true 表示 PR 的第一次 CI 就通过，中途没有回修。
	CIFirstPass bool `json:"ci_first_pass,omitempty"`
	// BaseBranch 是启用 --ci-repo 时该用例独占的 PR base 分支。
	BaseBranch string `json:"base_branch,omitempty"`
	// PRURL 是交付产生的草稿 PR（启用 --ci-repo 时有值）。
	PRURL string `json:"pr_url,omitempty"`
	// Blocked 表示运行被基础设施问题阻断（如模型余额不足），
	// 与产品能力无关，不应计入成功率分母。
	Blocked bool   `json:"blocked,omitempty"`
	Err     string `json:"error,omitempty"`
}

// Summary 是基准集的聚合指标。启用 --repeat 时，每个用例会贡献多次 trial，
// 因此 SuccessRate 是“运行次数”维度的比例，可用来观察模型波动。
type Summary struct {
	Trials int `json:"trials"`
	Total  int `json:"total"`
	Passed int `json:"passed"`
	// Blocked 是因基础设施（余额/配额）未能完成的运行次数。
	Blocked       int     `json:"blocked"`
	SuccessRate   float64 `json:"success_rate"`
	HumanRate     float64 `json:"human_rate"`
	MeanCostUSD   float64 `json:"mean_cost_usd"`
	MeanFixRounds float64 `json:"mean_fix_rounds"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
	// CIRuns 是真正跑过检查的交付次数；CI 首次通过率的分母。
	CIRuns int `json:"ci_runs"`
	// CIFirstPass 是第一次 CI 就通过（未回修）的次数。
	CIFirstPass int `json:"ci_first_pass"`
	// CIFirstPassRate 即 PRD 的 CI 首次通过率；CIRuns 为 0 时无意义（保持 0）。
	CIFirstPassRate float64 `json:"ci_first_pass_rate"`
	// CINone 是 PR 上没有检查的次数；按设计不计入分母，但必须显式披露。
	CINone int `json:"ci_none"`
}

// ModelConfig 非 nil 时启用模型分级、规划与审查，并记录费用。
type ModelConfig struct {
	Client    *llm.Client
	Model     string
	MaxTokens int
	Pricing   cost.Table
}

// Options 控制一次基准运行。
type Options struct {
	// BackendFor 为每个用例返回一个全新的执行后端（Fake 脚本不应跨用例泄漏）。
	BackendFor func(Case) (backend.ExecutionBackend, error)
	Model      *ModelConfig
	// Reviewer 覆盖默认 Reviewer（测试用）。
	Reviewer review.Reviewer
	// WorkDir 是临时目录根；为空时使用系统临时目录。每个用例在起下面独占一个子目录。
	WorkDir string
	// Repeat 是每个用例的重复运行次数（默认 1）。重复采样用于区分模型波动与真实能力。
	Repeat int
	// Keep 为 true 时保留临时目录，便于排查失败用例。
	Keep bool
	// CI 非 nil 时，每个用例都会推送到真实远端并开启 delivery + ci_watch，
	// 用真实 CI 的结果统计首次通过率。为 nil 时完全离线（旧行为）。
	CI *CIRemote
	// Log 接收逐用例进度输出，可为 nil。
	Log io.Writer
}

// Load 读取并校验 manifest。
func Load(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("解析基准清单失败: %w", err)
	}
	if len(m.Cases) == 0 {
		return Manifest{}, fmt.Errorf("基准清单为空: %s", path)
	}
	seen := map[string]bool{}
	for i, c := range m.Cases {
		if c.ID == "" {
			return Manifest{}, fmt.Errorf("第 %d 个用例缺少 id", i+1)
		}
		if seen[c.ID] {
			return Manifest{}, fmt.Errorf("用例 id 重复: %s", c.ID)
		}
		seen[c.ID] = true
		if c.Fixture == "" {
			return Manifest{}, fmt.Errorf("%s: 缺少 fixture", c.ID)
		}
		if strings.TrimSpace(c.Requirement) == "" {
			return Manifest{}, fmt.Errorf("%s: 缺少 requirement", c.ID)
		}
		if len(c.Verify) == 0 {
			return Manifest{}, fmt.Errorf("%s: 缺少 verify 命令", c.ID)
		}
	}
	return m, nil
}

// Run 读取清单并运行全部用例。
func Run(ctx context.Context, manifestPath string, opts Options) (Summary, []Outcome, error) {
	manifest, err := Load(manifestPath)
	if err != nil {
		return Summary{}, nil, err
	}
	return RunManifest(ctx, filepath.Dir(manifestPath), manifest, opts)
}

// RunManifest 运行给定的清单。manifestDir 用于解析 fixture 相对路径。
func RunManifest(ctx context.Context, manifestDir string, manifest Manifest, opts Options) (Summary, []Outcome, error) {
	baseDir := opts.WorkDir
	var err error
	if baseDir == "" {
		baseDir, err = os.MkdirTemp("", "agentdev-bench-*")
		if err != nil {
			return Summary{}, nil, err
		}
	} else if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return Summary{}, nil, err
	}
	if !opts.Keep && opts.WorkDir == "" {
		defer os.RemoveAll(baseDir)
	}

	repeat := opts.Repeat
	if repeat <= 0 {
		repeat = 1
	}
	var outcomes []Outcome
	for run := 1; run <= repeat; run++ {
		for _, c := range manifest.Cases {
			outcome := runCase(ctx, manifestDir, baseDir, c, run, opts)
			outcomes = append(outcomes, outcome)
			if opts.Log != nil {
				suffix := ""
				if repeat > 1 {
					suffix = fmt.Sprintf(" (run %d/%d)", run, repeat)
				}
				fmt.Fprintf(opts.Log, "[%s] %s%s 级别=%s 花费=$%.4f 时长=%.1fs\n",
					outcome.Status, outcome.ID, suffix, orDash(outcome.Level), outcome.SpentUSD, outcome.DurationS)
				if outcome.Err != "" {
					fmt.Fprintf(opts.Log, "  错误: %s\n", outcome.Err)
				}
			}
		}
	}
	return summarize(outcomes, len(manifest.Cases)), outcomes, nil
}

func runCase(ctx context.Context, manifestDir, baseDir string, c Case, run int, opts Options) Outcome {
	started := time.Now()
	outcome := Outcome{ID: c.ID, Run: run, ExpectLevel: c.ExpectLevel}

	dirName := c.ID
	if run > 1 {
		dirName = fmt.Sprintf("%s-run%d", c.ID, run)
	}
	work := filepath.Join(baseDir, dirName)
	if err := os.MkdirAll(work, 0o755); err != nil {
		outcome.Err = err.Error()
		outcome.Status = "ERROR"
		return outcome
	}
	repoDir := filepath.Join(work, "repo")
	if err := copyFixture(filepath.Join(manifestDir, c.Fixture), repoDir); err != nil {
		outcome.Err = "复制 fixture 失败: " + err.Error()
		outcome.Status = "ERROR"
		return outcome
	}
	var ci CIRemote
	if opts.CI != nil {
		ci = opts.CI.withDefaults()
		if err := writeCIWorkflow(repoDir, ci); err != nil {
			outcome.Err = "写入 CI 定义失败: " + err.Error()
			outcome.Status = "ERROR"
			return outcome
		}
	}
	if err := initRepo(repoDir); err != nil {
		outcome.Err = "初始化仓库失败: " + err.Error()
		outcome.Status = "ERROR"
		return outcome
	}
	// defaultBranch 默认是 main；启用真实 CI 时改为该用例独占的基线分支。
	defaultBranch := "main"
	headBranch := ""
	if opts.CI != nil {
		base, err := pushBaseline(repoDir, c.ID, run, ci)
		if err != nil {
			outcome.Err = "准备远端基线失败: " + err.Error()
			outcome.Status = "ERROR"
			return outcome
		}
		defaultBranch = base
		outcome.BaseBranch = base
	}

	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		outcome.Err = err.Error()
		outcome.Status = "ERROR"
		return outcome
	}
	cfg, err := config.Load(home)
	if err != nil {
		outcome.Err = "加载配置失败: " + err.Error()
		outcome.Status = "ERROR"
		return outcome
	}
	budget := c.BudgetUSD
	if budget <= 0 {
		budget = 2
	}
	cfg.Repos = map[string]config.RepoConfig{
		"bench/" + c.ID: {
			LocalPath:         repoDir,
			DefaultBranch:     defaultBranch,
			Remote:            ci.RemoteName,
			VerifyCommands:    orderedVerify(c.Verify),
			VerifyConfigFiles: c.VerifyConfigFiles,
			BudgetUSD:         budget,
		},
	}
	if opts.CI != nil {
		cfg.Delivery.Enabled = true
		cfg.Delivery.DraftPR = true
		cfg.Delivery.CIWatch = true
		cfg.Delivery.MaxCIAttempts = 2
		cfg.Delivery.CITimeoutMinutes = ci.TimeoutMinutes
		cfg.Delivery.CIPollSeconds = ci.PollSeconds
	} else {
		cfg.Delivery.Enabled = false
	}
	cfg.Review.Enabled = opts.Model != nil || opts.Reviewer != nil
	cfg.Triage.UseModel = opts.Model != nil
	cfg.Planning.Mode = "auto"

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		outcome.Err = "打开数据库失败: " + err.Error()
		outcome.Status = "ERROR"
		return outcome
	}
	defer st.Close()

	task, err := st.CreateTask(store.NewTask{
		Repo: "bench/" + c.ID, Requirement: c.Requirement, BudgetUSD: budget,
	})
	if err != nil {
		outcome.Err = "创建任务失败: " + err.Error()
		outcome.Status = "ERROR"
		return outcome
	}

	be, err := opts.BackendFor(c)
	if err != nil {
		outcome.Err = "构造后端失败: " + err.Error()
		outcome.Status = "ERROR"
		return outcome
	}
	orchOpts := []orchestrator.Option{}
	if opts.Model != nil {
		orchOpts = append(orchOpts, orchestrator.WithModel(
			opts.Model.Client, opts.Model.Model, opts.Model.MaxTokens, opts.Model.Pricing))
	}
	if opts.Reviewer != nil {
		orchOpts = append(orchOpts, orchestrator.WithReviewer(opts.Reviewer))
	}
	orch := orchestrator.New(st, cfg, be, orchOpts...)

	result, runErr := orch.RunTask(task.ID)
	headBranch = result.Branch
	outcome.DurationS = time.Since(started).Seconds()
	if runErr != nil {
		outcome.Err = runErr.Error()
	}
	outcome.Status = string(result.Status)
	if outcome.Status == "" {
		outcome.Status = "ERROR"
	}
	outcome.Level = result.Level
	outcome.SpentUSD = result.SpentUSD
	outcome.FixAttempts = result.FixAttempts
	outcome.ReviewAttempts = result.ReviewAttempts
	outcome.CIAttempts = result.CIAttempts
	outcome.PRURL = result.PRURL
	outcome.LevelMatched = c.ExpectLevel == "" || strings.EqualFold(result.Level, c.ExpectLevel)
	outcome.Passed = result.Status == state.Done && runErr == nil && outcome.LevelMatched

	if events, err := st.ListEvents(task.ID); err == nil {
		outcome.Degradations = len(report.Degradations(events))
		for _, e := range events {
			if e.Type == "provider_quota" {
				outcome.Blocked = true
			}
		}
		if opts.CI != nil {
			outcome.CIVerdict = ciVerdict(events)
			// “首次即通过”要求真的跑过检查，且全程没有因为 CI 失败回过炉。
			outcome.CIFirstPass = outcome.CIVerdict == "passed" && result.CIAttempts == 0
		}
	}

	if opts.CI != nil && ci.Cleanup {
		cleanupCase(ctx, repoDir, headBranch, outcome.BaseBranch, result.PRNumber, ci, opts.Log)
	}
	return outcome
}

// ciVerdict 从事件流里取本任务最终的 CI 判定。
//
// 不能只看任务状态：DONE 既可能是“CI 通过”，也可能是“没有 CI”。
// 两者在交付质量上是完全不同的结论，混在一起会把没跑过的检查算成通过。
// 同理，CI 失败后挂起（NEEDS_HUMAN）没有 DONE 事件，但它确实跑过 CI 且失败，
// 必须计入分母，不能退化成“未知”被悄悄排除。
func ciVerdict(events []store.Event) string {
	verdict := ""
	sawCIFailure := false
	for _, e := range events {
		if e.Type == "ci_failed" {
			sawCIFailure = true
			continue
		}
		if e.Type != "state_changed" || e.ToStatus != string(state.Done) {
			continue
		}
		if v, ok := e.Payload["ci"].(string); ok && v != "" {
			verdict = v
		}
	}
	switch verdict {
	case "passed", "failed", "none":
		return verdict
	case "":
		if sawCIFailure {
			return "failed"
		}
		return "unknown"
	default:
		// skipped 等其它取值同样不构成“跑过 CI”。
		if sawCIFailure {
			return "failed"
		}
		return "unknown"
	}
}

func summarize(outcomes []Outcome, caseCount int) Summary {
	var s Summary
	s.Total = caseCount
	s.Trials = len(outcomes)
	if len(outcomes) == 0 {
		return s
	}
	var (
		fixRounds     int
		executedSpend float64
	)
	for _, o := range outcomes {
		// 总花费按实际发生额统计：即使运行被阻断，钱也已经花了。
		s.TotalCostUSD += o.SpentUSD
		if o.Blocked {
			s.Blocked++
			continue
		}
		switch o.CIVerdict {
		case "passed", "failed":
			// 只有真的跑过检查才进分母：passed 且未回修才计为首次通过。
			s.CIRuns++
			if o.CIFirstPass {
				s.CIFirstPass++
			}
		case "none":
			// 没有 CI 是独立结论，既不算通过也不算失败，但必须被看见。
			s.CINone++
		}
		executedSpend += o.SpentUSD
		if o.Passed {
			s.Passed++
		}
		if o.Status == string(state.NeedsHuman) || o.Status == "ERROR" {
			s.HumanRate++
		}
		fixRounds += o.FixAttempts
	}
	// 分母只算真正执行过的运行：基础设施阻断不代表产品能力。
	executed := float64(len(outcomes) - s.Blocked)
	if executed == 0 {
		return s
	}
	s.SuccessRate = float64(s.Passed) / executed
	s.HumanRate = s.HumanRate / executed
	s.MeanCostUSD = executedSpend / executed
	s.MeanFixRounds = float64(fixRounds) / executed
	if s.CIRuns > 0 {
		s.CIFirstPassRate = float64(s.CIFirstPass) / float64(s.CIRuns)
	}
	return s
}

// CaseStat 按用例聚合多次运行，用于观察波动。
type CaseStat struct {
	ID      string `json:"id"`
	Runs    int    `json:"runs"`
	Passed  int    `json:"passed"`
	Blocked int    `json:"blocked,omitempty"`
}

// ByCase 返回每个用例的通过次数，便于在重复采样下识别不稳定用例。
func ByCase(outcomes []Outcome) []CaseStat {
	order := []string{}
	index := map[string]*CaseStat{}
	for _, o := range outcomes {
		stat, ok := index[o.ID]
		if !ok {
			index[o.ID] = &CaseStat{ID: o.ID}
			order = append(order, o.ID)
			stat = index[o.ID]
		}
		if o.Blocked {
			stat.Blocked++
			continue
		}
		stat.Runs++
		if o.Passed {
			stat.Passed++
		}
	}
	out := make([]CaseStat, 0, len(order))
	for _, id := range order {
		out = append(out, *index[id])
	}
	return out
}

// orderedVerify 保留 YAML 声明顺序。映射在解析后是无序的，因此这里按名称排序，
// 保证同一清单每次执行顺序一致（验证顺序影响首次失败的定位）。
func orderedVerify(m map[string]string) config.VerifyCommands {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make(config.VerifyCommands, 0, len(names))
	for _, name := range names {
		out = append(out, config.VerifyCommand{Name: name, Command: m[name]})
	}
	return out
}

func copyFixture(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是目录", src)
	}
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		// fixture 里即使误带了 .git 也不复制：每次都要从干净历史重建。
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func initRepo(dir string) error {
	if _, err := workspace.Git(dir, "init", "-b", "main"); err != nil {
		return err
	}
	if _, err := workspace.Git(dir, "add", "-A"); err != nil {
		return err
	}
	_, err := workspace.Git(dir, "-c", "user.name=bench", "-c", "user.email=bench@localhost",
		"commit", "-m", "bench fixture baseline")
	return err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
