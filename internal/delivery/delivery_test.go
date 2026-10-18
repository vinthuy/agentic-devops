package delivery

import (
	"context"
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
	res, err := fake.CreatePR(context.Background(), PRRequest{Base: "main", Head: "agent/x", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Number != 7 || len(fake.Requests) != 1 || !fake.Requests[0].Draft {
		t.Errorf("请求未被正确记录: %+v / %+v", res, fake.Requests)
	}
	if fake.Name() != "fake" {
		t.Errorf("名称异常: %s", fake.Name())
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
