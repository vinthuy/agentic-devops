package cli

import (
	"strings"
	"testing"

	"agentdev/internal/testutil"
)

func TestWebhookWithoutSecretExitsUsage(t *testing.T) {
	home := writeHome(t, testutil.NewRepo(t))
	t.Setenv("AGENTDEV_WEBHOOK_SECRET", "")
	code, _, errOut := invoke(t, "webhook", "--home", home)
	if code != ExitUsage {
		t.Fatalf("缺少密钥时退出码应为 %d，实际 %d: %s", ExitUsage, code, errOut)
	}
	if !strings.Contains(errOut, "AGENTDEV_WEBHOOK_SECRET") {
		t.Fatalf("应提示密钥环境变量: %s", errOut)
	}
}
