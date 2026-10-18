package backend

import (
	"errors"
	"fmt"
	"strings"

	"agentdev/internal/llm"
	"agentdev/internal/tools"
)

// Error 表示可恢复的后端错误（限流、超时、轮数耗尽等），任务应进入 NEEDS_HUMAN。
type Error struct {
	Msg string
	// Cause 保留底层错误（例如 llm.APIError），让上层能区分余额不足等基础设施问题。
	Cause error
}

func (e *Error) Error() string { return e.Msg }

func (e *Error) Unwrap() error { return e.Cause }

func Errorf(format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// Wrap 把底层错误包装成可恢复的后端错误，同时保留错误链。
func Wrap(err error) error {
	if err == nil {
		return nil
	}
	return &Error{Msg: err.Error(), Cause: err}
}

// IsBackendError 判断是否为可恢复的后端错误。
func IsBackendError(err error) bool {
	var e *Error
	return errors.As(err, &e)
}

// InfoNeededError 表示步骤主动请求澄清：任务应进入 NEEDS_INFO 等待用户回复，
// 而不是当成失败挂起。与 Error 的区别在于它有明确的“需要什么信息”。
type InfoNeededError struct {
	Questions []string
}

func (e *InfoNeededError) Error() string {
	return "需要补充信息: " + strings.Join(e.Questions, "; ")
}

// AsInfoNeeded 判断错误是否为澄清请求。
func AsInfoNeeded(err error) (*InfoNeededError, bool) {
	var e *InfoNeededError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

type Usage = llm.Usage

type StepContext struct {
	TaskID         string
	Source         string
	Requirement    string
	Plan           string
	Worktree       string
	Tools          *tools.Runtime
	MaxTurns       int
	FailureSummary string
	// OnUsage 在每一轮模型调用后被调用，用于计费与预算熔断。
	// 返回错误会中止本步骤，并把该错误向上传递。
	OnUsage func(Usage) error
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
	source, trusted := "user", true
	if ctx.Source == "issue" {
		source, trusted = "issue", false
	}
	parts := []string{
		fmt.Sprintf("<task_requirement source=\"%s\" trusted=\"%t\">\n%s\n</task_requirement>", source, trusted, ctx.Requirement),
		fmt.Sprintf("<plan>\n%s\n</plan>", ctx.Plan),
	}
	if ctx.FailureSummary != "" {
		parts = append(parts, fmt.Sprintf("<failure_summary trusted=\"false\">\n%s\n</failure_summary>", ctx.FailureSummary))
	}
	if ctx.Tools != nil {
		if help := ctx.Tools.CommandHelp(); help != "" {
			parts = append(parts, fmt.Sprintf("<allowed_commands trusted=\"true\">\n%s\n</allowed_commands>", help))
		}
	}
	return strings.Join(parts, "\n\n")
}
