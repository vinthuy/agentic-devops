package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/delivery"
	"agentdev/internal/review"
	"agentdev/internal/state"
	"agentdev/internal/store"
)

type captureReviews struct {
	reviews []delivery.PullReview
	err     error
}

func (c *captureReviews) PostReview(_ context.Context, _ string, number int64, rv delivery.PullReview) error {
	c.reviews = append(c.reviews, rv)
	return c.err
}

type prStore struct {
	store.Repository
}

func (p prStore) Transition(id string, version int64, to state.Status, payload, fields map[string]any) (store.Task, error) {
	task, err := p.Repository.Transition(id, version, to, payload, fields)
	if err != nil {
		return task, err
	}
	task.PRNumber = 7
	task.PRURL = "https://github.com/acme/app/pull/7"
	return task, nil
}

func TestNoPullRequestSkipsReviewPublication(t *testing.T) {
	f := setup(t)
	f.cfg.Review.Enabled = true
	task := f.task("S")
	poster := &captureReviews{err: errors.New("should not be called")}
	done, err := New(f.st, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithReviewer(&stubReviewer{reviews: []review.Review{approveReview()}}),
		WithReviewPoster(poster)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("没有 PR 时审查通过仍应完成，实际 %s", done.Status)
	}
	if len(poster.reviews) != 0 {
		t.Fatalf("没有 PR 时不应发表 review: %+v", poster.reviews)
	}
	if len(eventsOfType(t, f.st, task.ID, "review_post_failed")) != 0 {
		t.Fatal("没有 PR 不应记成发表失败")
	}
}

func TestReviewPublicationFailureKeepsLocalDecision(t *testing.T) {
	f := setup(t)
	f.cfg.Review.Enabled = true
	f.cfg.Policy.MaxReviewAttempts = 0
	task := f.task("S")
	poster := &captureReviews{err: errors.New("github 503")}
	done, err := New(prStore{f.st}, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithReviewer(&stubReviewer{reviews: []review.Review{changesReview()}}),
		WithReviewPoster(poster)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.NeedsHuman {
		t.Fatalf("发表失败后应保持打回后的状态，实际 %s", done.Status)
	}
	if len(poster.reviews) != 1 || poster.reviews[0].Event != delivery.ReviewEventRequestChanges {
		t.Fatalf("应在状态转移后请求修改: %+v", poster.reviews)
	}
	events := eventsOfType(t, f.st, task.ID, "review_post_failed")
	if len(events) != 1 {
		t.Fatalf("应记录发表失败，实际 %d", len(events))
	}
	msg, _ := events[0].Payload["error"].(string)
	if !strings.Contains(msg, "github 503") {
		t.Fatalf("失败事件应包含错误原因: %+v", events[0].Payload)
	}
}

func TestApprovedReviewIsPostedAsAComment(t *testing.T) {
	f := setup(t)
	f.cfg.Review.Enabled = true
	task := f.task("S")
	poster := &captureReviews{}
	done, err := New(prStore{f.st}, f.cfg, backend.NewFake([][]backend.ToolCall{good}),
		WithReviewer(&stubReviewer{reviews: []review.Review{approveReview()}}),
		WithReviewPoster(poster)).RunTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != state.Done {
		t.Fatalf("发表成功后应保持通过后的交付结果，实际 %s", done.Status)
	}
	if len(poster.reviews) != 1 || poster.reviews[0].Event != delivery.ReviewEventComment {
		t.Fatalf("通过时应发表评论: %+v", poster.reviews)
	}
	if len(eventsOfType(t, f.st, task.ID, "review_posted")) != 1 {
		t.Fatal("应记录 review_posted")
	}
}
