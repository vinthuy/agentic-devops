package cli

import (
	"strings"
	"testing"

	"agentdev/internal/testutil"
)

// parkAtApproval 造一个停在审批门的 M 级任务（不推进到编码）。
func parkAtApproval(t *testing.T, home string) string {
	t.Helper()
	taskID := createTask(t, home, "--level", "M")
	script := writeScript(t, home, "good.json", goodStep())
	code, out, errOut := invoke(t, "run", "--backend", "fake", "--script", script, "--task", taskID, "--home", home)
	if code != ExitHuman || !strings.Contains(out, "AWAITING_APPROVAL") {
		t.Fatalf("M 级任务应停在审批门 (%d): out=%s err=%s", code, out, errOut)
	}
	return taskID
}

// 审批门此前是单行道：AwaitingApproval 只有 → CODING 一条出边，人只能放行。
// 驳回必须是可达的，否则「审批」退化成「通知」。
func TestRejectEndsTaskWithoutCoding(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := parkAtApproval(t, home)

	code, out, errOut := invoke(t, "task", "reject", taskID,
		"--reason", "这个改法会动到公开 API", "--by", "leo", "--home", home)
	if code != ExitOK {
		t.Fatalf("驳回失败 %d: %s", code, errOut)
	}
	if !strings.Contains(out, "REJECTED") {
		t.Errorf("驳回后应为 REJECTED: %s", out)
	}

	code, out, _ = invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "REJECTED") {
		t.Errorf("状态应为 REJECTED (%d): %s", code, out)
	}
	// 理由要说得出、且能区分「谁否的」。
	if !strings.Contains(out, "这个改法会动到公开 API") || !strings.Contains(out, "leo") {
		t.Errorf("驳回理由与驳回人应可审计: %s", out)
	}
	// 被驳回的任务不应再被 run 取出（终态）。
	script := writeScript(t, home, "unused.json", goodStep())
	code, out, _ = invoke(t, "run", "--backend", "fake", "--script", script, "--home", home)
	if code != ExitOK || !strings.Contains(out, "没有可推进的任务") {
		t.Errorf("驳回后队列应为空 (%d): %s", code, out)
	}
}

// 驳回不给理由直接拒绝：没有理由既无法审计，也无法在 --replan 时告诉规划器哪里不行。
func TestRejectRequiresReason(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := parkAtApproval(t, home)

	code, _, errOut := invoke(t, "task", "reject", taskID, "--home", home)
	if code != ExitUsage || !strings.Contains(errOut, "理由") {
		t.Errorf("无理由的驳回应为用法错误 (%d): %s", code, errOut)
	}
	// 失败的驳回不应改状态。
	code, out, _ := invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "AWAITING_APPROVAL") {
		t.Errorf("驳回失败后状态应不变 (%d): %s", code, out)
	}
}

// --replan 把「计划不行」和「任务不要了」分开：退回 PLANNING 重出计划，
// 理由并入需求，规划器下一轮能看到。
func TestRejectReplanReturnsToPlanningWithReason(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := parkAtApproval(t, home)

	code, out, errOut := invoke(t, "task", "reject", taskID, "--replan",
		"--reason", "计划没有覆盖过期边界，补充 ttl=0 的情况", "--home", home)
	if code != ExitOK {
		t.Fatalf("驳回失败 %d: %s", code, errOut)
	}
	if !strings.Contains(out, "PLANNING") {
		t.Errorf("--replan 应退回 PLANNING: %s", out)
	}

	code, out, _ = invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "PLANNING") {
		t.Errorf("状态应为 PLANNING (%d): %s", code, out)
	}
	if !strings.Contains(out, "计划没有覆盖过期边界") {
		t.Errorf("驳回理由应进入需求上下文: %s", out)
	}
}

// 已进入编码的任务不能再被驳回：驳回的是计划，不是已经落地的改动。
func TestRejectRejectedAfterCoding(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := parkAtApproval(t, home)
	code, _, errOut := invoke(t, "task", "approve", taskID, "--home", home)
	if code != ExitOK {
		t.Fatalf("批准失败 %d: %s", code, errOut)
	}
	code, _, _ = invoke(t, "task", "reject", taskID, "--reason", "不要了", "--home", home)
	if code != ExitUsage {
		t.Errorf("已批准（CODING）的任务不应可驳回，实际 %d", code)
	}
}

// 驳回后回退规划，重新跑完必须**再次停在审批门**——M 级不能因为「重规划过一次」
// 就自动放行，否则驳回一次等于把审批门拆了。
func TestReplanAfterRejectParksAgain(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := parkAtApproval(t, home)

	if code, _, errOut := invoke(t, "task", "reject", taskID, "--replan",
		"--reason", "补充边界条件", "--home", home); code != ExitOK {
		t.Fatalf("驳回失败: %s", errOut)
	}

	script := writeScript(t, home, "good2.json", goodStep())
	code, out, errOut := invoke(t, "run", "--backend", "fake", "--script", script, "--task", taskID, "--home", home)
	if code != ExitHuman {
		t.Fatalf("重规划后应再次停在审批门 (%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "AWAITING_APPROVAL") {
		t.Errorf("重规划后应为 AWAITING_APPROVAL: %s", out)
	}
	code, out, _ = invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "AWAITING_APPROVAL") {
		t.Errorf("重规划后应再次停在审批门 (%d): %s", code, out)
	}
}
