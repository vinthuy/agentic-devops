package orchestrator

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agentdev/internal/backend"
	"agentdev/internal/config"
	"agentdev/internal/policy"
	"agentdev/internal/report"
	"agentdev/internal/state"
	"agentdev/internal/store"
	"agentdev/internal/tools"
	"agentdev/internal/verify"
	"agentdev/internal/workspace"
)

// Orchestrator 驱动单个任务从 QUEUED 走到终态或挂起状态（SDD 4 / 5）。
// M0 范围：需求即计划、无审批、本地提交不推送。
type Orchestrator struct {
	store   store.Repository
	cfg     *config.App
	backend backend.ExecutionBackend
	ws      *workspace.Manager
}

func New(repo store.Repository, cfg *config.App, be backend.ExecutionBackend) *Orchestrator {
	return &Orchestrator{store: repo, cfg: cfg, backend: be, ws: workspace.New(cfg)}
}

// RunTask 执行任务直到终态或挂起。返回错误仅表示前置条件不满足或存储失败；
// 执行过程中的业务失败会体现在返回任务的状态上。
func (o *Orchestrator) RunTask(taskID string) (store.Task, error) {
	task, err := o.store.GetTask(taskID)
	if err != nil {
		return store.Task{}, err
	}
	if task.Status != state.Queued {
		return task, fmt.Errorf("任务 %s 当前状态为 %s，只能执行 QUEUED 任务", taskID, task.Status)
	}
	repo, ok := o.cfg.Repos[task.Repo]
	if !ok {
		return o.fail(task, "仓库未在 repos.yaml 中配置: "+task.Repo)
	}

	task, err = o.execute(task, repo)
	if err != nil {
		latest, gerr := o.store.GetTask(taskID)
		if gerr != nil {
			return store.Task{}, gerr
		}
		task, err = o.fail(latest, err.Error())
		if err != nil {
			return task, err
		}
	}

	if _, rerr := report.WriteRunArtifacts(o.store, o.cfg, taskID); rerr != nil {
		return task, fmt.Errorf("写入运行报告失败: %w", rerr)
	}
	return task, nil
}

func (o *Orchestrator) execute(task store.Task, repo config.RepoConfig) (store.Task, error) {
	var err error
	task, err = o.move(task, state.Triaging, nil, nil)
	if err != nil {
		return task, err
	}

	level := task.Level
	if level == "" {
		level = "S"
	}
	switch level {
	case "L":
		return o.move(task, state.Rejected,
			map[string]any{"reason": "L 级任务不自动执行，请拆分为 S/M 级子任务"},
			map[string]any{"finished_at": now()})
	case "M":
		return o.move(task, state.NeedsHuman,
			map[string]any{"reason": "M 级需要审批，审批流程尚未实现"}, nil)
	}

	task, err = o.move(task, state.Planning, nil,
		map[string]any{"plan_json": `{"mode":"requirement-as-plan"}`})
	if err != nil {
		return task, err
	}

	info, err := o.ws.Create(task.Repo, repo, task.ID, slugSource(task))
	if err != nil {
		return task, err
	}
	task, err = o.move(task, state.Coding, nil, map[string]any{
		"branch":        info.Branch,
		"base_sha":      info.BaseSHA,
		"worktree_path": filepath.ToSlash(info.Path),
	})
	if err != nil {
		return task, err
	}

	pol, err := policy.NewEngine(o.cfg.Policy, repo, info.Path, nil)
	if err != nil {
		return task, err
	}
	rt := tools.New(pol, o.cfg.Policy.ToolOutputMaxChars, o.cfg.Policy.CommandTimeoutS, o.violationRecorder(task.ID))
	verifier := verify.Verifier{
		Timeout:      time.Duration(o.cfg.Policy.CommandTimeoutS) * time.Second,
		ArtifactsDir: filepath.Join(o.cfg.RunsDir(), task.ID, "artifacts"),
		TailChars:    3000,
	}

	failure := ""
	for {
		res, err := o.backend.RunStep(backend.StepContext{
			TaskID:         task.ID,
			Requirement:    task.Requirement,
			Plan:           task.Requirement,
			Worktree:       info.Path,
			Tools:          rt,
			MaxTurns:       o.cfg.Policy.MaxTurnsPerStep,
			FailureSummary: failure,
		})
		if err != nil {
			if backend.IsBackendError(err) {
				return o.move(task, state.NeedsHuman, map[string]any{"reason": err.Error()}, nil)
			}
			return task, err
		}
		if err := o.store.AppendEvent(task.ID, "step_finished", map[string]any{
			"turns":         res.Turns,
			"summary":       truncateRunes(res.FinalText, 2000),
			"input_tokens":  res.Usage.InputTokens,
			"output_tokens": res.Usage.OutputTokens,
		}); err != nil {
			return task, err
		}

		task, err = o.move(task, state.Verifying, nil, nil)
		if err != nil {
			return task, err
		}
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

		if len(changes) == 0 {
			failure = "本步骤未产生任何文件改动，请根据需求完成实际修改。"
			if err := o.store.RecordVerification(task.ID, task.FixAttempts, false,
				map[string]any{"passed": false, "reason": failure}); err != nil {
				return task, err
			}
		} else {
			rep, err := verifier.Run(info.Path, repo.VerifyCommands, task.FixAttempts)
			if err != nil {
				return task, err
			}
			if err := o.store.RecordVerification(task.ID, task.FixAttempts, rep.Passed, rep.ToMap()); err != nil {
				return task, err
			}
			if rep.Passed {
				break
			}
			failure = rep.FailureSummary()
		}

		if task.FixAttempts >= o.cfg.Policy.MaxFixAttempts {
			return o.move(task, state.NeedsHuman, map[string]any{
				"reason":       "修复次数已达上限",
				"last_failure": failure,
			}, nil)
		}
		task, err = o.move(task, state.Coding, nil, map[string]any{"fix_attempts": task.FixAttempts + 1})
		if err != nil {
			return task, err
		}
	}

	task, err = o.move(task, state.Committing, nil, nil)
	if err != nil {
		return task, err
	}
	sha, err := o.ws.Commit(info, commitMessage(task))
	if err != nil {
		return task, err
	}
	if sha == "" {
		return o.move(task, state.NeedsHuman, map[string]any{"reason": "没有可提交的改动"}, nil)
	}
	return o.move(task, state.Done, nil, map[string]any{
		"commit_sha":  sha,
		"finished_at": now(),
	})
}

func (o *Orchestrator) move(task store.Task, to state.Status, payload map[string]any, fields map[string]any) (store.Task, error) {
	return o.store.Transition(task.ID, task.Version, to, payload, fields)
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

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func slugSource(task store.Task) string {
	if task.Title != "" {
		return task.Title
	}
	return truncateRunes(task.Requirement, 60)
}

func commitMessage(task store.Task) string {
	first := strings.TrimSpace(task.Requirement)
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	if first == "" {
		first = task.ID
	}
	return fmt.Sprintf("agent(%s): %s\n\nTask: %s", task.ID, truncateRunes(first, 72), task.ID)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
