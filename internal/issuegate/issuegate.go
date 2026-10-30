// Package issuegate 把来源 Issue 上的斜线命令投影到现有审批动作，并回写计划与终态。
//
// 评论正文、标签都不会变成新的需求。批准、驳回、退回规划只走 Approve 与 Reject，
// 与 CLI 共用同一套状态转移，避免两条路径写出不同的审计载荷。
package issuegate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"agentdev/internal/config"
	"agentdev/internal/report"
	"agentdev/internal/state"
	"agentdev/internal/store"
)

const (
	IntakeLabel = "agent-ready"

	EventApplied = "github_command_applied"
	EventIgnored = "github_command_ignored"
	EventPosted  = "github_comment_posted"
	EventFailed  = "github_comment_failed"
)

// ErrReasonRequired 表示驳回或退回规划没有理由。CLI 把它显示为用法错误。
var ErrReasonRequired = errors.New("驳回必须给出理由")

// Comment 是一条已取回的 Issue 评论。Collaborator 由调用方按仓库权限填好。
type Comment struct {
	ID           string
	Author       string
	Body         string
	Collaborator bool
}

// Commenter 发表一条 Issue 评论。测试用替身，正式路径走 gh。
type Commenter interface {
	Post(ctx context.Context, repo, sourceRef string, number int64, body string) (string, error)
}

// LabelResult 只描述标签能做什么。任何标签都不会改变任务状态。
type LabelResult struct {
	Intake bool
	Status state.Status
}

// Approve 把等待审批的任务放行到编码。extra 会并入审计载荷（例如评论 id）。
func Approve(st store.Repository, task store.Task, by string, extra map[string]any, fields map[string]any) (store.Task, error) {
	payload := map[string]any{"approved_by": by}
	for k, v := range extra {
		payload[k] = v
	}
	return st.Transition(task.ID, task.Version, state.Coding, payload, fields)
}

// Reject 驳回计划。replan 为真时带着理由退回规划，否则进入终态 REJECTED。
func Reject(st store.Repository, task store.Task, by, reason string, replan bool, extra map[string]any) (store.Task, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return task, ErrReasonRequired
	}
	if err := st.RecordInteraction(task.ID, "reject", reason); err != nil {
		return task, err
	}
	payload := map[string]any{
		"rejected_by": by,
		"reason":      truncate(reason),
		"replan":      replan,
	}
	for k, v := range extra {
		payload[k] = v
	}
	target := state.Rejected
	fields := map[string]any{}
	if replan {
		target = state.Planning
		fields["requirement"] = task.Requirement + "\n\n[审批驳回]\n" + reason
	}
	return st.Transition(task.ID, task.Version, target, payload, fields)
}

// Apply 处理一条评论。非命令文本直接忽略。已见过的 comment id 不再转移状态。
func Apply(ctx context.Context, st store.Repository, task store.Task, c Comment, poster Commenter) (store.Task, error) {
	cmd, reason, ok := parseSlash(c.Body)
	if !ok {
		return task, nil
	}
	if strings.TrimSpace(c.ID) == "" {
		return task, ignore(st, task, c, "missing_comment_id")
	}
	seen, err := commentSeen(st, task.ID, c.ID)
	if err != nil {
		return task, err
	}
	if seen {
		fresh, gerr := st.GetTask(task.ID)
		if gerr != nil {
			return task, gerr
		}
		return fresh, nil
	}
	if !c.Collaborator {
		return denyCollaborator(ctx, st, task, c, poster)
	}
	if task.Status != state.AwaitingApproval {
		return task, ignore(st, task, c, "wrong_status")
	}
	extra := map[string]any{"comment_id": c.ID, "command": cmd}
	var updated store.Task
	switch cmd {
	case "/approve":
		updated, err = Approve(st, task, c.Author, extra, nil)
	case "/reject", "/replan":
		if reason == "" {
			return task, ignore(st, task, c, "empty_reason")
		}
		updated, err = Reject(st, task, c.Author, reason, cmd == "/replan", extra)
	default:
		return task, ignore(st, task, c, "unknown_command")
	}
	if err != nil {
		return task, err
	}
	if err := st.AppendEvent(task.ID, EventApplied, map[string]any{
		"comment_id": c.ID,
		"command":    cmd,
		"by":         c.Author,
	}); err != nil {
		return updated, err
	}
	return updated, nil
}

// Sync 按顺序处理一批评论。前面的转移会成为后面评论看到的状态。
func Sync(ctx context.Context, st store.Repository, task store.Task, comments []Comment, poster Commenter) (store.Task, error) {
	current := task
	var first error
	for _, c := range comments {
		next, err := Apply(ctx, st, current, c, poster)
		if err != nil && first == nil {
			first = err
		}
		if next.ID != "" {
			current = next
		}
	}
	return current, first
}

// ApplyLabel 说明接入标签的效果。它不创建任务，也不批准、驳回或退回规划。
func ApplyLabel(_ context.Context, _ store.Repository, task store.Task, label string) (LabelResult, error) {
	return LabelResult{Intake: label == IntakeLabel, Status: task.Status}, nil
}

