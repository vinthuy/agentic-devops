package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentdev/internal/backend"
	"agentdev/internal/config"
	"agentdev/internal/cost"
	"agentdev/internal/delivery"
	"agentdev/internal/llm"
	"agentdev/internal/planning"
	"agentdev/internal/policy"
	"agentdev/internal/report"
	"agentdev/internal/state"
	"agentdev/internal/store"
	"agentdev/internal/tools"
	"agentdev/internal/triage"
	"agentdev/internal/verify"
	"agentdev/internal/workspace"
)

// Orchestrator 驱动单个任务在状态机中前进，直到终态或挂起（SDD 4 / 5）。
// 它是一个状态机驱动器：每一步只推进一次转换，因此中断后可从当前状态继续（resume）。
type Orchestrator struct {
	store   store.Repository
	cfg     *config.App
	backend backend.ExecutionBackend
	ws      *workspace.Manager

	// 模型相关（可选）。为 nil 时退化为纯规则 + 静态计划，可完全离线运行。
	model      *llm.Client
	modelName  string
	maxTokens  int
	pricing    cost.Table
	planner    planning.Planner
	codeHost   delivery.CodeHost
	forceDelev bool

	warnedPricing bool
	warnedBudget  bool
}

// Option 用于注入可选依赖。
type Option func(*Orchestrator)

// WithModel 注入模型客户端与单价表，启用模型分级与模型规划。
func WithModel(client *llm.Client, model string, maxTokens int, pricing cost.Table) Option {
	return func(o *Orchestrator) {
		o.model = client
		o.modelName = model
		o.maxTokens = maxTokens
		o.pricing = pricing
	}
}

// WithPlanner 覆盖默认的规划器（测试用）。
func WithPlanner(p planning.Planner) Option {
	return func(o *Orchestrator) { o.planner = p }
}

// WithCodeHost 覆盖默认的 PR 平台实现（测试用）。
func WithCodeHost(h delivery.CodeHost) Option {
	return func(o *Orchestrator) { o.codeHost = h }
}

// WithDelivery 强制开启推送与创建 PR，用于覆盖 delivery.enabled。
func WithDelivery(enabled bool) Option {
	return func(o *Orchestrator) { o.forceDelev = enabled }
}

