package agent

import (
	"strings"
	"testing"
)

// 本文件钉的是「大 Write 入历史后消息结构不变」这个不变量。
//
// 背景:曾有方案提出把大 content 的 Write 从 assistant.tool_calls 中整体移除、
// 改用一条 role=user 的"执行记录"消息呈现。那样做会踩 sanitizeToolPairs ——
// user 消息会终结 tool 配对组(见 sanitize.go 的 default 分支),于是同一批次里
// 排在它后面的 tool 结果全部变成孤儿被丢弃,还会留下悬挂 tool_calls,正是
// sanitizeToolPairs 存在的目的(issue #94)所要防的那个 400。
//
// 现在参数原样入历史,结构一动不动,下面把这点固定下来。

// pairingReport 返回:被丢弃的 tool 消息数、悬挂(无 tool 响应)的 tool_call id。
func pairingReport(in []ChatMessage) (dropped int, dangling []string) {
	out := sanitizeToolPairs(in)

	kept := make(map[string]bool)
	for _, m := range out {
		if m.Role == "tool" {
			kept[m.ToolCallID] = true
		}
	}
	for _, m := range in {
		if m.Role == "tool" && !kept[m.ToolCallID] {
			dropped++
		}
	}
	for _, m := range out {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if !kept[tc.ID] {
				dangling = append(dangling, tc.ID)
			}
		}
	}
	return dropped, dangling
}

// TestBigWrite_KeepsToolPairing 一轮里同时有大 Write 和另一个工具时,配对必须完好。
func TestBigWrite_KeepsToolPairing(t *testing.T) {
	bigArgs := `{"path":"big.go","content":` + jsonStr(strings.Repeat("x", 64*1024)) + `}`
	convo := []ChatMessage{
		{Role: "user", Content: "写个大文件,再读一下 main.go"},
		{Role: "assistant", ToolCalls: rewriteToolCallArgsForHistory([]ToolCall{
			{ID: "call_write", Type: "function", Function: ToolCallFunc{Name: "Write", Arguments: bigArgs}},
			{ID: "call_read", Type: "function", Function: ToolCallFunc{Name: "Read", Arguments: `{"path":"main.go"}`}},
		})},
		{Role: "tool", ToolCallID: "call_write", Name: "Write", Content: "已写入 big.go"},
		{Role: "tool", ToolCallID: "call_read", Name: "Read", Content: "package main"},
	}

	dropped, dangling := pairingReport(convo)
	if dropped != 0 {
		t.Errorf("不应有 tool 结果被丢弃, got %d 条", dropped)
	}
	if len(dangling) != 0 {
		t.Errorf("不应有悬挂 tool_calls, got %v", dangling)
	}
	// 正常会话下 sanitizeToolPairs 必须是 no-op(不动前缀 = 不击穿缓存)
	if out := sanitizeToolPairs(convo); len(out) != len(convo) {
		t.Errorf("正常会话应原样返回, want %d 条 got %d 条", len(convo), len(out))
	}
}

// TestBigWrite_FailedWriteErrorReachesModel 大 Write 失败时,错误必须传达给模型。
// (若把 tool_call 从 assistant 里移除,这条 tool 消息会变孤儿被消毒器丢掉,
// 模型将完全不知道写失败了,大概率当成功继续往下走。)
func TestBigWrite_FailedWriteErrorReachesModel(t *testing.T) {
	bigArgs := `{"path":"big.go","content":` + jsonStr(strings.Repeat("x", 64*1024)) + `}`
	convo := []ChatMessage{
		{Role: "user", Content: "写个大文件"},
		{Role: "assistant", ToolCalls: rewriteToolCallArgsForHistory([]ToolCall{
			{ID: "call_write", Type: "function", Function: ToolCallFunc{Name: "Write", Arguments: bigArgs}},
		})},
		{Role: "tool", ToolCallID: "call_write", Name: "Write", Content: "写入失败: permission denied"},
	}

	for _, m := range sanitizeToolPairs(convo) {
		if m.Role == "tool" && strings.Contains(m.Content, "permission denied") {
			return
		}
	}
	t.Fatal("大 Write 的失败信息必须传达给模型")
}

// TestBigWrite_CountedInTokenEstimate 大 content 必须被计入 token 估算,
// 否则压缩不会按时触发 —— 这是"不折叠参数"能成立的前提。
func TestBigWrite_CountedInTokenEstimate(t *testing.T) {
	const n = 64 * 1024
	bigArgs := `{"path":"big.go","content":` + jsonStr(strings.Repeat("x", n)) + `}`
	msg := ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
		{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "Write", Arguments: bigArgs}},
	}}

	// 只要求量级对得上,不锁死具体分词器的结果。
	if got := MsgTokens(msg); got < n/8 {
		t.Fatalf("大 content 应被计入 MsgTokens, got=%d(content %d 字节)—— 漏算会导致压缩不触发", got, n)
	}
}
