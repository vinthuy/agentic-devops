package orchestrator

import (
	"testing"

	"agentdev/internal/backend"
	"agentdev/internal/state"
)

func TestRetrievalMissStopsForClarification(t *testing.T) {
	f := setup(t)
	task := f.taskWithRequirement("修复会话在并发下丢失状态")
	be := backend.NewFake([][]backend.ToolCall{good})
	done := f.run(be, task.ID)
	if done.Status != state.NeedsInfo {
		t.Fatalf("没有命中时应待澄清，实际 %s", done.Status)
	}
	if done.Status == state.Coding || done.WorktreePath != "" {
		t.Fatal("没有命中时不应进入编码")
	}
	events, err := f.st.ListEvents(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == "retrieval_completed" && e.Payload["hits"] == float64(0) {
			found = true
		}
	}
	if !found {
		t.Fatal("应记录检索没有命中")
	}
	if len(be.Calls) != 0 {
		t.Fatal("待澄清前不应调用编码后端")
	}
}
