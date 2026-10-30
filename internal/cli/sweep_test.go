package cli

import (
	"strings"
	"testing"
	"time"

	"agentdev/internal/testutil"
)

func TestSweepWithNothingExpiredExitsZero(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	code, out, errOut := invoke(t, "sweep", "--home", home)
	if code != ExitOK {
		t.Fatalf("没有到期项时退出码应为 0 (%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "没有到期项") {
		t.Fatalf("应说明没有到期项: %s", out)
	}
	_, list, _ := invoke(t, "task", "list", "--home", home)
	if strings.Contains(list, "acme/app") {
		t.Fatalf("sweep 不得创建任务: %s", list)
	}
	code, out, errOut = invoke(t, "sweep", "--home", home)
	if code != ExitOK {
		t.Fatalf("再次执行仍应退出 (%d): out=%s err=%s", code, out, errOut)
	}
}

func TestSweepDoesNotStartQueuedTask(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := createTask(t, home)
	code, out, errOut := invoke(t, "sweep", "--home", home)
	if code != ExitOK {
		t.Fatalf("sweep 失败 (%d): out=%s err=%s", code, out, errOut)
	}
	code, status, _ := invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(status, "QUEUED") {
		t.Fatalf("排队任务应保持 QUEUED (%d): %s", code, status)
	}
}

func TestSweepEscalatesStaleApproval(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	writePolicy(t, home, "approval_timeout_minutes: 60\n")
	taskID := parkAtApproval(t, home)
	backdateTask(t, home, taskID, 2*time.Hour)

	code, out, errOut := invoke(t, "sweep", "--home", home)
	if code != ExitOK {
		t.Fatalf("sweep 失败 (%d): out=%s err=%s", code, out, errOut)
	}
	if !strings.Contains(out, "审批") {
		t.Fatalf("应报告审批到期: %s", out)
	}
	code, status, _ := invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK || !strings.Contains(status, "NEEDS_HUMAN") {
		t.Fatalf("应升级为 NEEDS_HUMAN (%d): %s", code, status)
	}
	if strings.Contains(status, "CODING") {
		t.Fatalf("超时不应进入编码: %s", status)
	}
}
