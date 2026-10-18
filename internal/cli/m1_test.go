package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentdev/internal/testutil"
)

func writeScript(t *testing.T, home string, name string, steps ...[]map[string]any) string {
	t.Helper()
	data, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func goodStep() []map[string]any {
	return []map[string]any{{"tool": "write_file", "args": map[string]any{"path": "hello.txt", "content": "hello\n"}}}
}

func badStep() []map[string]any {
	return []map[string]any{{"tool": "write_file", "args": map[string]any{"path": "hello.txt", "content": "bye\n"}}}
}

func createTask(t *testing.T, home string, extra ...string) string {
	t.Helper()
	args := append([]string{"task", "create", "--repo", "acme/app", "--text", "创建 hello.txt"}, extra...)
	args = append(args, "--home", home)
	code, out, errOut := invoke(t, args...)
	if code != ExitOK {
		t.Fatalf("创建任务失败 %d: %s %s", code, out, errOut)
	}
	id := idPattern.FindString(out)
	if id == "" {
		t.Fatalf("输出中找不到任务 ID: %s", out)
	}
	return id
}

func TestTaskCreateLevelDefaultsToAuto(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	createTask(t, home)
	code, out, _ := invoke(t, "task", "list", "--home", home)
	if code != ExitOK {
		t.Fatalf("list 失败: %d", code)
	}
	// 未执行前级别为空，列表中显示占位符。
	if !strings.Contains(out, "-") {
		t.Errorf("未分级任务应显示占位符: %s", out)
	}

	code, out, _ = invoke(t, "task", "create", "--repo", "acme/app", "--text", "x",
		"--level", "m", "--home", home)
	if code != ExitOK || !strings.Contains(out, "M 级（指定）") {
		t.Errorf("显式指定级别应被记录 (%d): %s", code, out)
	}
	code, _, _ = invoke(t, "task", "create", "--repo", "acme/app", "--text", "x",
		"--level", "X", "--home", home)
	if code != ExitUsage {
		t.Errorf("非法级别应为用法错误，实际 %d", code)
	}
}

func TestMLevelTaskRequiresApprovalThenRuns(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := createTask(t, home, "--level", "M")
	script := writeScript(t, home, "good.json", goodStep())

	code, out, _ := invoke(t, "run", "--backend", "fake", "--script", script, "--task", taskID, "--home", home)
	if code != ExitHuman || !strings.Contains(out, "AWAITING_APPROVAL") {
		t.Fatalf("M 级应先等待审批 (%d): %s", code, out)
	}

	code, out, errOut := invoke(t, "task", "approve", taskID, "--by", "张三", "--home", home)
	if code != ExitOK {
		t.Fatalf("审批失败 %d: %s", code, errOut)
	}
	if !strings.Contains(out, "已批准") {
		t.Errorf("审批输出异常: %s", out)
	}

	code, out, _ = invoke(t, "run", "--backend", "fake", "--script", script, "--task", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "DONE") {
		t.Fatalf("审批后应完成 (%d): %s", code, out)
	}
}

func TestApproveRejectsWrongState(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := createTask(t, home)
	code, _, errOut := invoke(t, "task", "approve", taskID, "--home", home)
	if code != ExitUsage || !strings.Contains(errOut, "QUEUED") {
		t.Errorf("非审批状态应报错 (%d): %s", code, errOut)
	}
}

func TestResumeAfterNeedsHuman(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := createTask(t, home)
	bad := writeScript(t, home, "bad.json", badStep(), badStep(), badStep())

	code, _, _ := invoke(t, "run", "--backend", "fake", "--script", bad, "--task", taskID, "--home", home)
	if code != ExitHuman {
		t.Fatalf("应需要人工介入，实际 %d", code)
	}

	code, out, errOut := invoke(t, "task", "resume", taskID, "--home", home)
	if code != ExitOK {
		t.Fatalf("恢复失败 %d: %s", code, errOut)
	}
	if !strings.Contains(out, "VERIFYING") {
		t.Errorf("应恢复到挂起前的 VERIFYING: %s", out)
	}
	if !strings.Contains(out, "修复次数已重置") {
		t.Errorf("恢复应重置修复次数: %s", out)
	}

	good := writeScript(t, home, "good.json", goodStep())
	code, out, _ = invoke(t, "run", "--backend", "fake", "--script", good, "--task", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "DONE") {
		t.Fatalf("恢复后应完成 (%d): %s", code, out)
	}
}

func TestResumeRejectsNonSuspendedTask(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := createTask(t, home)
	code, _, _ := invoke(t, "task", "resume", taskID, "--home", home)
	if code != ExitUsage {
		t.Errorf("QUEUED 任务不应可恢复，实际 %d", code)
	}
}

func TestCancelStopsTaskAndFreesQueue(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := createTask(t, home)

	code, out, errOut := invoke(t, "task", "cancel", taskID, "--reason", "需求作废", "--home", home)
	if code != ExitOK {
		t.Fatalf("取消失败 %d: %s", code, errOut)
	}
	if !strings.Contains(out, "CANCELLED") {
		t.Errorf("取消输出异常: %s", out)
	}

	code, out, _ = invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "CANCELLED") {
		t.Errorf("状态应为 CANCELLED (%d): %s", code, out)
	}

	// 终态任务不应再被 run 取出。
	code, out, _ = invoke(t, "run", "--backend", "fake", "--script", "unused.json", "--home", home)
	if code != ExitOK || !strings.Contains(out, "没有可推进的任务") {
		t.Errorf("取消后队列应为空 (%d): %s", code, out)
	}
}

func TestCostCommandReportsLedgerAndPricingWarning(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := createTask(t, home)
	script := writeScript(t, home, "good.json", goodStep())
	invoke(t, "run", "--backend", "fake", "--script", script, "--task", taskID, "--home", home)

	code, out, _ := invoke(t, "cost", taskID, "--home", home)
	if code != ExitOK {
		t.Fatalf("cost 失败: %d", code)
	}
	if !strings.Contains(out, "无计费记录") {
		t.Errorf("fake 后端不应产生计费: %s", out)
	}
	if !strings.Contains(out, "单价") {
		t.Errorf("未配置单价时应给出提示: %s", out)
	}
}

func TestReplyOnlyFromNeedsInfo(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := createTask(t, home)
	code, _, _ := invoke(t, "task", "reply", taskID, "--text", "补充", "--home", home)
	if code != ExitUsage {
		t.Errorf("QUEUED 任务不应接受回复，实际 %d", code)
	}
}

func TestRunTaskFlagRejectsUnknownID(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	code, _, errOut := invoke(t, "run", "--backend", "fake", "--task", "t_20260101_abcdef", "--home", home)
	if code != ExitUsage || !strings.Contains(errOut, "不存在") {
		t.Errorf("未知任务应为用法错误 (%d): %s", code, errOut)
	}
}

func TestRunMaxProcessesMultipleTasks(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	createTask(t, home)
	createTask(t, home)
	script := writeScript(t, home, "good.json", goodStep())

	code, out, errOut := invoke(t, "run", "--backend", "fake", "--script", script, "--max", "2", "--home", home)
	if code != ExitOK {
		t.Fatalf("批量执行失败 %d: %s", code, errOut)
	}
	if strings.Count(out, "DONE") != 2 {
		t.Errorf("应完成两个任务: %s", out)
	}

	code, out, _ = invoke(t, "run", "--backend", "fake", "--script", script, "--home", home)
	if code != ExitOK || !strings.Contains(out, "没有可推进的任务") {
		t.Errorf("队列应为空 (%d): %s", code, out)
	}
}