func New(repo store.Repository, cfg *config.App, be backend.ExecutionBackend, opts ...Option) *Orchestrator {
	o := &Orchestrator{store: repo, cfg: cfg, backend: be, ws: workspace.New(cfg)}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// RunTask 推进任务至终态或挂起。返回错误仅表示存储或前置条件问题；
// 执行过程中的业务失败会体现为任务状态（NEEDS_HUMAN / FAILED）。
func (o *Orchestrator) RunTask(taskID string) (store.Task, error) {
	task, err := o.store.GetTask(taskID)
	if err != nil {
		return store.Task{}, err
	}
	if !runnableStatus(task.Status) {
		return task, fmt.Errorf("任务 %s 当前状态为 %s，无法推进", taskID, task.Status)
	}
	repo, ok := o.cfg.Repos[task.Repo]
	if !ok {
		return o.fail(task, "仓库未在 repos.yaml 中配置: "+task.Repo)
	}

	task, err = o.drive(task, repo)
	if err != nil {
		latest, gerr := o.store.GetTask(taskID)
		if gerr != nil {
			return store.Task{}, gerr
		}
		if task, err = o.fail(latest, err.Error()); err != nil {
			return task, err
		}
	}

	if _, rerr := report.WriteRunArtifacts(o.store, o.cfg, taskID); rerr != nil {
		return task, fmt.Errorf("写入运行报告失败: %w", rerr)
	}
	return task, nil
}

func runnableStatus(s state.Status) bool {
	for _, r := range store.RunnableStatuses {
		if r == s {
			return true
		}
	}
	return false
}

// drive 是状态机主循环：按当前状态执行一步，直到进入终态或挂起态。
func (o *Orchestrator) drive(task store.Task, repo config.RepoConfig) (store.Task, error) {
	for {
		if state.Terminal[task.Status] || state.Suspended[task.Status] {
			return task, nil
		}
		if exceeded, msg := o.timedOut(task); exceeded {
			return o.move(task, state.NeedsHuman, map[string]any{"reason": msg}, nil)
		}

		var err error
		switch task.Status {
		case state.Queued, state.Triaging:
			task, err = o.triageStep(task, repo)
		case state.Planning:
			task, err = o.planStep(task, repo)
		case state.Coding:
			task, err = o.codeStep(task, repo)
		case state.Verifying:
			task, err = o.verifyStep(task, repo)
		case state.Committing:
			task, err = o.deliverStep(task, repo)
		case state.Pushed:
			task, err = o.prStep(task, repo)
		case state.PROpened:
			task, err = o.move(task, state.Done, map[string]any{"reason": "PR 已创建"}, map[string]any{"finished_at": now()})
		default:
			return task, fmt.Errorf("状态 %s 没有对应的推进步骤", task.Status)
		}
		if err != nil {
			return task, err
		}
	}
}

func (o *Orchestrator) timedOut(task store.Task) (bool, string) {
	limit := o.cfg.Policy.TaskTimeoutMinutes
	if limit <= 0 || task.CreatedAt == "" {
		return false, ""
	}
	start, err := time.Parse(time.RFC3339, task.CreatedAt)
	if err != nil {
		return false, ""
	}
	if time.Since(start) > time.Duration(limit)*time.Minute {
		return true, fmt.Sprintf("任务已超过 %d 分钟上限", limit)
	}
	return false, ""
}

// triageStep 分级：用户显式指定 > 规则 > 模型估算（取更保守者）。
func (o *Orchestrator) triageStep(task store.Task, repo config.RepoConfig) (store.Task, error) {
	task, err := o.move(task, state.Triaging, nil, nil)
	if err != nil {
		return task, err
	}

	var (
		result triage.Result
		source = "rules"
	)
	switch {
	case task.LevelOverride && task.Level != "":
		lvl, _, perr := triage.Parse(task.Level)
		if perr != nil {
			return task, perr
		}
		result = triage.Result{Level: lvl, Reasons: []string{"用户通过 --level 显式指定"}}
		source = "user_override"
	default:
		result = triage.Rules(task.Requirement, repo)
		if o.cfg.Triage.UseModel && o.model != nil && result.Level < triage.L {
			est, eerr := (&triage.ModelEstimator{
				Client: o.model, Model: o.modelName, MaxTokens: o.maxTokens,
			}).Estimate(context.Background(), task.Requirement, repo)
			if eerr != nil {
				// 分级失败不应阻断任务：记录后按规则结果继续。
				if aerr := o.store.AppendEvent(task.ID, "triage_model_failed",
					map[string]any{"error": eerr.Error()}); aerr != nil {
					return task, aerr
				}
			} else {
				if uerr := o.recordUsage(task.ID, "triage", o.modelName, est.Usage); uerr != nil {
					return task, uerr
				}
				result = triage.Combine(result, est)
				source = "rules+model"
			}
		}
	}
	if err := o.store.AppendEvent(task.ID, "triage_ready", map[string]any{
		"source": source, "model_used": result.ModelUsed, "level": result.Level.String(),
	}); err != nil {
		return task, err
	}

	payload := map[string]any{
		"level":      result.Level.String(),
		"reasons":    result.Reasons,
		"model_used": result.ModelUsed,
	}
	if result.Level == triage.L {
		return o.move(task, state.Rejected, mergePayload(payload, map[string]any{
			"reason": "L 级任务不自动执行，请拆分为 S/M 级子任务",
		}), map[string]any{"level": result.Level.String()})
	}
	task, err = o.move(task, state.Planning, payload,
		map[string]any{"level": result.Level.String()})
	if err != nil {
		return task, err
	}
	if err := o.checkBudget(task.ID); err != nil {
		return o.suspend(task, "分级后预算已用尽")
	}
	return task, nil
}

// planStep 生成结构化计划。需求不清时进入 NEEDS_INFO；M 级任务进入 AWAITING_APPROVAL。
func (o *Orchestrator) planStep(task store.Task, repo config.RepoConfig) (store.Task, error) {
	req := planning.Requirement{
		TaskID:      task.ID,
		Text:        task.Requirement,
		VerifyNames: verifyNames(repo),
		Commands:    commandMap(repo),
		RepoSummary: repoSummary(repo.LocalPath),
	}

	planner := o.planner
	planSource := "configured_planner"
	if planner == nil && o.cfg.Planning.Mode == "auto" && o.model != nil {
		planner = &planning.ModelPlanner{
			Client: o.model, Model: o.modelName, MaxTokens: o.maxTokens,
			OnUsage: func(role string, u llm.Usage) {
				_ = o.recordUsage(task.ID, role, o.modelName, u)
			},
		}
		planSource = "model"
	} else if planner == nil {
		planSource = "static"
	}

	plan, err := planWith(planner, req)
	if err != nil {
		if aerr := o.store.AppendEvent(task.ID, "plan_failed", map[string]any{
			"error":  err.Error(),
			"reason": "模型规划失败，降级为静态计划",
		}); aerr != nil {
			return task, aerr
		}
		plan, err = planning.StaticPlanner{}.Plan(context.Background(), req)
		planSource = "static_fallback"
		if err != nil {
			return task, err
		}
	} else if planner == nil {
		planSource = "static"
	}
	if err := o.store.AppendEvent(task.ID, "plan_ready", map[string]any{"source": planSource}); err != nil {
		return task, err
	}
	if o.model != nil {
		if err := o.checkBudget(task.ID); err != nil {
			return o.suspend(task, "规划后预算已用尽")
		}
	}

	planJSON, err := json.Marshal(plan)
	if err != nil {
		return task, err
	}

	if plan.NeedsInfo() {
		return o.move(task, state.NeedsInfo,
			map[string]any{"questions": plan.Questions, "summary": plan.Summary},
			map[string]any{"plan_json": string(planJSON)})
	}

	info, err := o.ensureWorktree(task, repo)
	if err != nil {
		return task, err
	}
	fields := map[string]any{
		"plan_json":     string(planJSON),
		"branch":        info.Branch,
		"base_sha":      info.BaseSHA,
		"worktree_path": filepath.ToSlash(info.Path),
	}
	payload := map[string]any{
		"summary":    plan.Summary,
		"steps":      len(plan.Steps),
		"acceptance": len(plan.Acceptance),
		"scope":      plan.Scope.AllowedPaths,
	}
	if len(plan.Scope.AllowedPaths) == 0 {
		if aerr := o.store.AppendEvent(task.ID, "plan_scope_unrestricted",
			map[string]any{"note": "计划未限定 allowed_paths，写入范围仅受全局策略限制"}); aerr != nil {
			return task, aerr
		}
	}

	if task.Level == triage.M.String() {
		return o.move(task, state.AwaitingApproval,
			mergePayload(payload, map[string]any{"reason": "M 级任务需要人工审批后再执行"}), fields)
	}
	return o.move(task, state.Coding, payload, fields)
}

func planWith(p planning.Planner, req planning.Requirement) (planning.Plan, error) {
	if p == nil {
		return planning.StaticPlanner{}.Plan(context.Background(), req)
	}
	plan, err := p.Plan(context.Background(), req)
	if err != nil {
		return planning.Plan{}, err
	}
	if verr := planning.Validate(plan, nameSet(req.VerifyNames)); verr != nil {
		return planning.Plan{}, verr
	}
	return plan, nil
}

// codeStep 执行一次编码步骤。模型错误与预算超限都挂起等待人工处理。
func (o *Orchestrator) codeStep(task store.Task, repo config.RepoConfig) (store.Task, error) {
	info := workspace.Info{Path: task.WorktreePath, Branch: task.Branch, BaseSHA: task.BaseSHA}
	if info.Path == "" {
		return o.suspend(task, "任务缺少工作区信息，无法继续编码")
	}
	plan, _ := planning.Parse(task.PlanJSON)

	pol, err := policy.NewEngine(o.cfg.Policy, repo, info.Path, plan.Scope.AllowedPaths)
	if err != nil {
		return task, err
	}
	rt := tools.New(pol, o.cfg.Policy.ToolOutputMaxChars, o.cfg.Policy.CommandTimeoutS, o.violationRecorder(task.ID))

	res, err := o.backend.RunStep(backend.StepContext{
		TaskID:         task.ID,
		Requirement:    task.Requirement,
		Plan:           plan.PromptText(),
		Worktree:       info.Path,
		Tools:          rt,
		MaxTurns:       o.cfg.Policy.MaxTurnsPerStep,
		FailureSummary: failureOf(task, o.store),
		OnUsage:        o.usageHook(task.ID, "coder"),
	})
	if err != nil {
		switch {
		case errors.Is(err, cost.ErrBudgetExceeded):
			return o.suspend(task, "预算上限已用尽，已停止调用模型")
		case backend.IsBackendError(err):
			return o.suspend(task, err.Error())
		default:
			return task, err
		}
	}
	if err := o.store.AppendEvent(task.ID, "step_finished", map[string]any{
		"turns":              res.Turns,
		"summary":            truncateRunes(res.FinalText, 2000),
		"input_tokens":       res.Usage.InputTokens,
		"output_tokens":      res.Usage.OutputTokens,
		"cache_read_tokens":  res.Usage.CacheReadTokens,
		"cache_write_tokens": res.Usage.CacheWriteTokens,
	}); err != nil {
		return task, err
	}
	return o.move(task, state.Verifying, nil, nil)
}

// verifyStep 做防作弊检查、运行验证命令与验收标准，失败则回到 CODING 修复。
func (o *Orchestrator) verifyStep(task store.Task, repo config.RepoConfig) (store.Task, error) {
	info := workspace.Info{Path: task.WorktreePath, Branch: task.Branch, BaseSHA: task.BaseSHA}
	if info.Path == "" {
		return o.suspend(task, "任务缺少工作区信息，无法验证")
	}
	plan, _ := planning.Parse(task.PlanJSON)

	changes, err := o.ws.ChangedFiles(info)
	if err != nil {
		return task, err
	}
	diff, err := o.ws.Diff(info)
	if err != nil {
		return task, err
	}

	if findings := verify.Anticheat(changes, diff, repo.VerifyConfigFiles); len(findings) > 0 {
		if err := o.store.AppendEvent(task.ID, "anticheat", map[string]any{"findings": findings}); err != nil {
			return task, err
		}
		return o.move(task, state.NeedsHuman, map[string]any{
			"reason":   "防作弊检查命中",
			"findings": findings,
		}, nil)
	}

	verifier := verify.Verifier{
		Timeout:      time.Duration(o.cfg.Policy.CommandTimeoutS) * time.Second,
		ArtifactsDir: filepath.Join(o.cfg.RunsDir(), task.ID, "artifacts"),
		TailChars:    3000,
	}

	failure := ""
	if len(changes) == 0 {
		failure = "本步骤未产生任何文件改动，请根据需求完成实际修改。"
		if err := o.store.RecordVerification(task.ID, task.FixAttempts, false,
			map[string]any{"passed": false, "reason": failure}); err != nil {
			return task, err
		}
	} else {
		rep, err := verifier.RunAll(info.Path, repo.VerifyCommands, plan.Acceptance, task.FixAttempts)
		if err != nil {
			return task, err
		}
		if err := o.store.RecordVerification(task.ID, task.FixAttempts, rep.Passed, rep.ToMap()); err != nil {
			return task, err
		}
		if rep.Passed {
			return o.move(task, state.Committing, map[string]any{
				"changed_files": len(changes),
				"acceptance":    len(rep.Acceptance),
			}, nil)
		}
		failure = rep.FailureSummary()
	}

	if task.FixAttempts >= o.cfg.Policy.MaxFixAttempts {
		return o.move(task, state.NeedsHuman, map[string]any{
			"reason":       "修复次数已达上限",
			"last_failure": failure,
		}, nil)
	}
	task, err = o.move(task, state.Coding, map[string]any{"last_failure": truncateRunes(failure, 2000)},
		map[string]any{"fix_attempts": task.FixAttempts + 1})
	if err != nil {
		return task, err
	}
	if err := o.store.AppendEvent(task.ID, "fix_requested", map[string]any{
		"attempt": task.FixAttempts,
		"failure": truncateRunes(failure, 2000),
	}); err != nil {
		return task, err
	}
	return task, nil
}

// deliverStep 提交改动，并（在开启交付时）扫描密钥、推送分支。
func (o *Orchestrator) deliverStep(task store.Task, repo config.RepoConfig) (store.Task, error) {
	info := workspace.Info{Path: task.WorktreePath, Branch: task.Branch, BaseSHA: task.BaseSHA}
	if info.Path == "" {
		return o.suspend(task, "任务缺少工作区信息，无法提交")
	}
	sha, excluded, err := o.ws.Commit(info, commitMessage(task))
	if err != nil {
		return task, err
	}
	if len(excluded) > 0 {
		// 生成产物被排除是正常行为，但要留痕：PR 里看不到它们必须是有理由的。
		if eerr := o.store.AppendEvent(task.ID, "commit_artifacts_excluded", map[string]any{
			"paths": excluded,
		}); eerr != nil {
			return task, eerr
		}
	}
	if sha == "" {
		return o.move(task, state.NeedsHuman, map[string]any{"reason": "没有可提交的改动"}, nil)
	}
	shaFields := map[string]any{"commit_sha": sha}

	deliver := o.cfg.Delivery.Enabled || o.forceDelev
	if !deliver {
		return o.move(task, state.Done,
			map[string]any{"commit_sha": sha, "delivery": "disabled"},
			mergeFields(shaFields, map[string]any{"finished_at": now()}))
	}

	diff, err := o.ws.Diff(info)
	if err != nil {
		return task, err
	}
	if findings := delivery.ScanDiff(diff); len(findings) > 0 {
		if eerr := o.store.AppendEvent(task.ID, "secret_scan_failed", map[string]any{
			"findings": delivery.MarshalFindings(findings),
		}); eerr != nil {
			return task, eerr
		}
		return o.move(task, state.NeedsHuman, map[string]any{
			"reason":   "密钥扫描命中，已阻止推送",
			"findings": delivery.MarshalFindings(findings),
		}, shaFields)
	}

	ok, err := delivery.RemoteExists(info.Path, repo.Remote)
	if err != nil {
		return task, err
	}
	if !ok {
		return o.move(task, state.NeedsHuman,
			map[string]any{"reason": fmt.Sprintf("远端 %q 不存在，无法推送", repo.Remote)}, shaFields)
	}
	if err := delivery.Push(info.Path, repo.Remote, info.Branch); err != nil {
		return o.move(task, state.NeedsHuman, map[string]any{"reason": err.Error()}, shaFields)
	}
	return o.move(task, state.Pushed,
		map[string]any{"commit_sha": sha, "remote": repo.Remote, "branch": info.Branch}, shaFields)
}

// prStep 创建草稿 PR。gh 不可用时挂起：分支已推送，人工创建 PR 即可。
func (o *Orchestrator) prStep(task store.Task, repo config.RepoConfig) (store.Task, error) {
	host := o.codeHost
	if host == nil {
		if !delivery.Available() {
			return o.move(task, state.NeedsHuman, map[string]any{
				"reason": "gh CLI 不可用；分支已推送，请手工创建 PR",
				"branch": task.Branch,
			}, nil)
		}
		host = delivery.GHCli{}
	}
	pr, err := host.CreatePR(context.Background(), delivery.PRRequest{
		Worktree: task.WorktreePath,
		Base:     repo.DefaultBranch,
		Head:     task.Branch,
		Title:    prTitle(task),
		Body:     prBody(o.store, task),
		Draft:    o.cfg.Delivery.DraftPR,
	})
	if err != nil {
		return o.move(task, state.NeedsHuman, map[string]any{"reason": err.Error()}, nil)
	}
	task, err = o.move(task, state.PROpened,
		map[string]any{"pr_url": pr.URL, "pr_number": pr.Number, "draft": o.cfg.Delivery.DraftPR},
		map[string]any{"pr_url": pr.URL, "pr_number": pr.Number})
	if err != nil {
		return task, err
	}
	return o.move(task, state.Done, map[string]any{"pr_url": pr.URL}, map[string]any{"finished_at": now()})
}

// ---- 计费与预算 ----

func (o *Orchestrator) usageHook(taskID, role string) func(llm.Usage) error {
	return func(u llm.Usage) error {
		if err := o.recordUsage(taskID, role, o.modelName, u); err != nil {
			return err
		}
		return o.checkBudget(taskID)
	}
}

func (o *Orchestrator) recordUsage(taskID, role, model string, u llm.Usage) error {
	if u.Empty() {
		return nil
	}
	usd, known := o.pricing.Cost(model, u)
	entry := store.CostEntry{
		TS: now(), Role: role, Model: model,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
		CostUSD: usd,
	}
	if err := o.store.RecordCost(taskID, role, model, entry); err != nil {
		return err
	}
	if err := o.store.AddSpend(taskID, usd); err != nil {
		return err
	}
	if !known && !o.warnedPricing {
		o.warnedPricing = true
		return o.store.AppendEvent(taskID, "cost_pricing_missing", map[string]any{
			"model": model,
			"note":  "models.yaml 未配置该模型单价，本次调用按 $0 计费，预算熔断不生效",
		})
	}
	return nil
}

// checkBudget 返回 cost.ErrBudgetExceeded 表示已经用尽预算。
func (o *Orchestrator) checkBudget(taskID string) error {
	task, err := o.store.GetTask(taskID)
	if err != nil {
		return err
	}
	if task.BudgetUSD <= 0 {
		return nil
	}
	ratio := task.SpentUSD / task.BudgetUSD
	if ratio >= 1 {
		return cost.ErrBudgetExceeded
	}
	if ratio >= cost.WarningThreshold && !o.warnedBudget {
		o.warnedBudget = true
		return o.store.AppendEvent(task.ID, "budget_warning", map[string]any{
			"spent_usd": task.SpentUSD, "budget_usd": task.BudgetUSD,
		})
	}
	return nil
}

// ---- 辅助 ----

func (o *Orchestrator) move(task store.Task, to state.Status, payload map[string]any, fields map[string]any) (store.Task, error) {
	return o.store.Transition(task.ID, task.Version, to, payload, fields)
}

// suspend 把任务挂起到 NEEDS_HUMAN，等待人工处理（resume）。
func (o *Orchestrator) suspend(task store.Task, reason string) (store.Task, error) {
	if state.Terminal[task.Status] {
		return task, nil
	}
	return o.move(task, state.NeedsHuman, map[string]any{"reason": reason}, nil)
}

func (o *Orchestrator) fail(task store.Task, reason string) (store.Task, error) {
	if state.Terminal[task.Status] {
		return task, nil
	}
	return o.store.Transition(task.ID, task.Version, state.Failed,
		map[string]any{"reason": reason},
		map[string]any{"finished_at": now()})
}

func (o *Orchestrator) violationRecorder(taskID string) tools.ViolationHook {
	return func(kind, detail string, blocked bool) {
		_ = o.store.RecordViolation(taskID, kind, detail, blocked)
	}
}

func (o *Orchestrator) ensureWorktree(task store.Task, repo config.RepoConfig) (workspace.Info, error) {
	if task.WorktreePath != "" {
		if _, err := os.Stat(task.WorktreePath); err == nil {
			return workspace.Info{Path: task.WorktreePath, Branch: task.Branch, BaseSHA: task.BaseSHA}, nil
		}
	}
	return o.ws.Create(task.Repo, repo, task.ID, slugSource(task))
}

func failureOf(task store.Task, repo store.Repository) string {
	events, err := repo.ListEvents(task.ID)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "fix_requested" {
			if s, ok := events[i].Payload["failure"].(string); ok {
				return s
			}
		}
	}
	return ""
}

