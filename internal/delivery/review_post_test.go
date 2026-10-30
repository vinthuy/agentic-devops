package delivery

import (
	"testing"

	"agentdev/internal/review"
)

func TestRequestChangesBecomesAReviewThatAsksForChanges(t *testing.T) {
	got := BuildPullReview(review.Review{
		Decision: review.RequestChanges,
		Summary:  "实现与需求不一致",
		Findings: []review.Finding{
			{Path: "hello.txt", Line: 1, Severity: review.SeverityHigh, Comment: "应返回 None"},
			{Path: "hello.txt", Line: 2, Severity: review.SeverityLow, Comment: "命名可以更清楚"},
			{Path: "tokens.py", Line: 8, Severity: review.SeverityMedium, Comment: "边界应包含等号"},
		},
	})
	if got.Event != ReviewEventRequestChanges {
		t.Fatalf("打回应请求修改，实际 %s", got.Event)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("每条阻塞意见对应一条评论，实际 %d: %+v", len(got.Comments), got.Comments)
	}
	if got.Comments[0].Path != "hello.txt" || got.Comments[1].Path != "tokens.py" {
		t.Fatalf("评论应指向阻塞意见的文件: %+v", got.Comments)
	}
}

func TestApproveBecomesAComment(t *testing.T) {
	got := BuildPullReview(review.Review{
		Decision: review.Approve,
		Summary:  "改动满足需求",
		Findings: []review.Finding{
			{Path: "hello.txt", Line: 1, Severity: review.SeverityLow, Comment: "可以再补一行说明"},
		},
	})
	if got.Event != ReviewEventComment {
		t.Fatalf("通过时应发表评论，实际 %s", got.Event)
	}
	if len(got.Comments) != 0 {
		t.Fatalf("通过不得请求修改: %+v", got.Comments)
	}
	if got.Body == "" {
		t.Fatal("评论应包含结论摘要")
	}
}
