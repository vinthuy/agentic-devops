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
	"agentdev/internal/review"
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
	reviewer   review.Reviewer
	codeHost   delivery.CodeHost
	ciWatcher  delivery.CIWatcher
	ciPoll     time.Duration
	ciTimeout  time.Duration
	sleep      func(context.Context, time.Duration) error
	forceDelev bool

	warnedPricing bool
	warnedBudget  bool
	// auditErr 记录审计事件写入失败（钩子无法返回错误），由步骤结束后上报。
	auditErr error
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

// WithReviewer 覆盖默认的 Reviewer（测试用）。
func WithReviewer(r review.Reviewer) Option {
	return func(o *Orchestrator) { o.reviewer = r }
}

// WithCIWatcher 覆盖默认的 CI 查询实现（测试用）。
func WithCIWatcher(w delivery.CIWatcher) Option {
	return func(o *Orchestrator) { o.ciWatcher = w }
}

// WithCITiming 覆盖 CI 轮询间隔与总超时（测试中设为极小值）。
func WithCITiming(poll, timeout time.Duration) Option {
	return func(o *Orchestrator) {
		o.ciPoll = poll
		o.ciTimeout = timeout
	}
}

// WithDelivery 强制开启推送与创建 PR，用于覆盖 delivery.enabled。
func WithDelivery(enabled bool) Option {
	return func(o *Orchestrator) { o.forceDelev = enabled }
}

func New(repo store.Repository, cfg *config.App, be backend.ExecutionBackend, opts ...Option) *Orchestrator {
	o := &Orchestrator{
		store: repo, cfg: cfg, backend: be, ws: workspace.New(cfg),
		ciPoll:    time.Duration(cfg.Delivery.CIPollSeconds) * time.Second,
		ciTimeout: time.Duration(cfg.Delivery.CITimeoutMinutes) * time.Minute,
		sleep:     sleepCtx,
	}
	if o.ciPoll <= 0 {
		o.ciPoll = 60 * time.Second
	}
	if o.ciTimeout <= 0 {
		o.ciTimeout = 30 * time.Minute
	}
	for _, opt := range opts {
		opt(o)
	}
	return o
}