func verifyNames(repo config.RepoConfig) []string {
	out := make([]string, 0, len(repo.VerifyCommands))
	for _, c := range repo.VerifyCommands {
		out = append(out, c.Name)
	}
	return out
}

func commandMap(repo config.RepoConfig) map[string]string {
	out := make(map[string]string, len(repo.VerifyCommands))
	for _, c := range repo.VerifyCommands {
		out[c.Name] = c.Command
	}
	return out
}

func nameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

func mergeFields(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func mergePayload(a, b map[string]any) map[string]any { return mergeFields(a, b) }

var skipSummaryDirs = map[string]bool{
	".git": true, "node_modules": true, "__pycache__": true, ".venv": true, "venv": true,
	"dist": true, "build": true, "target": true, ".idea": true, ".gradle": true,
}

// repoSummary 生成给规划器看的浅层文件概览。信任边界：这是仓库内容，仅作参考。
func repoSummary(root string) string {
	if root == "" {
		return ""
	}
	const maxEntries = 200
	var lines []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		depth := strings.Count(rel, "/")
		if d.IsDir() {
			if skipSummaryDirs[d.Name()] || depth >= 2 {
				return filepath.SkipDir
			}
			return nil
		}
		if depth >= 3 {
			return nil
		}
		lines = append(lines, rel)
		if len(lines) >= maxEntries {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return ""
	}
	return strings.Join(lines, "\n")
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func slugSource(task store.Task) string {
	if task.Title != "" {
		return task.Title
	}
	return truncateRunes(task.Requirement, 60)
}

func prTitle(task store.Task) string {
	first := firstLine(task.Requirement)
	if first == "" {
		first = task.ID
	}
	return fmt.Sprintf("[agentdev] %s", truncateRunes(first, 72))
}

func prBody(repo store.Repository, task store.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "由 agentdev 自动生成的任务 **%s**。\n\n", task.ID)
	b.WriteString("## 需求\n\n")
	fmt.Fprintf(&b, "> %s\n\n", strings.ReplaceAll(task.Requirement, "\n", "\n> "))
	if plan, err := planning.Parse(task.PlanJSON); err == nil && plan.Summary != "" {
		b.WriteString("## 计划\n\n")
		fmt.Fprintf(&b, "%s\n\n", plan.Summary)
		if len(plan.Acceptance) > 0 {
			b.WriteString("## 验收标准\n\n")
			for _, a := range plan.Acceptance {
				fmt.Fprintf(&b, "- `%s` %s\n", a.ID, a.Description)
			}
			b.WriteString("\n")
		}
	}
	if runs, err := repo.ListVerifications(task.ID); err == nil && len(runs) > 0 {
		last := runs[len(runs)-1]
		b.WriteString("## 验证\n\n")
		fmt.Fprintf(&b, "- 第 %d 次验证：通过=%v\n", last.Attempt, last.Passed)
		if outcomes, ok := last.Report["outcomes"].([]any); ok {
			for _, o := range outcomes {
				m, _ := o.(map[string]any)
				fmt.Fprintf(&b, "  - `%v`：退出码 %v\n", m["command"], m["exit_code"])
			}
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "## 成本\n\n- 预算 $%.2f，已用 $%.4f\n\n", task.BudgetUSD, task.SpentUSD)
	b.WriteString("本 PR 由自动化流程创建为草稿，**不会自动合并**，请人工审阅后再合并。\n")
	return b.String()
}

func commitMessage(task store.Task) string {
	first := firstLine(task.Requirement)
	if first == "" {
		first = task.ID
	}
	return fmt.Sprintf("agent(%s): %s\n\nTask: %s", task.ID, truncateRunes(first, 72), task.ID)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
