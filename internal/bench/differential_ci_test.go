package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/state"
	"agentdev/internal/store"
)

func TestDifferentialWorkflowHasACIOnlyCheck(t *testing.T) {
	root := filepath.Join("..", "..", "bench")
	manifest, err := Load(filepath.Join(root, "ci_loop.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := os.ReadFile(filepath.Join(root, "ci_loop_workflow.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var local []string
	for _, c := range manifest.Cases {
		for _, cmd := range c.Verify {
			local = append(local, cmd)
		}
	}
	checks := ciOnlyChecks(string(workflow), local)
	if len(checks) == 0 {
		t.Fatal("差异基准的 workflow 必须包含一条本地验证不执行的检查")
	}
	for _, cmd := range local {
		if strings.Contains(cmd, checks[0]) {
			t.Fatalf("本地验证命令不应执行 %s: %s", checks[0], cmd)
		}
	}
}

func TestLocalPassAndCIOnlyFailureIsNotFirstPass(t *testing.T) {
	failure := "AssertionError: ValueError not raised"
	events := []store.Event{
		{Type: "ci_failed", Payload: map[string]any{"summary": "1 项检查失败（test）"}},
		{Type: "fix_requested", Payload: map[string]any{"source": "ci", "failure": failure}},
	}
	if ciFirstPassAllowed("passed", 0, events) {
		t.Fatal("本地通过且 workflow 额外检查失败时，不得记为 CI 首次通过")
	}
	if !ciFirstPassAllowed("passed", 0, nil) {
		t.Fatal("没有 CI 失败记录的通过仍应算首次通过")
	}
	got, _ := events[1].Payload["failure"].(string)
	if !strings.Contains(got, "ValueError not raised") {
		t.Fatal("现有 CI 修复流程应能读到这条失败")
	}
	same := []store.Event{{
		Type: "state_changed", ToStatus: string(state.Done),
		Payload: map[string]any{"ci": "passed"},
	}}
	if ciVerdict(same) != "passed" || !ciFirstPassAllowed("passed", 0, same) {
		t.Fatal("同一批命令的既有通过记录不应被改写")
	}
	if len(ciOnlyChecks(defaultCIWorkflow, []string{
		"go test ./...", "node --test", "python -m unittest -q",
	})) != 0 {
		t.Fatal("与本地相同的 workflow 不应多出一条检查")
	}
}