// sleepCtx 等待指定时长，可被上下文取消。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
		case state.Reviewing:
			task, err = o.reviewStep(task, repo)
		case state.Committing:
			task, err = o.deliverStep(task, repo)
		case state.Pushed:
			task, err = o.prStep(task, repo)
		case state.PROpened:
			if o.cfg.Delivery.CIWatch {
				task, err = o.move(task, state.CIWatching,
					map[string]any{"pr_number": task.PRNumber, "reason": "等待 CI 结果"}, nil)
			} else {
				task, err = o.move(task, state.Done,
					map[string]any{
						"reason": "PR 已创建", "ci": "skipped",
						"summary": "CI 监听未开启；不会将未查询状态标记为通过",
					}, map[string]any{"finished_at": now()})
			}
		case state.CIWatching:
			task, err = o.ciStep(task, repo)
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
			}).Estimate(context.Background(), task.Requirement, repo, task.Source)
			if eerr != nil {
				if uerr := o.recordUsage(task.ID, "triage", o.modelName, est.Usage); uerr != nil {
					return task, uerr
				}
				// 分级失败不应阻断任务：记录后按规则结果继续。
				if aerr := o.store.AppendEvent(task.ID, "triage_model_failed",
					map[string]any{"error": delivery.RedactText(eerr.Error())}); aerr != nil {
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
		Source:      task.Source,
		Text:        task.Requirement,
		Feedback:    planFeedback(o.store, task.ID),
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
			"error":  delivery.RedactText(err.Error()),
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
	o.auditErr = nil
	rt := tools.New(pol, o.cfg.Policy.ToolOutputMaxChars, o.cfg.Policy.CommandTimeoutS, o.violationRecorder(task.ID))
	rt.OnToolCall(o.toolCallRecorder(task.ID))

	res, err := o.backend.RunStep(backend.StepContext{
		TaskID:         task.ID,
		Source:         task.Source,
		Requirement:    task.Requirement,
		Plan:           plan.PromptText(),
		Worktree:       info.Path,
		Tools:          rt,
		MaxTurns:       o.cfg.Policy.MaxTurnsPerStep,
		FailureSummary: failureOf(task, o.store),
		OnUsage:        o.usageHook(task.ID, "coder"),
	})
	// 审计写入失败优先上报：没有工具调用记录就失去可追溯性，不能当作成功。
	if o.auditErr != nil {
		return task, o.auditErr
	}
	if err != nil {
		if need, ok := backend.AsInfoNeeded(err); ok {
			// 模型主动请求澄清：这是正常暂停，不是失败。问题写入 interactions，
			// 用户的回复会并入需求文本，下一轮规划能看到完整上下文。
			if rerr := o.store.RecordInteraction(task.ID, "question", strings.Join(need.Questions, "\n")); rerr != nil {
				return task, rerr
			}
			return o.move(task, state.NeedsInfo, map[string]any{
				"questions": need.Questions,
				"reason":    "编码步骤请求补充信息",
			}, nil)
		}
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
	var auxFailed []string
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
			next := state.Committing
			if o.reviewEnabled() {
				next = state.Reviewing
			}
			return o.move(task, next, map[string]any{
				"changed_files": len(changes),
				"acceptance":    len(rep.Acceptance),
			}, nil)
		}
		// 验收标准本身不可执行（选择器匹配不到测试）不是代码缺陷：
		// 改代码永远修不好它，必须回到规划，且只自动重规划一次。
		if invalid := rep.InvalidAcceptance(); len(invalid) > 0 {
			return o.replanForInvalidAcceptance(task, invalid)
		}
		failure = rep.FailureSummary()
		auxFailed = auxiliaryFailures(plan.Acceptance, rep.Acceptance)
	}

	if task.FixAttempts >= o.cfg.Policy.MaxFixAttempts {
		reason := "修复次数已达上限"
		if len(auxFailed) > 0 {
			// 可执行的主验证（command/test）都过了，只剩 grep/file_exists 这类
			// 结构断言不满足。改代码往往永远满足不了写死的模式，真实原因更可能是
			// 验收标准本身写错；如实说明，避免把规划缺陷伪装成代码缺陷。
			reason = "可执行验证已通过，但附加验收标准未满足（可能是验收标准本身写错，例如 grep 模式过窄）"
		}
		return o.move(task, state.NeedsHuman, map[string]any{
			"reason":       reason,
			"last_failure": failure,
			"aux_failed":   auxFailed,
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

// auxiliaryFailures 在“可执行验证全部通过、只剩结构断言未满足”时返回这些断言 ID。
// command/test 执行真实行为，是主验证；grep/file_exists 只是结构声明，写错时
// 改代码永远满足不了。返回空表示存在主验证失败，应按普通修复处理。
func auxiliaryFailures(planned []planning.Acceptance, got []verify.AcceptanceOutcome) []string {
	kinds := make(map[string]planning.CheckType, len(planned))
	for _, a := range planned {
		kinds[a.ID] = a.Check.Type
	}
	var aux []string
	for _, o := range got {
		if o.Passed {
			continue
		}
		switch kinds[o.ID] {
		case planning.CheckCommand, planning.CheckTest:
			return nil
		case planning.CheckGrep, planning.CheckFileExists:
			aux = append(aux, o.ID)
		}
	}
	return aux
}

// planFeedback 从最近一次“验收标准不可执行”事件生成重新规划的反馈。
func planFeedback(repo store.Repository, taskID string) string {
	events, err := repo.ListEvents(taskID)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "acceptance_invalid" {
			continue
		}
		detail, _ := events[i].Payload["reason"].(string)
		var lines []string
		if findings, ok := events[i].Payload["findings"].([]any); ok {
			for _, f := range findings {
				m, _ := f.(map[string]any)
				lines = append(lines, fmt.Sprintf("- [%v] %v → %v",
					m["id"], m["description"], m["evidence"]))
			}
		}
		if len(lines) == 0 {
			return detail
		}
		return detail + "\n" + strings.Join(lines, "\n")
	}
	return ""
}

// replanForInvalidAcceptance 处理不可执行的验收标准：回到 PLANNING 重新规划一次；
// 若已经重规划过，说明模型仍给不出可执行的检查，转人工而不是继续烧预算。
func (o *Orchestrator) replanForInvalidAcceptance(
	task store.Task, invalid []verify.AcceptanceOutcome,
) (store.Task, error) {
	findings := make([]any, 0, len(invalid))
	for _, a := range invalid {
		findings = append(findings, map[string]any{
			"id": a.ID, "description": a.Description,
			"target": a.Target, "evidence": truncateRunes(a.Evidence, 500),
		})
	}
	payload := map[string]any{
		"reason":   "验收标准不可执行：选择器未匹配到任何测试",
		"findings": findings,
	}
	if err := o.store.AppendEvent(task.ID, "acceptance_invalid", payload); err != nil {
		return task, err
	}

	events, err := o.store.ListEvents(task.ID)
	if err != nil {
		return task, err
	}
	for _, e := range events {
		if e.Type != "plan_replanned" {
			continue
		}
		// 恢复到 PLANNING 才有意义：恢复到 VERIFYING 只会用同一份坏计划再次失败。
		return o.move(task, state.NeedsHuman, map[string]any{
			"reason":   "重新规划后验收标准仍不可执行，需人工修正计划",
			"findings": findings,
		}, map[string]any{"previous_status": string(state.Planning)})
	}
	if err := o.store.AppendEvent(task.ID, "plan_replanned", map[string]any{
		"cause": "acceptance_invalid",
	}); err != nil {
		return task, err
	}
	return o.move(task, state.Planning, map[string]any{
		"reason": "验收标准不可执行，已回到规划重新生成",
	}, nil)
}

// reviewEnabled 报告是否启用独立审查。没有模型时无从审查，直接跳过。
func (o *Orchestrator) reviewEnabled() bool {
	return o.cfg.Review.Enabled && (o.reviewer != nil || o.model != nil)
}

// reviewStep 让独立 Reviewer 审查最终 diff。
//
// 审查本身失败（网络、格式、校验）不等于代码有问题，因此挂起等人工判断，
// 而不是当作“不通过”回炉改代码——那会把一次接口故障变成无谓的改动。
func (o *Orchestrator) reviewStep(task store.Task, repo config.RepoConfig) (store.Task, error) {
	info := workspace.Info{Path: task.WorktreePath, Branch: task.Branch, BaseSHA: task.BaseSHA}
	if info.Path == "" {
		return o.suspend(task, "任务缺少工作区信息，无法审查")
	}
	diff, err := o.ws.Diff(info)
	if err != nil {
		return task, err
	}
	plan, _ := planning.Parse(task.PlanJSON)

	reviewer := o.reviewer
	if reviewer == nil {
		reviewer = &review.ModelReviewer{
			Client: o.model, Model: o.modelName, MaxTokens: o.maxTokens,
			OnUsage: func(role string, u llm.Usage) {
				_ = o.recordUsage(task.ID, role, o.modelName, u)
			},
		}
	}
	verdict, err := reviewer.Review(context.Background(), review.Input{
		Source:      task.Source,
		Requirement: task.Requirement,
		Plan:        plan.PromptText(),
		Diff:        diff,
		Acceptance:  acceptanceSummary(o.store, task.ID),
	})
	if err == nil {
		// 即便替换成自定义 Reviewer，也必须经过同一套程序侧校验；
		// 不允许 approve 掩盖阻塞意见，或引用 diff 之外的路径。
		err = review.ValidateAgainstDiff(verdict, diff)
	}
	if err != nil {
		if aerr := o.store.AppendEvent(task.ID, "review_failed",
			map[string]any{"error": truncateRunes(delivery.RedactText(err.Error()), 2000)}); aerr != nil {
			return task, aerr
		}
		return o.suspend(task, "独立审查失败，需人工确认: "+delivery.RedactText(err.Error()))
	}
	if err := o.checkBudget(task.ID); err != nil {
		return o.suspend(task, "审查后预算已用尽")
	}
	verdict.Summary = delivery.RedactText(verdict.Summary)
	for i := range verdict.Findings {
		verdict.Findings[i].Comment = delivery.RedactText(verdict.Findings[i].Comment)
	}
	if err := o.store.AppendEvent(task.ID, "review_result", map[string]any{
		"decision": string(verdict.Decision), "summary": verdict.Summary,
		"findings": reviewFindings(verdict),
	}); err != nil {
		return task, err
	}

	if verdict.Approved() {
		return o.move(task, state.Committing, map[string]any{
			"review":   "approve",
			"summary":  verdict.Summary,
			"findings": len(verdict.Findings),
		}, nil)
	}

	attempts := task.ReviewAttempts + 1
	if attempts > o.cfg.Policy.MaxReviewAttempts {
		return o.move(task, state.NeedsHuman, map[string]any{
			"reason":          fmt.Sprintf("审查未通过且已达上限（%d 次）", o.cfg.Policy.MaxReviewAttempts),
			"last_failure":    truncateRunes(verdict.Format(), 2000),
			"review_findings": reviewFindings(verdict),
		}, nil)
	}
	if aerr := o.store.AppendEvent(task.ID, "fix_requested", map[string]any{
		"attempt": attempts,
		"source":  "reviewer",
		"failure": truncateRunes(verdict.Format(), 2000),
	}); aerr != nil {
		return task, aerr
	}
	return o.move(task, state.Coding, map[string]any{
		"review":   "request_changes",
		"attempt":  attempts,
		"summary":  verdict.Summary,
		"findings": reviewFindings(verdict),
	}, map[string]any{"review_attempts": attempts})
}

// acceptanceSummary 把最近一次验收结果整理成审查者能直接引用的证据。
func acceptanceSummary(repo store.Repository, taskID string) string {
	runs, err := repo.ListVerifications(taskID)
	if err != nil || len(runs) == 0 {
		return ""
	}
	last := runs[len(runs)-1]
	var b strings.Builder
	flag := "未通过"
	if last.Passed {
		flag = "通过"
	}
	fmt.Fprintf(&b, "验证命令：%s\n", flag)
	acceptance, _ := last.Report["acceptance"].([]any)
	for _, a := range acceptance {
		m, _ := a.(map[string]any)
		mark := "未满足"
		if p, _ := m["passed"].(bool); p {
			mark = "满足"
		}
		fmt.Fprintf(&b, "- [%v] %v：%s\n", m["id"], m["description"], mark)
	}
	return strings.TrimRight(b.String(), "\n")
}

func reviewFindings(r review.Review) []any {
	out := make([]any, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, map[string]any{
			"path": f.Path, "line": f.Line, "severity": string(f.Severity), "comment": f.Comment,
		})
	}
	return out
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
		// 崩溃恢复只适用于“提交成功但状态没落库”：此时任务还没记录过 commit_sha。
		// 若任务已有 commit_sha，说明本次确实没有新改动（例如 CI 失败后编码器没改代码），
		// 不能复用旧提交反复重推，否则会让 CI 重试计数空转。
		if task.CommitSHA == "" {
			recovered, ok, herr := o.ws.HeadCommitFor(info, task.ID)
			if herr != nil {
				return task, herr
			}
			if ok {
				sha = recovered
				if aerr := o.store.AppendEvent(task.ID, "commit_recovered", map[string]any{
					"commit": sha, "note": "提交已存在但状态未落库，已复用该提交",
				}); aerr != nil {
					return task, aerr
				}
			}
		}
		if sha == "" {
			return o.move(task, state.NeedsHuman, map[string]any{"reason": "没有可提交的改动"}, nil)
		}
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
	if err := delivery.Push(delivery.PushRequest{
		Worktree:      info.Path,
		Remote:        repo.Remote,
		Branch:        info.Branch,
		Protected:     repo.DefaultBranch,
		AllowedPrefix: o.cfg.Delivery.BranchPrefix,
	}); err != nil {
		return o.move(task, state.NeedsHuman, map[string]any{"reason": err.Error()}, shaFields)
	}
	return o.move(task, state.Pushed,
		map[string]any{"commit_sha": sha, "remote": repo.Remote, "branch": info.Branch}, shaFields)
}

// prStep 幂等地创建或更新草稿 PR。gh 不可用时挂起：分支已推送，人工创建 PR 即可。
//
// 必须幂等：CI 失败会回到 CODING 再走一遍交付流程，直接 create 会撞上
// “a pull request already exists”，把一次可修复的 CI 失败变成人工介入。
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
	pr, err := host.EnsurePR(context.Background(), delivery.PRRequest{
		Worktree: task.WorktreePath,
		Base:     repo.DefaultBranch,
		Head:     task.Branch,
		Title:    prTitle(task),
		Body:     prBody(o.store, o.cfg, task),
		Draft:    o.cfg.Delivery.DraftPR,
	})
	if err != nil {
		return o.move(task, state.NeedsHuman, map[string]any{"reason": err.Error()}, nil)
	}
	action := "updated"
	if pr.Created {
		action = "created"
	}
	return o.move(task, state.PROpened,
		map[string]any{
			"pr_url": pr.URL, "pr_number": pr.Number, "draft": o.cfg.Delivery.DraftPR,
			"action": action,
		},
		map[string]any{"pr_url": pr.URL, "pr_number": pr.Number})
}

// ciStep 轮询 PR 的 CI 结果（SDD 5.7）。
//
// 通过 → DONE；失败 → 回传日志并回到 CODING（受 max_ci_attempts 限制）；
// 没有 CI → DONE 并明确记录“未运行 CI”；等待超时 → 挂起等人工。
func (o *Orchestrator) ciStep(task store.Task, repo config.RepoConfig) (store.Task, error) {
	finish := func(payload map[string]any) (store.Task, error) {
		return o.move(task, state.Done, payload, map[string]any{"finished_at": now()})
	}
	if !o.cfg.Delivery.CIWatch {
		return finish(map[string]any{"ci": "skipped", "reason": "未开启 CI 监听"})
	}
	if task.PRNumber <= 0 {
		return o.suspend(task, "PR 编号缺失，无法监听 CI")
	}

	watcher := o.ciWatcher
	if watcher == nil {
		if !delivery.Available() {
			return o.suspend(task, "gh CLI 不可用，无法查询 CI 状态")
		}
		watcher = delivery.GHCli{}
	}
	req := delivery.CIRequest{
		Worktree: task.WorktreePath,
		PRNumber: task.PRNumber,
		Head:     task.Branch,
	}
	deadline := time.Now().Add(o.ciTimeout)

	for {
		snap, err := watcher.Checks(context.Background(), req)
		if err != nil {
			return o.suspend(task, "查询 CI 状态失败: "+delivery.RedactText(err.Error()))
		}
		switch snap.State {
		case delivery.CINone:
			return finish(map[string]any{
				"ci": "none", "summary": snap.Summary,
				"note": "PR 上未配置 CI 检查，按无 CI 处理",
			})
		case delivery.CIPassed:
			return finish(map[string]any{"ci": "passed", "summary": snap.Summary})
		case delivery.CIFailed:
			snap.Summary = delivery.RedactText(snap.Summary)
			snap.Logs = delivery.RedactText(snap.Logs)
			if aerr := o.store.AppendEvent(task.ID, "ci_failed", map[string]any{
				"summary": snap.Summary, "checks": snap.Checks,
			}); aerr != nil {
				return task, aerr
			}
			attempts := task.CIAttempts + 1
			if attempts > o.cfg.Delivery.MaxCIAttempts {
				return o.move(task, state.NeedsHuman, map[string]any{
					"reason":  fmt.Sprintf("CI 失败且已达上限（%d 次）", o.cfg.Delivery.MaxCIAttempts),
					"summary": snap.Summary,
				}, nil)
			}
			failure := "CI 失败：" + snap.Summary
			if snap.Logs != "" {
				failure += "\n\n" + snap.Logs
			}
			if aerr := o.store.AppendEvent(task.ID, "fix_requested", map[string]any{
				"attempt": attempts,
				"source":  "ci",
				"failure": truncateRunes(failure, 2000),
			}); aerr != nil {
				return task, aerr
			}
			return o.move(task, state.Coding, map[string]any{
				"ci": "failed", "attempt": attempts, "summary": snap.Summary,
			}, map[string]any{"ci_attempts": attempts})
		case delivery.CIPending:
			if !time.Now().Before(deadline) {
				return o.move(task, state.NeedsHuman, map[string]any{
					"reason":  fmt.Sprintf("CI 超过 %s 仍未完成", o.ciTimeout),
					"summary": snap.Summary,
				}, nil)
			}
			wait := o.ciPoll
			remaining := time.Until(deadline)
			if wait > remaining {
				wait = remaining
			}
			if serr := o.sleep(context.Background(), wait); serr != nil {
				return task, serr
			}
		default:
			return o.suspend(task, "未知的 CI 状态: "+string(snap.State))
		}
	}
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

// toolCallRecorder 把每次工具调用写入事件表，满足“所有工具调用可追溯”（FR-021）。
// 写入失败不静默吞掉：记录到 auditErr，由本步骤结束后统一上报。
func (o *Orchestrator) toolCallRecorder(taskID string) tools.ToolCallHook {
	return func(name string, args map[string]any, ok bool, detail string) {
		payload := map[string]any{
			"tool": name, "ok": ok,
			"detail": delivery.RedactText(detail),
		}
		for k, v := range args {
			if k == "tool" {
				continue
			}
			if s, isString := v.(string); isString {
				payload[k] = delivery.RedactText(s)
				continue
			}
			payload[k] = v
		}
		if err := o.store.AppendEvent(taskID, "tool_call", payload); err != nil && o.auditErr == nil {
			o.auditErr = fmt.Errorf("写入工具调用审计失败: %w", err)
		}
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

func prBody(repo store.Repository, cfg *config.App, task store.Task) string {
	var b strings.Builder
	events, _ := repo.ListEvents(task.ID)

	fmt.Fprintf(&b, "由 agentdev 自动生成的任务 **%s**。\n\n", task.ID)
	if task.Source == "issue" && task.SourceRef != "" {
		fmt.Fprintf(&b, "来源：GitHub Issue %s\n\n", task.SourceRef)
	} else {
		b.WriteString("来源：CLI 需求\n\n")
	}

	b.WriteString("## 需求\n\n")
	fmt.Fprintf(&b, "> %s\n\n", strings.ReplaceAll(task.Requirement, "\n", "\n> "))

	if plan, err := planning.Parse(task.PlanJSON); err == nil && plan.Summary != "" {
		b.WriteString("## 计划\n\n")
		fmt.Fprintf(&b, "- 来源：%s\n", planSourceLabel(events))
		if len(plan.Scope.AllowedPaths) > 0 {
			fmt.Fprintf(&b, "- 允许改动范围：%s\n", strings.Join(plan.Scope.AllowedPaths, ", "))
		}
		b.WriteString("\n")
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
		b.WriteString("## 验证结果\n\n")
		if last.Passed {
			b.WriteString("验证命令与验收标准全部通过。\n\n")
		} else {
			b.WriteString("> 注意：验证未通过，本 PR 不应合并。\n\n")
		}
		if outcomes, ok := last.Report["outcomes"].([]any); ok && len(outcomes) > 0 {
			b.WriteString("| 命令 | 退出码 |\n| --- | --- |\n")
			for _, o := range outcomes {
				m, _ := o.(map[string]any)
				fmt.Fprintf(&b, "| `%v` | %v |\n", m["command"], m["exit_code"])
			}
			b.WriteString("\n")
		}
		if acceptance, ok := last.Report["acceptance"].([]any); ok && len(acceptance) > 0 {
			b.WriteString("| 验收标准 | 结果 |\n| --- | --- |\n")
			for _, a := range acceptance {
				m, _ := a.(map[string]any)
				mark := "未满足"
				if p, _ := m["passed"].(bool); p {
					mark = "满足"
				}
				fmt.Fprintf(&b, "| [%v] %v | %s |\n", m["id"], m["description"], mark)
			}
			b.WriteString("\n")
		}
	}

	if s := lastReviewSummary(events); s != "" {
		fmt.Fprintf(&b, "## 独立审查\n\n%s\n\n", s)
	}

	fmt.Fprintf(&b, "## 成本\n\n- 预算 $%.2f，已用 $%.4f\n\n", task.BudgetUSD, task.SpentUSD)
	if cfg != nil {
		fmt.Fprintf(&b, "运行报告：`%s`\n\n", filepath.Join(cfg.RunsDir(), task.ID, "report.md"))
	}
	b.WriteString("本 PR 由自动化流程创建为草稿，**不会自动合并**，请人工审阅后再合并。\n")
	return b.String()
}

// planSourceLabel 说明计划来自模型还是静态降级，避免 PR 正文夸大验证强度。
func planSourceLabel(events []store.Event) string {
	label := "静态计划"
	for _, e := range events {
		if e.Type != "plan_ready" {
			continue
		}
		switch source, _ := e.Payload["source"].(string); source {
		case "model":
			label = "模型生成"
		case "configured_planner":
			label = "配置的自定义规划器"
		case "static_fallback":
			label = "静态降级（模型规划失败，验收标准仅来自验证命令）"
		}
	}
	return label
}

func lastReviewSummary(events []store.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != "review_result" {
			continue
		}
		decision, _ := events[i].Payload["decision"].(string)
		summary, _ := events[i].Payload["summary"].(string)
		if summary == "" {
			return decision
		}
		return fmt.Sprintf("%s：%s", decision, summary)
	}
	return ""
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
