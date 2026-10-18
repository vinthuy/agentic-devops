package backend

import (
	"errors"
	"fmt"
	"strings"

	"agentdev/internal/tools"
)

// Error 表示可恢复的后端错误（限流、超时、轮数耗尽等），任务应进入 NEEDS_HUMAN。
type Error struct {
	Msg string
}

func (e *Error) Error() string { return e.Msg }

func Errorf(format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// IsBackendError 判断是否为可恢复的后端错误。
func IsBackendError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type StepContext struct {
	TaskID         string
	Requirement    string
	Plan           string
	Worktree       string
	Tools          *tools.Runtime
	MaxTurns       int
	FailureSummary string
}

type StepResult struct {
	FinalText string
	Turns     int
	Usage     Usage
}

// ExecutionBackend 负责一步编码：与模型交互并通过 ToolRuntime 执行副作用。
type ExecutionBackend interface {
	Name() string
	RunStep(ctx StepContext) (StepResult, error)
}

// UserMessage 按 SDD 9.2 的标签约定构造首条用户消息。
func UserMessage(ctx StepContext) string {
	parts := []string{
		fmt.Sprintf("<task_requirement source=\"user\">\n%s\n</task_requirement>", ctx.Requirement),
		fmt.Sprintf("<plan>\n%s\n</plan>", ctx.Plan),
	}
	if ctx.FailureSummary != "" {
		parts = append(parts, fmt.Sprintf("<failure_summary trusted=\"false\">\n%s\n</failure_summary>", ctx.FailureSummary))
	}
	return strings.Join(parts, "\n\n")
}
