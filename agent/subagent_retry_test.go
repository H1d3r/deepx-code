package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"deepx/tools"

	tea "charm.land/bubbletea/v2"
)

// fakeStream 按预设序列依次返回每轮的 content;到末尾后一直返回最后一项。
func fakeStream(seq []string, calls *int) func(context.Context, string, string, string, []ChatMessage, int, []tools.OpenAIToolSpec, string, string, chan<- tea.Msg) (string, string, []ToolCall, string, *UsageInfo, error) {
	return func(ctx context.Context, apiKey, baseURL, modelID string, convo []ChatMessage, maxTokens int, toolSpecs []tools.OpenAIToolSpec, reasoningEffort, thinking string, ch chan<- tea.Msg) (string, string, []ToolCall, string, *UsageInfo, error) {
		i := *calls
		*calls++
		if i >= len(seq) {
			i = len(seq) - 1
		}
		return seq[i], "", nil, "stop", nil, nil
	}
}

// 第一轮模型抽风返回 "{ }"(带空格)→ 应丢弃并重试 → 第二轮拿到真报告。
func TestRunSubAgent_DegenerateRetry(t *testing.T) {
	orig := streamOnceFn
	defer func() { streamOnceFn = orig }()
	calls := 0
	streamOnceFn = fakeStream([]string{"{ }", "## 真报告\n\n完整内容在此。"}, &calls)

	res := runSubAgent(context.Background(), subAgentInput{
		Entry: ModelEntry{ContextWindow: 65536}, FullOutput: true,
		NodeID: "syn", NodeTitle: "汇总", Mode: AgentMode_Auto,
	})
	if res.Err != nil {
		t.Fatalf("err: %v", res.Err)
	}
	if calls != 2 {
		t.Fatalf("应重试一次(共 2 次调用),实际 %d", calls)
	}
	if !strings.Contains(res.Summary, "真报告") {
		t.Fatalf("重试后应拿到真报告,实际: %q", res.Summary)
	}
}

// 模型一直抽风返回 "{}" → 重试用尽后不再把 {} 当结果,而是给明确说明。
func TestRunSubAgent_DegenerateExhausted(t *testing.T) {
	orig := streamOnceFn
	defer func() { streamOnceFn = orig }()
	calls := 0
	streamOnceFn = fakeStream([]string{"{}"}, &calls)

	res := runSubAgent(context.Background(), subAgentInput{
		Entry: ModelEntry{ContextWindow: 65536}, FullOutput: true,
		NodeID: "syn", NodeTitle: "汇总", Mode: AgentMode_Auto,
	})
	if calls != maxBadOutputRetries+1 {
		t.Fatalf("应重试 %d 次(共 %d 次调用),实际 %d", maxBadOutputRetries, maxBadOutputRetries+1, calls)
	}
	if isDegenerateOutput(res.Summary) {
		t.Fatalf("重试用尽后不应再交出退化结果,实际: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "空结果") {
		t.Fatalf("应给明确说明,实际: %q", res.Summary)
	}
}

// 正常的短输出(如 review 说"无")不是退化,不应被重试。
func TestRunSubAgent_ShortOutputNotRetried(t *testing.T) {
	orig := streamOnceFn
	defer func() { streamOnceFn = orig }()
	calls := 0
	streamOnceFn = fakeStream([]string{"无"}, &calls)

	res := runSubAgent(context.Background(), subAgentInput{
		Entry: ModelEntry{ContextWindow: 65536}, FullOutput: true,
		NodeID: "rev", NodeTitle: "审查", Mode: AgentMode_Auto,
	})
	if calls != 1 {
		t.Fatalf("正常短输出不该重试,实际调用 %d 次", calls)
	}
	if res.Summary != "无" {
		t.Fatalf("应原样返回'无',实际: %q", res.Summary)
	}
}

// WantJSON 的 agent:第一轮返回 markdown(非 JSON)→ 应重试 → 第二轮返回合法 JSON。
func TestRunSubAgent_JSONRetry(t *testing.T) {
	orig := streamOnceFn
	defer func() { streamOnceFn = orig }()
	calls := 0
	streamOnceFn = fakeStream([]string{
		"## 安全审查\n\n| # | 问题 |\n|---|---|\n| 1 | xxx |",  // markdown,非 JSON
		`{"findings":[{"severity":"high","issue":"x"}]}`, // 合法 JSON
	}, &calls)

	res := runSubAgent(context.Background(), subAgentInput{
		Entry: ModelEntry{ContextWindow: 65536}, FullOutput: true, WantJSON: true,
		NodeID: "正确性审查", NodeTitle: "审查", Mode: AgentMode_Auto,
	})
	if calls != 2 {
		t.Fatalf("非 JSON 应重试一次(共 2 次调用),实际 %d", calls)
	}
	if !json.Valid([]byte(res.Summary)) {
		t.Fatalf("重试后应是合法 JSON,实际: %q", res.Summary)
	}
}

// WantJSON 的 agent 返回带 ```json 围栏的 JSON → extractJSONBlock 能剥出 → 视为合格,不重试。
func TestRunSubAgent_JSONFencedAccepted(t *testing.T) {
	orig := streamOnceFn
	defer func() { streamOnceFn = orig }()
	calls := 0
	streamOnceFn = fakeStream([]string{"```json\n{\"findings\":[]}\n```"}, &calls)
	res := runSubAgent(context.Background(), subAgentInput{
		Entry: ModelEntry{ContextWindow: 65536}, FullOutput: false, WantJSON: true,
		NodeID: "审查", NodeTitle: "审查", Mode: AgentMode_Auto,
	})
	if calls != 1 {
		t.Fatalf("带围栏的合法 JSON 不该重试,实际调用 %d 次", calls)
	}
	_ = res
}

// 声明 schema 的 agent:模型【调用 structured_output 工具】交结果 → 结果取自工具 arguments(合法 JSON),
// 而不是解析正文。即使正文是 markdown 也无所谓。
func TestRunSubAgent_StructuredOutputTool(t *testing.T) {
	orig := streamOnceFn
	defer func() { streamOnceFn = orig }()
	calls := 0
	streamOnceFn = func(ctx context.Context, apiKey, baseURL, modelID string, convo []ChatMessage, maxTokens int, toolSpecs []tools.OpenAIToolSpec, reasoningEffort, thinking string, ch chan<- tea.Msg) (string, string, []ToolCall, string, *UsageInfo, error) {
		calls++
		// 正文随便写点 markdown,真正的结果走工具调用的 arguments。
		return "我看完了,提交结果。", "", []ToolCall{{
			ID: "t1", Type: "function",
			Function: ToolCallFunc{Name: "structured_output", Arguments: `{"findings":[{"severity":"high","issue":"x"}]}`},
		}}, "tool_calls", nil, nil
	}

	res := runSubAgent(context.Background(), subAgentInput{
		Entry: ModelEntry{ContextWindow: 65536}, FullOutput: true, WantJSON: true,
		Schema: []byte(`{"type":"object","required":["findings"],"properties":{"findings":{"type":"array"}}}`),
		NodeID: "正确性审查", NodeTitle: "审查", Mode: AgentMode_Auto,
	})
	if calls != 1 {
		t.Fatalf("调了工具就该一轮拿到结果,实际调用 %d 次", calls)
	}
	if res.Summary != `{"findings":[{"severity":"high","issue":"x"}]}` {
		t.Fatalf("结果应取自 structured_output 的 arguments,实际: %q", res.Summary)
	}
}
