package backend

import (
	"fmt"
	"strings"
)

// ToolCall 是脚本中的一次工具调用。
type ToolCall struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// Fake 按脚本依次执行工具调用，用于测试与离线演示。脚本耗尽后不做任何修改。
type Fake struct {
	script [][]ToolCall
	Calls  []string
}

func NewFake(script [][]ToolCall) *Fake {
	return &Fake{script: script}
}

func (f *Fake) Name() string { return "fake" }

func (f *Fake) RunStep(ctx StepContext) (StepResult, error) {
	f.Calls = append(f.Calls, ctx.TaskID)
	var actions []ToolCall
	if len(f.script) > 0 {
		actions = f.script[0]
		f.script = f.script[1:]
	}
	var lines []string
	for _, a := range actions {
		res := ctx.Tools.Execute(a.Tool, a.Args)
		status := "error"
		if res.OK {
			status = "ok"
		}
		content := res.Content
		if len(content) > 200 {
			content = content[:200]
		}
		lines = append(lines, fmt.Sprintf("%s: %s %s", a.Tool, status, content))
	}
	return StepResult{FinalText: strings.Join(lines, "\n"), Turns: len(actions)}, nil
}
