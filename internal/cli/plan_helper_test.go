package cli

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// applyPlanJSON 直接把一份计划写进任务记录。
//
// 用它而不是跑一遍真实规划，是为了让「计划要改测试文件」这种情形可确定地构造出来：
// 靠模型偶然产出这样的计划来测安全提示是不可接受的。
func applyPlanJSON(t *testing.T, home, taskID, planJSON string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "agentdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE tasks SET plan_json = ? WHERE id = ?", planJSON, taskID); err != nil {
		t.Fatal(err)
	}
}
