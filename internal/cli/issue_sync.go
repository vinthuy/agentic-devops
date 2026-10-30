package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"agentdev/internal/config"
	"agentdev/internal/issuegate"
	"agentdev/internal/store"
)

// ghIssueClient 用 gh 读取评论、核对协作者，并发表回写。
type ghIssueClient struct{}

func (ghIssueClient) Post(ctx context.Context, repo, _ string, number int64, body string) (string, error) {
	if !validRepoSlug(repo) || number <= 0 {
		return "", fmt.Errorf("无法回写 Issue: 仓库 %q 或编号 %d 无效", repo, number)
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "--method", "POST",
		fmt.Sprintf("repos/%s/issues/%d/comments", repo, number), "--input", "-")
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("发表 Issue 评论失败: %s", strings.TrimSpace(stderr.String()))
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &resp); err != nil {
		return "", fmt.Errorf("解析评论响应失败: %w", err)
	}
	if resp.ID == 0 {
		return "", fmt.Errorf("评论响应缺少 id")
	}
	return strconv.FormatInt(resp.ID, 10), nil
}

func (ghIssueClient) List(ctx context.Context, repo string, number int64) ([]issuegate.Comment, error) {
	if !validRepoSlug(repo) || number <= 0 {
		return nil, fmt.Errorf("无法读取评论: 仓库 %q 或编号 %d 无效", repo, number)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api", "--paginate",
		fmt.Sprintf("repos/%s/issues/%d/comments", repo, number))
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("读取 Issue 评论失败: %s", strings.TrimSpace(stderr.String()))
	}
	var raw []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &raw); err != nil {
		return nil, fmt.Errorf("解析 Issue 评论失败: %w", err)
	}
	out := make([]issuegate.Comment, 0, len(raw))
	for _, c := range raw {
		out = append(out, issuegate.Comment{
			ID:     strconv.FormatInt(c.ID, 10),
			Author: c.User.Login,
			Body:   c.Body,
		})
	}
	return out, nil
}

func (ghIssueClient) Collaborator(ctx context.Context, repo, login string) (bool, error) {
	if !validRepoSlug(repo) || login == "" || strings.ContainsAny(login, "/?# \t") {
		return false, fmt.Errorf("无法检查协作权限")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/%s/collaborators/%s", repo, login))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	msg := stderr.String()
	if strings.Contains(msg, "404") {
		return false, nil
	}
	return false, fmt.Errorf("检查协作权限失败: %s", strings.TrimSpace(msg))
}

func postIssueUpdate(ctx context.Context, st store.Repository, cfg *config.App, task store.Task, kind string) error {
	if task.Source != "issue" || cfg == nil {
		return nil
	}
	return issuegate.PostUpdate(ctx, st, ghIssueClient{}, task, cfg.Repos[task.Repo], cfg.Policy, kind)
}

func taskSyncIssueCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "sync-issue <task-id>",
		Short: "读取来源 Issue 上的 /approve、/reject、/replan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, st, task, err := loadTask(cmd, opts)
			if err != nil {
				return err
			}
			defer st.Close()
			if task.Source != "issue" {
				return fail(ExitUsage, "任务 %s 不是来自 Issue", task.ID)
			}
			repo, number, ok := issuegate.ParseIssueRef(task.SourceRef)
			if !ok {
				return fail(ExitUsage, "无法解析来源 Issue: %s", task.SourceRef)
			}
			client := ghIssueClient{}
			comments, err := client.List(cmd.Context(), repo, number)
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			known := map[string]bool{}
			for i := range comments {
				login := comments[i].Author
				allowed, seen := known[login]
				if !seen {
					allowed, err = client.Collaborator(cmd.Context(), repo, login)
					if err != nil {
						return fail(ExitError, "%v", err)
					}
					known[login] = allowed
				}
				comments[i].Collaborator = allowed
			}
			updated, err := issuegate.Sync(cmd.Context(), st, task, comments, client)
			out := cmd.OutOrStdout()
			if updated.ID != "" {
				fmt.Fprintf(out, "[OK] 已处理 %s 的 Issue 评论，状态: %s\n", task.ID, updated.Status)
			}
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			if updated.Status == task.Status && updated.Version == task.Version {
				fmt.Fprintln(out, "没有新的审批动作")
			}
			return nil
		},
	}
}
