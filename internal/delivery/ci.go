package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// CIState 是 CI 的一次快照结论。
type CIState string

const (
	CIPending CIState = "pending"
	CIPassed  CIState = "passed"
	CIFailed  CIState = "failed"
	// CINone 表示 PR 上没有配置任何检查（不是失败，也不是通过）。
	CINone CIState = "none"
)

// CISnapshot 是一次 CI 检查的结果。
type CISnapshot struct {
	State   CIState
	Summary string
	// Logs 是失败时截断的日志，用于回传给编码步骤。
	Logs string
	// Checks 是检查项数量，用于区分“没有 CI”和“还没开始跑”。
	Checks int
}

// CIRequest 描述一次 CI 查询。
type CIRequest struct {
	Worktree string
	PRNumber int64
	Head     string
}

// CIWatcher 查询 PR 的 CI 状态。实现不得阻塞等待轮询，只返回当前快照。
type CIWatcher interface {
	Name() string
	Checks(ctx context.Context, req CIRequest) (CISnapshot, error)
}

// maxCILogChars 限制回传日志的大小，避免把整份 CI 输出塞进模型上下文。
const maxCILogChars = 6000

// GHCli 通过 gh CLI 查询 CI。gh 使用当前用户凭据，本项目不保存 token。
func (g GHCli) Checks(ctx context.Context, req CIRequest) (CISnapshot, error) {
	out, err := g.runGh(ctx, req.Worktree, "pr", "checks", fmt.Sprintf("%d", req.PRNumber),
		"--json", "name,state,bucket,link")
	if err != nil {
		return CISnapshot{}, err
	}
	var checks []struct {
		Name   string `json:"name"`
		State  string `json:"state"`
		Bucket string `json:"bucket"`
		Link   string `json:"link"`
	}
	if strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(out), &checks); err != nil {
			return CISnapshot{}, fmt.Errorf("解析 gh pr checks 输出失败: %w", err)
		}
	}
	if len(checks) == 0 {
		return CISnapshot{State: CINone, Summary: "PR 上没有 CI 检查"}, nil
	}

	failed, pending, passed := 0, 0, 0
	var failedNames []string
	for _, c := range checks {
		switch strings.ToLower(c.Bucket) {
		case "fail", "cancel":
			failed++
			failedNames = append(failedNames, c.Name)
		case "pending":
			pending++
		case "pass", "skipping":
			passed++
		default:
			// 老版本 gh 可能没有 bucket，退回看 state。
			switch strings.ToUpper(c.State) {
			case "FAILURE", "ERROR", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED":
				failed++
				failedNames = append(failedNames, c.Name)
			case "PENDING", "QUEUED", "IN_PROGRESS", "EXPECTED", "WAITING", "REQUESTED":
				pending++
			case "SUCCESS", "COMPLETED", "NEUTRAL", "SKIPPED":
				passed++
			default:
				// 未知状态绝不能被当成通过；保守地继续等待，最终由超时转人工。
				pending++
			}
		}
	}

	snap := CISnapshot{Checks: len(checks)}
	switch {
	case failed > 0:
		snap.State = CIFailed
		snap.Summary = fmt.Sprintf("%d 项检查失败（%s）", failed, strings.Join(failedNames, ", "))
		snap.Logs = truncateTail(g.failedLogs(ctx, req.Worktree, req.Head), maxCILogChars)
	case pending > 0:
		snap.State = CIPending
		snap.Summary = fmt.Sprintf("%d 项检查仍在运行（通过 %d，失败 %d）", pending, passed, failed)
	default:
		snap.State = CIPassed
		snap.Summary = fmt.Sprintf("%d 项检查全部通过", passed)
	}
	return snap, nil
}

// failedLogs 取该分支最近一次失败运行的日志。拿不到就返回空串：
// 日志只是修复的辅助信息，不应让 CI 判定本身失败。
func (g GHCli) failedLogs(ctx context.Context, worktree, head string) string {
	if head == "" {
		return ""
	}
	out, err := g.runGh(ctx, worktree, "run", "list", "--branch", head, "--limit", "5",
		"--json", "databaseId,conclusion,workflowName")
	if err != nil {
		return ""
	}
	var runs []struct {
		ID       int64  `json:"databaseId"`
		Result   string `json:"conclusion"`
		Workflow string `json:"workflowName"`
	}
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		return ""
	}
	for _, r := range runs {
		if !strings.EqualFold(r.Result, "failure") {
			continue
		}
		logs, err := g.runGh(ctx, worktree, "run", "view", fmt.Sprintf("%d", r.ID), "--log-failed")
		if err != nil {
			continue
		}
		return fmt.Sprintf("workflow %s 失败，日志（尾部）:\n%s", r.Workflow, logs)
	}
	return ""
}

func (g GHCli) runGh(ctx context.Context, worktree string, args ...string) (string, error) {
	stdout, stderr, err := g.run(ctx, worktree, args, "")
	if err != nil {
		if isNoChecksMessage(stderr) || isNoChecksMessage(stdout) {
			return "[]", nil
		}
		var exitErr *exec.ExitError
		// gh pr checks 在存在失败检查时以非零码退出，但 stdout 仍是完整 JSON，
		// 因此只要有输出就交给调用方解析。
		if errors.As(err, &exitErr) && stdout != "" {
			return stdout, nil
		}
		return "", fmt.Errorf("gh %s 失败: %s", strings.Join(args, " "), strings.TrimSpace(stderr))
	}
	return stdout, nil
}

func isNoChecksMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "no checks reported") ||
		strings.Contains(lower, "no checks found") ||
		strings.Contains(lower, "no status checks")
}

// truncateTail 保留尾部内容：CI 日志的关键错误通常在最后。
func truncateTail(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return "…（前部已截断）\n" + string(r[len(r)-limit:])
}

// FakeCI 供测试使用。Snapshots 按调用顺序消费，耗尽后重复最后一个。
type FakeCI struct {
	Snapshots []CISnapshot
	Err       error
	Calls     int
}

func (f *FakeCI) Name() string { return "fake-ci" }

func (f *FakeCI) Checks(_ context.Context, _ CIRequest) (CISnapshot, error) {
	if f.Err != nil {
		return CISnapshot{}, f.Err
	}
	if len(f.Snapshots) == 0 {
		return CISnapshot{State: CINone, Summary: "无 CI"}, nil
	}
	idx := f.Calls
	if idx >= len(f.Snapshots) {
		idx = len(f.Snapshots) - 1
	}
	f.Calls++
	return f.Snapshots[idx], nil
}
