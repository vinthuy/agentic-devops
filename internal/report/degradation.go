package report

import (
	"fmt"
	"strings"

	"agentdev/internal/store"
)

// Degradation 是一次「结果可用、但偏离了设计承诺」的记录。
//
// 单独归类的原因：降级如果只是躺在事件流里，一份降级跑出来的报告会和完全跑通的
// 报告长得一模一样，而这恰好违背本项目「可审计、可信任」的前提。凡是降级，
// 都必须在报告和命令行里被显式点名。
type Degradation struct {
	TS     string
	Type   string
	Label  string
	Detail string
}

// degradationLabels 的键是事件类型。只有出现在这里的类型才会被当成需要人工
// 注意的降级；其余事件（state_changed、step_finished 等）属于正常流程。
var degradationLabels = map[string]string{
	"triage_model_failed":     "分级模型调用失败，本次仅按规则分级",
	"plan_failed":             "模型规划失败，已降级为静态计划（验收标准仅来自验证命令）",
	"plan_scope_unrestricted": "计划未限定改动范围，写入仅受全局策略约束",
	"cost_pricing_missing":    "缺少模型单价，调用按 $0 计，预算熔断对此无效",
	"budget_warning":          "花费接近预算上限",
	"review_failed":           "独立审查执行失败，结果已挂起等待人工",
	"ci_failed":               "CI 检查失败，已将结果交给自动修复",
	"secret_scan_failed":      "密钥扫描命中，已阻止推送",
	"anticheat":               "防作弊检查命中，已转人工",
}

// Degradations 按时间顺序返回需要人工注意的降级与异常。
func Degradations(events []store.Event) []Degradation {
	var out []Degradation
	for _, e := range events {
		if e.Type == "triage_ready" || e.Type == "plan_ready" {
			continue
		}
		label, ok := degradationLabels[e.Type]
		if !ok {
			continue
		}
		out = append(out, Degradation{
			TS:     e.TS,
			Type:   e.Type,
			Label:  label,
			Detail: degradationDetail(e),
		})
	}
	return out
}

// SummaryLine 返回一行用于命令行输出的提醒。
func (d Degradation) SummaryLine() string {
	if d.Detail == "" {
		return d.Type + ": " + d.Label
	}
	return fmt.Sprintf("%s: %s（%s）", d.Type, d.Label, d.Detail)
}

// EventDetail 返回 task status 与报告时间线使用的安全、精简摘要。
// 不直接序列化整个 payload，避免把长 CI 日志或任意外部内容塞进终端。
func EventDetail(e store.Event) string {
	for _, d := range Degradations([]store.Event{e}) {
		if d.Detail == "" {
			return d.Label
		}
		return d.Label + "：" + d.Detail
	}
	p := e.Payload
	switch e.Type {
	case "triage_ready":
		return fmt.Sprintf("来源 %s，级别 %s", payloadString(p, "source"), payloadString(p, "level"))
	case "plan_ready":
		return "计划来源 " + payloadString(p, "source")
	case "review_result":
		return fmt.Sprintf("%s：%s（%d 条意见）", payloadString(p, "decision"),
			truncate(payloadString(p, "summary"), 160), len(payloadItems(p, "findings")))
	case "state_changed":
		if ci := payloadString(p, "ci"); ci != "" {
			return ci + "：" + truncate(payloadString(p, "summary"), 160)
		}
		if reason := payloadString(p, "reason"); reason != "" {
			return truncate(reason, 200)
		}
	case "fix_requested":
		source := payloadString(p, "source")
		if source != "" {
			return "修复来源 " + source
		}
	}
	return ""
}

// degradationDetail 从事件负载里挑出对人有用的一句话说明。
// 负载结构按事件类型而定，拿不到就返回空串而不是报错——报告本身不能因为
// 某条事件的负载不完整而失败。
func degradationDetail(e store.Event) string {
	p := e.Payload
	switch e.Type {
	case "plan_failed", "triage_model_failed":
		return truncate(payloadString(p, "error"), 300)
	case "review_failed":
		return truncate(payloadString(p, "error"), 300)
	case "ci_failed":
		return payloadString(p, "summary")
	case "cost_pricing_missing":
		return fmt.Sprintf("模型 %s。%s", payloadString(p, "model"), payloadString(p, "note"))
	case "budget_warning":
		return fmt.Sprintf("已用 $%.4f / 预算 $%.2f",
			payloadFloat(p, "spent_usd"), payloadFloat(p, "budget_usd"))
	case "plan_scope_unrestricted":
		return payloadString(p, "note")
	case "secret_scan_failed", "anticheat":
		if n := len(payloadItems(p, "findings")); n > 0 {
			return fmt.Sprintf("命中 %d 项，详见事件负载", n)
		}
		return ""
	}
	return ""
}

func payloadString(p map[string]any, key string) string {
	s, _ := p[key].(string)
	return s
}

func payloadFloat(p map[string]any, key string) float64 {
	f, _ := p[key].(float64)
	return f
}

// payloadItems 处理负载里的数组；JSON 反序列化后元素是 any，不假设具体类型。
func payloadItems(p map[string]any, key string) []any {
	items, _ := p[key].([]any)
	return items
}

func payloadStrings(p map[string]any, key string) []string {
	items := payloadItems(p, key)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func truncate(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// planSource 说明计划从哪来。降级的证据（plan_failed）优先，因为它是唯一能
// 说明「验收标准并非由模型给出」的信号。
func planSource(events []store.Event, costs []store.CostEntry) string {
	for _, e := range events {
		if e.Type == "plan_ready" {
			switch payloadString(e.Payload, "source") {
			case "model":
				return "模型生成"
			case "static":
				return "静态计划（未启用模型规划）"
			case "static_fallback":
				return "静态降级（模型规划失败，验收标准仅来自验证命令）"
			case "configured_planner":
				return "配置的自定义规划器"
			}
		}
		if e.Type == "plan_failed" {
			return "静态降级（模型规划失败，验收标准仅来自验证命令）"
		}
	}
	for _, c := range costs {
		if c.Role == "planner" {
			return "模型生成"
		}
	}
	return "静态计划（未启用模型规划）"
}

// triageSource 说明分级从哪来。
func triageSource(events []store.Event, costs []store.CostEntry) string {
	for _, e := range events {
		if e.Type == "triage_ready" {
			switch payloadString(e.Payload, "source") {
			case "rules":
				return "规则"
			case "user_override":
				return "用户显式指定"
			case "rules+model":
				return "规则 + 模型（取更保守者）"
			}
		}
		if e.Type == "triage_model_failed" {
			return "规则（模型估算失败）"
		}
	}
	for _, c := range costs {
		if c.Role == "triage" {
			return "规则 + 模型（取更保守者）"
		}
	}
	return "规则"
}
