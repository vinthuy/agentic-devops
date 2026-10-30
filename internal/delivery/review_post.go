package delivery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agentdev/internal/review"
)

const (
	// ReviewEventComment 是 GitHub review 的 COMMENT：可见，但不请求修改。
	ReviewEventComment = "COMMENT"
	// ReviewEventRequestChanges 是 GitHub review 的 REQUEST_CHANGES。
	ReviewEventRequestChanges = "REQUEST_CHANGES"
)

// PullReviewComment 是草稿 PR 上的一条审查评论，对应一条阻塞意见。
type PullReviewComment struct {
	Path string
	Line int
	Body string
}

// PullReview 是已经决定要发表的 PR review。Event 只能是评论或请求修改。
type PullReview struct {
	Event    string
	Body     string
	Comments []PullReviewComment
}

// ReviewPoster 在本地结论已经生效之后把 review 发表到草稿 PR。
type ReviewPoster interface {
	PostReview(ctx context.Context, worktree string, number int64, review PullReview) error
}

// BuildPullReview 把已校验的本地结论映射成 PR review。
// 通过只发表评论。打回只为 high/medium 阻塞意见各建一条评论。
func BuildPullReview(v review.Review) PullReview {
	if v.Approved() {
		return PullReview{Event: ReviewEventComment, Body: reviewBody(v)}
	}
	comments := make([]PullReviewComment, 0, len(v.Blockers()))
	for _, f := range v.Blockers() {
		comments = append(comments, PullReviewComment{
			Path: f.Path,
			Line: f.Line,
			Body: fmt.Sprintf("[%s] %s", f.Severity, f.Comment),
		})
	}
	return PullReview{Event: ReviewEventRequestChanges, Body: reviewBody(v), Comments: comments}
}

func reviewBody(v review.Review) string {
	body := strings.TrimSpace(v.Summary)
	if body == "" {
		body = "审查结论"
	}
	return body
}

// PostReview 用 gh 在已有 PR 上发表 review。调用方必须已经完成本地状态转移。
func (g GHCli) PostReview(ctx context.Context, worktree string, number int64, rv PullReview) error {
	if number <= 0 {
		return fmt.Errorf("PR 编号无效")
	}
	if strings.TrimSpace(worktree) == "" {
		return fmt.Errorf("缺少工作区，无法发表 PR review")
	}
	stdout, stderr, err := g.run(ctx, worktree, []string{
		"repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner",
	}, "")
	if err != nil {
		return fmt.Errorf("读取仓库名失败: %s", strings.TrimSpace(stderr))
	}
	slug := strings.Trim(strings.TrimSpace(stdout), `"`)
	if !validRepoSlug(slug) {
		return fmt.Errorf("仓库名不是 owner/name: %q", slug)
	}
	payload, err := json.Marshal(apiReview(rv))
	if err != nil {
		return err
	}
	_, stderr, err = g.run(ctx, worktree, []string{
		"api", "--method", "POST",
		fmt.Sprintf("repos/%s/pulls/%d/reviews", slug, number),
		"--input", "-",
	}, string(payload))
	if err != nil {
		return fmt.Errorf("发表 PR review 失败: %s", strings.TrimSpace(stderr))
	}
	return nil
}

type apiPullReview struct {
	Body     string           `json:"body"`
	Event    string           `json:"event"`
	Comments []apiPullComment `json:"comments,omitempty"`
}

type apiPullComment struct {
	Path        string `json:"path"`
	Body        string `json:"body"`
	Line        int    `json:"line,omitempty"`
	Side        string `json:"side,omitempty"`
	SubjectType string `json:"subject_type"`
}

func apiReview(rv PullReview) apiPullReview {
	out := apiPullReview{Body: rv.Body, Event: rv.Event}
	for _, c := range rv.Comments {
		item := apiPullComment{Path: c.Path, Body: c.Body, SubjectType: "file"}
		if c.Line > 0 {
			item.Line = c.Line
			item.Side = "RIGHT"
			item.SubjectType = "line"
		}
		out.Comments = append(out.Comments, item)
	}
	return out
}

func validRepoSlug(repo string) bool {
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
