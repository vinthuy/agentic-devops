package cli

import (
	"strings"
	"testing"

	"agentdev/internal/testutil"
)

// 审批者在 `task status` 里必须看得到计划，而不是只有需求一句话。
// 计划此前是只写不读的数据：门能拒能放，但看不到在批什么。
func TestTaskStatusShowsPlanAtApprovalGate(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := parkAtApproval(t, home)

	code, out, errOut := invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK {
		t.Fatalf("status 失败 (%d): %s", code, errOut)
	}
	if !strings.Contains(out, "计划:") {
		t.Fatalf("审批时应在 status 里展示计划: %s", out)
	}
	// 至少要有摘要（静态计划也会带 summary 或步骤）。
	if !strings.Contains(out, "摘要:") && !strings.Contains(out, "步骤:") {
		t.Errorf("计划渲染缺少摘要/步骤: %s", out)
	}
	// 门上的任务要给出下一步命令，否则人看到了状态也不知道怎么继续。
	if !strings.Contains(out, "task approve") || !strings.Contains(out, "task reject") {
		t.Errorf("审批门状态应给出批准/驳回命令: %s", out)
	}
}

// 计划要改验证配置文件时，必须在这条输出里直接点出来。
// 这是审批最该拦的事：改测试会让验证更容易通过。
func TestTaskStatusWarnsWhenPlanEditsTests(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	task := createTask(t, home, "--level", "M")

	// 构造一份「要改测试文件」的计划，模拟规划器想改测试让验证通过。
	plan := `{"status":"ready","summary":"放宽断言让测试通过",
	  "scope":{"allowed_paths":["**"]},
	  "steps":[{"id":"s1","description":"改测试","files":["check.go"]}],
	  "acceptance":[{"id":"ac1","description":"测试通过",
	    "check":{"type":"command","name":"check"}}]}`
	applyPlanJSON(t, home, task, plan)

	code, out, errOut := invoke(t, "task", "status", task, "--home", home)
	if code != ExitOK {
		t.Fatalf("status 失败 (%d): %s", code, errOut)
	}
	// 该仓库的 verify_config_files 是 check.go。
	if !strings.Contains(out, "验证配置文件") {
		t.Errorf("应标出计划改动了验证配置文件: %s", out)
	}
	if !strings.Contains(out, "放宽断言让测试通过") {
		t.Errorf("应展示计划摘要: %s", out)
	}
}

// 没有计划的任务（例如刚创建）不应渲染出空的计划章节。
func TestTaskStatusOmitsPlanWhenAbsent(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	taskID := createTask(t, home)
	code, out, _ := invoke(t, "task", "status", taskID, "--home", home)
	if code != ExitOK {
		t.Fatalf("status 失败 (%d)", code)
	}
	if strings.Contains(out, "计划:") {
		t.Errorf("没有计划时不应出现计划章节: %s", out)
	}
}
