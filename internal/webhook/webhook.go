// Package webhook 接收 GitHub Webhook。它只接入带 agent-ready 的 Issue，
// 或把斜线评论交给 issuegate。它不调用 run，也不批准标签。
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentdev/internal/config"
	"agentdev/internal/issuegate"
	"agentdev/internal/state"
	"agentdev/internal/store"
)

const maxBody = 1 << 20

// Handler 校验签名后处理一条投递。Allow 判断评论作者是不是协作者。
type Handler struct {
	Store  *store.SQLiteStore
	Repos  map[string]config.RepoConfig
	Secret string
	Allow  func(ctx context.Context, repo, login string) (bool, error)
	Poster issuegate.Commenter
}

type delivery struct {
	Action string `json:"action"`
	Issue  struct {
		Number  int64  `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	} `json:"issue"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Label struct {
		Name string `json:"name"`
	} `json:"label"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// ServeHTTP 只接受带签名的 POST。签名错误不改任务。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只接受 POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "读取请求失败", http.StatusBadRequest)
		return
	}
	if len(body) > maxBody {
		http.Error(w, "请求体过大", http.StatusRequestEntityTooLarge)
		return
	}
	if !validSignature(h.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "签名无效", http.StatusUnauthorized)
		return
	}
	id := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if id == "" {
		http.Error(w, "缺少 X-GitHub-Delivery", http.StatusBadRequest)
		return
	}
	fresh, err := h.Store.ClaimDelivery(id)
	if err != nil {
		http.Error(w, "记录投递失败", http.StatusInternalServerError)
		return
	}
	if !fresh {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "重复投递")
		return
	}
	if err := h.handle(r, body); err != nil {
		_ = h.Store.ForgetDelivery(id)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "已接收")
}

func (h *Handler) handle(r *http.Request, body []byte) error {
	var event delivery
	if err := json.Unmarshal(body, &event); err != nil {
		return fmt.Errorf("解析 Webhook 失败: %w", err)
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "issue_comment":
		return h.onComment(r.Context(), event)
	case "issues":
		return h.onIssues(event)
	default:
		return nil
	}
}

func (h *Handler) onComment(ctx context.Context, event delivery) error {
	if event.Action != "created" {
		return nil
	}
	repo := event.Repository.FullName
	if _, known := h.Repos[repo]; !known {
		return nil
	}
	task, ok, err := h.openTask(repo, event.Issue.Number)
	if err != nil || !ok {
		return err
	}
	if h.Allow == nil {
		return fmt.Errorf("未配置协作者检查")
	}
	allowed, err := h.Allow(ctx, repo, event.Comment.User.Login)
	if err != nil {
		return err
	}
	_, err = issuegate.Sync(ctx, h.Store, task, []issuegate.Comment{{
		ID:           fmt.Sprint(event.Comment.ID),
		Author:       event.Comment.User.Login,
		Body:         event.Comment.Body,
		Collaborator: allowed,
	}}, h.Poster)
	if err != nil && strings.Contains(err.Error(), "没有仓库协作权限") {
		return nil
	}
	return err
}

func (h *Handler) onIssues(event delivery) error {
	if event.Action != "labeled" || event.Label.Name != issuegate.IntakeLabel {
		return nil
	}
	repo := event.Repository.FullName
	repoCfg, known := h.Repos[repo]
	if !known {
		return nil
	}
	if _, ok, err := h.openTask(repo, event.Issue.Number); err != nil || ok {
		return err
	}
	title := strings.TrimSpace(event.Issue.Title)
	requirement := title
	if body := strings.TrimSpace(event.Issue.Body); body != "" {
		requirement += "\n\n" + body
	}
	requirement = strings.TrimSpace(requirement)
	if requirement == "" {
		return nil
	}
	ref := strings.TrimSpace(event.Issue.HTMLURL)
	if ref == "" {
		ref = fmt.Sprintf("%s#%d", repo, event.Issue.Number)
	}
	budget := repoCfg.BudgetUSD
	if budget <= 0 {
		budget = config.DefaultBudgetUSD
	}
	_, err := h.Store.CreateTask(store.NewTask{
		Repo:        repo,
		Title:       title,
		Requirement: requirement,
		BudgetUSD:   budget,
		Source:      "issue",
		SourceRef:   ref,
	})
	return err
}

func (h *Handler) openTask(repo string, number int64) (store.Task, bool, error) {
	tasks, err := h.Store.ListTasks("", repo)
	if err != nil {
		return store.Task{}, false, err
	}
	for _, task := range tasks {
		if task.Source != "issue" || state.Terminal[task.Status] {
			continue
		}
		slug, n, ok := issuegate.ParseIssueRef(task.SourceRef)
		if ok && slug == repo && n == number {
			return task, true, nil
		}
	}
	return store.Task{}, false, nil
}

func validSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}
