package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/planning"
	"agentdev/internal/state"
)

type capturePlanner struct {
	req  planning.Requirement
	plan planning.Plan
}

func (c *capturePlanner) Plan(_ context.Context, req planning.Requirement) (planning.Plan, error) {
	c.req = req
	return c.plan, nil
}

func TestAgentsMDReachesThePlanner(t *testing.T) {
	f := setup(t)
	if err := os.WriteFile(filepath.Join(f.repo, "AGENTS.md"), []byte("仓库约定：先写测试。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task := f.task("S")
	planner := &capturePlanner{plan: planning.Plan{
		Status: planning.StatusReady, Summary: "写 hello.txt",
		Acceptance: []planning.Acceptance{{
			ID: "ac1", Description: "hello.txt 存在",
			Check: planning.Check{Type: planning.CheckFileExists, Path: "hello.txt"},
		}},
	}}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}), WithPlanner(planner)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("有 AGENTS.md 时任务应继续，实际 %s", done.Status)
	}
	if !strings.Contains(planner.req.AgentsMD, "先写测试") || !strings.Contains(planner.req.AgentsMD, "AGENTS.md") {
		t.Fatalf("规划上下文应包含仓库文件内容: %q", planner.req.AgentsMD)
	}
}