// PostUpdate 回写计划或终态。失败只记事件并返回错误，不改变任务状态。
func PostUpdate(ctx context.Context, st store.Repository, poster Commenter, task store.Task, repo config.RepoConfig, pol config.PolicyConfig, kind string) error {
	if task.Source != "issue" {
		return nil
	}
	if kind != "plan" && kind != "final" {
		return fmt.Errorf("未知的回写类型: %s", kind)
	}
	slug, number, ok := ParseIssueRef(task.SourceRef)
	if !ok || poster == nil {
		return recordPostFailure(st, task.ID, kind, "无法解析来源 Issue 或未配置评论客户端")
	}
	body := planBody(task, repo, pol)
	if kind == "final" {
		body = finalBody(task)
	}
	id, err := poster.Post(ctx, slug, task.SourceRef, number, body)
	if err != nil {
		return recordPostFailure(st, task.ID, kind, err.Error())
	}
	return st.AppendEvent(task.ID, EventPosted, map[string]any{
		"comment_id": id,
		"kind":       kind,
	})
}

// ParseIssueRef 接受 https://github.com/owner/name/issues/N 或 owner/name#N。
func ParseIssueRef(ref string) (string, int64, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", 0, false
	}
	if strings.Contains(ref, "://") {
		u, err := url.Parse(ref)
		if err != nil || !strings.EqualFold(u.Host, "github.com") {
			return "", 0, false
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) != 4 || parts[2] != "issues" {
			return "", 0, false
		}
		n, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || n <= 0 || !validSlug(parts[0]+"/"+parts[1]) {
			return "", 0, false
		}
		return parts[0] + "/" + parts[1], n, true
	}
	slug, numText, ok := strings.Cut(ref, "#")
	if !ok || !validSlug(slug) {
		return "", 0, false
	}
	n, err := strconv.ParseInt(numText, 10, 64)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return slug, n, true
}

func denyCollaborator(ctx context.Context, st store.Repository, task store.Task, c Comment, poster Commenter) (store.Task, error) {
	if poster == nil {
		_ = ignore(st, task, c, "not_collaborator")
		return task, errors.New("没有仓库协作权限，不能审批")
	}
	slug, number, ok := ParseIssueRef(task.SourceRef)
	if !ok {
		_ = ignore(st, task, c, "not_collaborator")
		return task, errors.New("没有仓库协作权限，不能审批")
	}
	_, err := poster.Post(ctx, slug, task.SourceRef, number, "没有仓库协作权限，不能审批。")
	if err != nil {
		_ = ignore(st, task, c, "not_collaborator")
		return task, err
	}
	if err := ignore(st, task, c, "not_collaborator"); err != nil {
		return task, err
	}
	return task, nil
}

func ignore(st store.Repository, task store.Task, c Comment, reason string) error {
	return st.AppendEvent(task.ID, EventIgnored, map[string]any{
		"comment_id": c.ID,
		"command":    firstToken(c.Body),
		"by":         c.Author,
		"reason":     reason,
	})
}

func commentSeen(st store.Repository, taskID, commentID string) (bool, error) {
	events, err := st.ListEvents(taskID)
	if err != nil {
		return false, err
	}
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		if fmt.Sprint(e.Payload["comment_id"]) == commentID {
			return true, nil
		}
	}
	return false, nil
}

func recordPostFailure(st store.Repository, taskID, kind, message string) error {
	_ = st.AppendEvent(taskID, EventFailed, map[string]any{
		"kind":  kind,
		"error": message,
	})
	return errors.New(message)
}

func planBody(task store.Task, repo config.RepoConfig, pol config.PolicyConfig) string {
	lines := report.ReviewPlan(task.PlanJSON, repo, pol).Lines()
	var b strings.Builder
	b.WriteString("计划等待审批。\n")
	if len(lines) == 0 {
		b.WriteString("（计划内容为空）\n")
	} else {
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("\n回复 `/approve` 批准，`/reject 理由` 驳回，`/replan 理由` 退回规划。\n评论里的其他文字不会进入规划或编码。")
	return b.String()
}

func finalBody(task store.Task) string {
	pr := strings.TrimSpace(task.PRURL)
	if pr == "" {
		pr = "（无）"
	}
	return fmt.Sprintf("任务 %s 已结束。\n状态: %s\nPR: %s\nIssue 保持打开。", task.ID, task.Status, pr)
}

func parseSlash(body string) (string, string, bool) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	first, _, _ := strings.Cut(body, "\n")
	fields := strings.Fields(first)
	if len(fields) == 0 {
		return "", "", false
	}
	token := strings.ToLower(fields[0])
	switch token {
	case "/approve", "/reject", "/replan":
		reason := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(first), fields[0]))
		return token, reason, true
	default:
		return "", "", false
	}
}

func firstToken(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	first, _, _ := strings.Cut(body, "\n")
	fields := strings.Fields(first)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(fields[0])
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= 500 {
		return s
	}
	return string(r[:500]) + "..."
}

func validSlug(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, "-") {
			return false
		}
		for _, r := range part {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
				return false
			}
		}
	}
	return true
}
