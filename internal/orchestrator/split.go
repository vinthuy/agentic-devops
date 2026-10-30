package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"agentdev/internal/config"
	"agentdev/internal/delivery"
	"agentdev/internal/llm"
	"agentdev/internal/planning"
	"agentdev/internal/state"
	"agentdev/internal/store"
	"agentdev/internal/triage"
)

func (o *Orchestrator) splitLevel(task store.Task, repo config.RepoConfig) (store.Task, error) {
	kids, err := o.childTasks(task.ID, task.Repo)
	if err != nil {
		return task, err
	}
	if len(kids) > 0 {
		return o.move(task, state.NeedsHuman, map[string]any{
			"reason":        "l_split",
			"child_task_id": kids[len(kids)-1].ID,
		}, nil)
	}

	splitter := o.splitter
	if splitter == nil && o.model != nil {
		splitter = &planning.ModelSplitter{
			Client: o.model, Model: o.modelFor("planner"), MaxTokens: o.maxTokens,
			OnUsage: func(role string, u llm.Usage) {
				_ = o.recordUsage(task.ID, role, o.modelFor(role), u)
			},
		}
	}
	fail := func(cause error) (store.Task, error) {
		payload := map[string]any{"reason": "拆分失败"}
		if cause != nil {
			payload["error"] = delivery.RedactText(cause.Error())
		}
		return o.move(task, state.NeedsHuman, payload, nil)
	}
	if splitter == nil {
		return fail(fmt.Errorf("没有可用的拆分器"))
	}
	sp, err := splitter.Split(context.Background(), planning.Requirement{
		TaskID: task.ID, Source: task.Source, Text: task.Requirement, VerifyNames: verifyNames(repo),
	})
	if err != nil {
		return fail(err)
	}
	if err := planning.ValidateSplit(sp, nameSet(verifyNames(repo))); err != nil {
		return fail(err)
	}
	raw, err := json.Marshal(sp)
	if err != nil {
		return task, err
	}
	child, err := o.parkSlice(task, repo, sp.Slices[0], 0)
	if err != nil {
		return task, err
	}
	parent, err := o.move(task, state.NeedsHuman, map[string]any{
		"reason":        "l_split",
		"child_task_id": child.ID,
		"slices":        len(sp.Slices),
	}, map[string]any{"plan_json": string(raw)})
	if err != nil {
		return task, err
	}
	if err := o.store.AppendEvent(parent.ID, "l_split", map[string]any{
		"reason":        "l_split",
		"child_task_id": child.ID,
		"slices":        len(sp.Slices),
	}); err != nil {
		return parent, err
	}
	return parent, nil
}

func (o *Orchestrator) parkSlice(parent store.Task, repo config.RepoConfig, sl planning.Slice, index int) (store.Task, error) {
	child, err := o.store.CreateTask(store.NewTask{
		Repo:          parent.Repo,
		Requirement:   sl.Summary,
		BudgetUSD:     parent.BudgetUSD,
		Level:         triage.M.String(),
		LevelOverride: true,
		Title:         parent.Title,
		Source:        parent.Source,
		SourceRef:     parent.SourceRef,
		ParentTaskID:  parent.ID,
	})
	if err != nil {
		return store.Task{}, err
	}
	info, err := o.ensureWorktree(child, repo)
	if err != nil {
		return store.Task{}, err
	}
	plan := planning.Plan{
		Status:     planning.StatusReady,
		Summary:    sl.Summary,
		Scope:      planning.Scope{AllowedPaths: sl.AllowedPaths},
		Acceptance: sl.Acceptance,
		SliceIndex: index,
		Steps: []planning.Step{{
			ID:          fmt.Sprintf("slice-%d", index+1),
			Description: sl.Summary,
			Files:       sl.AllowedPaths,
		}},
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return store.Task{}, err
	}
	child, err = o.move(child, state.Triaging, map[string]any{"level": triage.M.String()}, nil)
	if err != nil {
		return store.Task{}, err
	}
	child, err = o.move(child, state.Planning, nil, nil)
	if err != nil {
		return store.Task{}, err
	}
	return o.move(child, state.AwaitingApproval, map[string]any{
		"reason":  "M 级任务需要人工审批后再执行",
		"summary": sl.Summary,
		"slice":   index,
	}, map[string]any{
		"plan_json":     string(raw),
		"branch":        info.Branch,
		"base_sha":      info.BaseSHA,
		"worktree_path": filepath.ToSlash(info.Path),
	})
}

func (o *Orchestrator) advanceSplit(child store.Task) error {
	if child.ParentTaskID == "" {
		return nil
	}
	parent, err := o.store.GetTask(child.ParentTaskID)
	if err != nil {
		return err
	}
	sp, err := planning.ParseSplit(parent.PlanJSON)
	if err != nil {
		return o.store.AppendEvent(parent.ID, "l_split_stopped", map[string]any{
			"reason": "拆分计划无法读取", "error": err.Error(),
		})
	}
	plan, err := planning.Parse(child.PlanJSON)
	if err != nil {
		return err
	}
	next := plan.SliceIndex + 1
	if next >= len(sp.Slices) {
		return nil
	}
	kids, err := o.childTasks(parent.ID, parent.Repo)
	if err != nil {
		return err
	}
	for _, kid := range kids {
		if kid.Status == state.Rejected || kid.Status == state.Cancelled {
			return o.store.AppendEvent(parent.ID, "l_split_stopped", map[string]any{
				"reason":        "上一段已结束，不再创建后续段",
				"child_task_id": kid.ID,
			})
		}
		kp, perr := planning.Parse(kid.PlanJSON)
		if perr == nil && kp.SliceIndex == next {
			return nil
		}
	}
	repo, ok := o.cfg.Repos[parent.Repo]
	if !ok {
		return fmt.Errorf("仓库未在 repos.yaml 中配置: %s", parent.Repo)
	}
	created, err := o.parkSlice(parent, repo, sp.Slices[next], next)
	if err != nil {
		return err
	}
	return o.store.AppendEvent(parent.ID, "l_split", map[string]any{
		"reason":        "l_split",
		"child_task_id": created.ID,
		"slices":        len(sp.Slices),
		"slice":         next,
	})
}

func (o *Orchestrator) childTasks(parentID, repoName string) ([]store.Task, error) {
	all, err := o.store.ListTasks("", repoName)
	if err != nil {
		return nil, err
	}
	var out []store.Task
	for _, task := range all {
		if task.ParentTaskID == parentID {
			out = append(out, task)
		}
	}
	return out, nil
}
