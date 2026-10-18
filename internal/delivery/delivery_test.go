package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const sampleDiff = `diff --git a/src/app.py b/src/app.py
index 1..2 100644
--- a/src/app.py
+++ b/src/app.py
@@ -1,3 +1,4 @@
 import os
+AWS_KEY = "AKIAABCDEFGHIJKLMNOP"
 print(1)
`

func TestScanDiffFindsAddedSecrets(t *testing.T) {
	findings := ScanDiff(sampleDiff)
	if len(findings) != 1 {
		t.Fatalf("应命中 1 条，实际 %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Path != "src/app.py" || f.Rule != "aws_access_key_id" || f.Line != 2 {
		t.Errorf("定位错误: %+v", f)
	}
	if strings.Contains(f.Detail, "AKIAABCDEFGHIJKLMNOP") {
		t.Errorf("详情不应回显完整密钥: %s", f.Detail)
	}
}

func TestScanDiffIgnoresRemovedLines(t *testing.T) {
	diff := `--- a/src/app.py
+++ b/src/app.py
@@ -1,2 +1,1 @@
-API_KEY = "sk-abcdefghijklmnopqrstuvwx"
 print(1)
`
	if findings := ScanDiff(diff); len(findings) != 0 {
		t.Errorf("删除密钥不是泄露，实际命中: %+v", findings)
	}
}

func TestScanDiffFlagsSensitiveFileNames(t *testing.T) {
	diff := `--- /dev/null
+++ b/config/.env.production
@@ -0,0 +1,1 @@
+HOST=example.com
`
	findings := ScanDiff(diff)
	if len(findings) == 0 || findings[0].Rule != "sensitive_file" {
		t.Fatalf("应命中敏感文件名: %+v", findings)
	}
}

func TestScanDiffFindsAssignedSecrets(t *testing.T) {
	diff := `--- a/x.py
+++ b/x.py
@@ -0,0 +1,1 @@
+password = "hunter2hunter2"
`
	findings := ScanDiff(diff)
	if len(findings) != 1 || findings[0].Rule != "assigned_secret" {
		t.Fatalf("应命中赋值形式的密钥: %+v", findings)
	}
}

func TestScanDiffCleanPatch(t *testing.T) {
	diff := `--- a/x.py
+++ b/x.py
@@ -0,0 +1,2 @@
+def add(a, b):
+    return a + b
`
	if findings := ScanDiff(diff); len(findings) != 0 {
		t.Errorf("干净补丁不应命中: %+v", findings)
	}
}

func TestParseHunkStart(t *testing.T) {
	if got := parseHunkStart("@@ -10,7 +20,8 @@ func main()"); got != 20 {
		t.Errorf("起始行解析错误: %d", got)
	}
	if got := parseHunkStart("garbage"); got != 0 {
		t.Errorf("无法解析时应返回 0: %d", got)
	}
}

func TestPRNumberFromURL(t *testing.T) {
	if got := prNumberFromURL("https://github.com/acme/app/pull/42"); got != 42 {
		t.Errorf("PR 编号解析错误: %d", got)
	}
	if got := prNumberFromURL("https://example.com/nothing"); got != 0 {
		t.Errorf("无编号时应为 0: %d", got)
	}
}

func TestFakeCodeHostRecordsRequests(t *testing.T) {
	fake := &Fake{Result: PRResult{Number: 7, URL: "https://example.invalid/pull/7"}}
	res, err := fake.EnsurePR(context.Background(), PRRequest{Base: "main", Head: "agent/x", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Number != 7 || len(fake.Requests) != 1 || !fake.Requests[0].Draft {
		t.Errorf("请求未被正确记录: %+v / %+v", res, fake.Requests)
	}
	if !res.Created {
		t.Error("首次创建应标记 Created")
	}
	if fake.Name() != "fake" {
		t.Errorf("名称异常: %s", fake.Name())
	}
}

// CI 失败后重走交付流程时，必须复用已有 PR 而不是再次创建。
func TestFakeCodeHostReportsExistingPR(t *testing.T) {
	fake := &Fake{Existing: true, Result: PRResult{Number: 7, URL: "https://example.invalid/pull/7"}}
	res, err := fake.EnsurePR(context.Background(), PRRequest{Head: "agent/x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created {
		t.Error("已存在的 PR 不应标记为新建")
	}
	if res.Number != 7 {
		t.Errorf("应返回已有 PR 编号: %+v", res)
	}
}

func TestFakeCIConsumesSnapshotsThenRepeatsLast(t *testing.T) {
	ci := &FakeCI{Snapshots: []CISnapshot{
		{State: CIPending, Summary: "running"},
		{State: CIPassed, Summary: "ok"},
	}}
	for i, want := range []CIState{CIPending, CIPassed, CIPassed} {
		snap, err := ci.Checks(context.Background(), CIRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if snap.State != want {
			t.Errorf("第 %d 次应为 %s，实际 %s", i+1, want, snap.State)
		}
	}
}

func TestTruncateTailKeepsTheEnd(t *testing.T) {
	got := truncateTail("12345", 3)
	if !strings.HasSuffix(got, "345") || !strings.Contains(got, "截断") {
		t.Errorf("应保留尾部并标注截断: %q", got)
	}
}

func TestRedactTextHidesKnownSecretFormats(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	got := RedactText("CI log contains " + secret + " in output")
	if strings.Contains(got, secret) || !strings.Contains(got, "*") {
		t.Errorf("密钥应脱敏: %q", got)
	}
}

type ghCall struct {
	args  []string
	stdin string
}

type fakeGHRunner struct {
	calls []ghCall
	steps []struct {
		stdout string
		stderr string
		err    error
	}
}

func (f *fakeGHRunner) Run(_ context.Context, _ string, _ string, args []string, stdin string) (string, string, error) {
	f.calls = append(f.calls, ghCall{args: append([]string(nil), args...), stdin: stdin})
	if len(f.steps) == 0 {
		return "", "unexpected gh call", errors.New("unexpected call")
	}
	step := f.steps[0]
	f.steps = f.steps[1:]
	return step.stdout, step.stderr, step.err
}

func ghStep(stdout, stderr string, err error) struct {
	stdout string
	stderr string
	err    error
} {
	return struct {
		stdout string
		stderr string
		err    error
	}{stdout, stderr, err}
}

func TestGHCliEnsurePRUpdatesExistingInsteadOfCreatingDuplicate(t *testing.T) {
	runner := &fakeGHRunner{steps: []struct {
		stdout string
		stderr string
		err    error
	}{
		ghStep(`{"number":12,"url":"https://github.com/acme/app/pull/12"}`, "", nil),
		ghStep("", "", nil),
	}}
	gh := GHCli{Path: "gh-test", Runner: runner}
	pr, err := gh.EnsurePR(context.Background(), PRRequest{
		Worktree: "repo", Head: "agent/task", Title: "updated", Body: "new body", Draft: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 12 || pr.Created {
		t.Errorf("应返回已有 PR 且不标新建: %+v", pr)
	}
	if len(runner.calls) != 2 || strings.Join(runner.calls[1].args[:2], " ") != "pr edit" {
		t.Fatalf("应为 view + edit 两次调用，而非 create: %+v", runner.calls)
	}
	if runner.calls[1].stdin != "new body" {
		t.Errorf("正文应通过 stdin 提供: %q", runner.calls[1].stdin)
	}
}

func TestGHCliEnsurePRCreatesWhenMissing(t *testing.T) {
	runner := &fakeGHRunner{steps: []struct {
		stdout string
		stderr string
		err    error
	}{
		ghStep("", "no pull requests found for branch \"agent/task\"", errors.New("exit 1")),
		ghStep("https://github.com/acme/app/pull/13\n", "", nil),
	}}
	pr, err := (GHCli{Runner: runner}).EnsurePR(context.Background(), PRRequest{Worktree: "repo", Head: "agent/task"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 13 || !pr.Created || len(runner.calls) != 2 || !strings.Contains(strings.Join(runner.calls[1].args, " "), "create") {
		t.Errorf("缺少 PR 时应创建一次: %+v calls=%+v", pr, runner.calls)
	}
}

func TestGHCliEnsurePRDoesNotTreatAuthFailureAsMissing(t *testing.T) {
	runner := &fakeGHRunner{steps: []struct {
		stdout string
		stderr string
		err    error
	}{ghStep("", "authentication required", errors.New("exit 1"))}}
	_, err := (GHCli{Runner: runner}).EnsurePR(context.Background(), PRRequest{Head: "agent/task"})
	if err == nil || !strings.Contains(err.Error(), "gh pr view 失败") || len(runner.calls) != 1 {
		t.Fatalf("鉴权失败不得误判为不存在并 create: err=%v calls=%+v", err, runner.calls)
	}
}

func TestGHCliChecksParsesFailuresAndFetchesLogs(t *testing.T) {
	runner := &fakeGHRunner{steps: []struct {
		stdout string
		stderr string
		err    error
	}{
		ghStep(`[{"name":"unit","state":"FAILURE","bucket":"fail"}]`, "", nil),
		ghStep(`[{"databaseId":99,"conclusion":"failure","workflowName":"CI"}]`, "", nil),
		ghStep("unit\tfailed: undefined symbol\n", "", nil),
	}}
	snap, err := (GHCli{Runner: runner}).Checks(context.Background(), CIRequest{Worktree: "repo", PRNumber: 4, Head: "agent/task"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != CIFailed || snap.Checks != 1 || !strings.Contains(snap.Summary, "unit") || !strings.Contains(snap.Logs, "undefined symbol") {
		t.Errorf("CI 失败解析异常: %+v", snap)
	}
}

func TestGHCliChecksTreatsNoConfiguredChecksAsNone(t *testing.T) {
	runner := &fakeGHRunner{steps: []struct {
		stdout string
		stderr string
		err    error
	}{ghStep("", "no checks reported on the branch", fmt.Errorf("exit 1"))}}
	snap, err := (GHCli{Runner: runner}).Checks(context.Background(), CIRequest{Worktree: "repo", PRNumber: 4})
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != CINone {
		t.Errorf("无 CI 检查应返回 none: %+v", snap)
	}
}

func TestGHCliChecksUnknownStateIsNotSuccess(t *testing.T) {
	runner := &fakeGHRunner{steps: []struct {
		stdout string
		stderr string
		err    error
	}{ghStep(`[{"name":"mystery","state":"SOMETHING_NEW","bucket":"unknown"}]`, "", nil)}}
	snap, err := (GHCli{Runner: runner}).Checks(context.Background(), CIRequest{Worktree: "repo", PRNumber: 5})
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != CIPending {
		t.Errorf("未知状态不能误报通过，应保守等待: %+v", snap)
	}
}

func TestMarshalFindingsProducesMaps(t *testing.T) {
	out := MarshalFindings([]Finding{{Path: "a", Line: 1, Rule: "r", Detail: "d"}})
	if len(out) != 1 {
		t.Fatalf("长度错误: %v", out)
	}
	m, ok := out[0].(map[string]any)
	if !ok || m["path"] != "a" {
		t.Errorf("结构错误: %+v", out[0])
	}
}
