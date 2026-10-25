package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentdev/internal/testutil"

	_ "modernc.org/sqlite"
)

// backdateTask 把任务的 updated_at 改到过去，用来模拟「已经等了一段时间」。
// 直接改库是有意的：否则这条测试要么等真实时间，要么把时钟注入进 CLI。
func backdateTask(t *testing.T, home, taskID string, ago time.Duration) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ts := time.Now().Add(-ago).UTC().Format(time.RFC3339)
	if _, err := db.Exec("UPDATE tasks SET updated_at = ? WHERE id = ?", ts, taskID); err != nil {
		t.Fatal(err)
	}
}

func writePolicy(t *testing.T, home, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "policy.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 审批超时后任务升级为 NEEDS_HUMAN，且提示里给出回退路径。
// 自动放行是绝对不行的：那等于把审批门拆掉。
func TestRunEscalatesStaleApproval(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	writePolicy(t, home, "approval_timeout_minutes: 60\n")
	taskID := parkAtApproval(t, home)

	backdateTask(t, home, taskID, 2*time.Hour)
	script := writeScript(t, home, "unused.json", goodStep())
	code, out, errOut := invoke(t, "run", "--backend", "fake", "--script", script, "--home", home)
	if code != ExitOK {
		t.Fatalf("run 失败 (%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "审批超时") || !strings.Contains(out, "NEEDS_HUMAN") {
		t.Errorf("应提示审批超时升级: %s", out)
	}

	code, out, _ = invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "NEEDS_HUMAN") {
		t.Errorf("应升级到 NEEDS_HUMAN (%d): %s", code, out)
	}
	// 升级不等于放行：绝不能进入编码。
	if strings.Contains(out, "CODING") {
		t.Errorf("超时不应自动放行进入编码: %s", out)
	}
}

// 未超时的任务必须原样留在审批门。
func TestRunDoesNotEscalateFreshApproval(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	writePolicy(t, home, "approval_timeout_minutes: 60\n")
	taskID := parkAtApproval(t, home)

	backdateTask(t, home, taskID, 5*time.Minute)
	script := writeScript(t, home, "unused.json", goodStep())
	code, out, _ := invoke(t, "run", "--backend", "fake", "--script", script, "--home", home)
	if code != ExitOK {
		t.Fatalf("run 失败 (%d): %s", code, out)
	}
	if strings.Contains(out, "审批超时") {
		t.Errorf("未超时不应升级: %s", out)
	}
	code, out, _ = invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "AWAITING_APPROVAL") {
		t.Errorf("应仍在审批门 (%d): %s", code, out)
	}
}

// 默认不超时：不写 approval_timeout_minutes 时，等再久也不升级。
func TestRunKeepsApprovalWhenTimeoutUnset(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := parkAtApproval(t, home)

	backdateTask(t, home, taskID, 30*24*time.Hour)
	script := writeScript(t, home, "unused.json", goodStep())
	if code, out, _ := invoke(t, "run", "--backend", "fake", "--script", script, "--home", home); code != ExitOK {
		t.Fatalf("run 失败 (%d): %s", code, out)
	}
	code, out, _ := invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(out, "AWAITING_APPROVAL") {
		t.Errorf("默认配置下应一直等待 (%d): %s", code, out)
	}
}

// 升级后 resume 必须能把任务送回审批门，而不是无路可走。
func TestResumeAfterApprovalTimeoutReturnsToGate(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	writePolicy(t, home, "approval_timeout_minutes: 60\n")
	taskID := parkAtApproval(t, home)
	backdateTask(t, home, taskID, 2*time.Hour)

	script := writeScript(t, home, "unused.json", goodStep())
	invoke(t, "run", "--backend", "fake", "--script", script, "--home", home)

	code, out, errOut := invoke(t, "task", "resume", taskID, "--home", home)
	if code != ExitOK {
		t.Fatalf("resume 失败 (%d): %s", code, errOut)
	}
	if !strings.Contains(out, "AWAITING_APPROVAL") {
		t.Errorf("resume 应回到审批门: %s", out)
	}
}
