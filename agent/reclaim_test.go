package agent

import (
	"fmt"
	"strings"
	"testing"
)

func toolMsg(id, name, content string) ChatMessage {
	return ChatMessage{Role: "tool", ToolCallID: id, Name: name, Content: content}
}

func asstCall(id, name, argsJSON string) ChatMessage {
	return ChatMessage{Role: "assistant", ToolCalls: []ToolCall{
		{ID: id, Type: "function", Function: ToolCallFunc{Name: name, Arguments: argsJSON}},
	}}
}

// buildConvo 造 1 system + 1 user + n 轮 (assistant call + tool result),每条工具结果内容为 content。
func buildConvo(n int, name, content string) []ChatMessage {
	convo := []ChatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "任务"}}
	for k := 0; k < n; k++ {
		id := fmt.Sprintf("c%d", k)
		convo = append(convo, asstCall(id, name, fmt.Sprintf(`{"path":"src/f%d.go"}`, k)))
		convo = append(convo, toolMsg(id, name, content))
	}
	return convo
}

func countTool(convo []ChatMessage) (reclaimed, kept int) {
	for _, m := range convo {
		if m.Role != "tool" {
			continue
		}
		if strings.HasPrefix(m.Content, reclaimMarkerPrefix) {
			reclaimed++
		} else {
			kept++
		}
	}
	return
}

func TestReclaim_OldReplacedRecentKept(t *testing.T) {
	big := strings.Repeat("这是一大段读取内容。", 500) // 每条远超小预算
	convo := buildConvo(10, "Read", big)
	before := len(convo)

	if n, _ := reclaimToolOutputs(convo, 4096); n == 0 { // 预算 = 4096*20% ≈ 819 token
		t.Fatal("应发生回收")
	}
	reclaimed, kept := countTool(convo)
	if reclaimed == 0 {
		t.Fatal("较旧工具结果应被回收")
	}
	if kept < reclaimMinTailToolMsgs {
		t.Fatalf("最近 %d 条应保留,实际保留 %d", reclaimMinTailToolMsgs, kept)
	}
	if len(convo) != before {
		t.Fatalf("reclaim 不应增删消息: %d -> %d", before, len(convo))
	}
}

func TestReclaim_TailProtected(t *testing.T) {
	convo := buildConvo(reclaimMinTailToolMsgs+3, "Bash", strings.Repeat("x", 5000))
	_, _ = reclaimToolOutputs(convo, 1024) // 预算极小

	seen := 0
	for i := len(convo) - 1; i >= 0; i-- {
		if convo[i].Role != "tool" {
			continue
		}
		seen++
		if seen <= reclaimMinTailToolMsgs && strings.HasPrefix(convo[i].Content, reclaimMarkerPrefix) {
			t.Fatalf("最近第 %d 条工具结果被误回收", seen)
		}
	}
}

func TestReclaim_ReferenceHasNameAndPath(t *testing.T) {
	convo := buildConvo(8, "Read", strings.Repeat("y", 8000))
	_, _ = reclaimToolOutputs(convo, 2048)

	found := false
	for _, m := range convo {
		if m.Role == "tool" && strings.HasPrefix(m.Content, reclaimMarkerPrefix) {
			if !strings.Contains(m.Content, "Read") || !strings.Contains(m.Content, "src/f") {
				t.Fatalf("引用应含工具名和 path, got=%q", m.Content)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("应有被回收的工具结果")
	}
}

func TestReclaim_Idempotent(t *testing.T) {
	convo := buildConvo(8, "Read", strings.Repeat("z", 6000))
	if n, _ := reclaimToolOutputs(convo, 2048); n == 0 {
		t.Fatal("首次应有改动")
	}
	if n, _ := reclaimToolOutputs(convo, 2048); n > 0 {
		t.Fatal("再次 reclaim 不应有新改动(幂等)")
	}
}

func TestReclaim_NonToolUntouched(t *testing.T) {
	userBig := strings.Repeat("u", 9000)
	asstBig := strings.Repeat("a", 9000)
	convo := []ChatMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: userBig},
		asstCall("c1", "Read", `{"path":"a.go"}`),
		{Role: "assistant", Content: asstBig},
	}
	before := len(convo)
	_, _ = reclaimToolOutputs(convo, 1024)
	if convo[1].Content != userBig {
		t.Fatal("user 消息不应被改")
	}
	if convo[3].Content != asstBig {
		t.Fatal("assistant 文本不应被改")
	}
	if len(convo) != before {
		t.Fatal("消息数不应变")
	}
}

func TestReclaim_UnderBudgetNoChange(t *testing.T) {
	convo := buildConvo(6, "Read", "tiny") // 工具输出很小
	if n, _ := reclaimToolOutputs(convo, 1_000_000); n > 0 {
		t.Fatal("预算充足时不应回收")
	}
}

func TestReclaim_SmallOldOutputsKept(t *testing.T) {
	// 较新的几条大 Read 把 keptTokens 撑过预算;更旧的都是小 Bash 输出。
	// 大的旧 Read 应被回收,小的旧 Bash 不该(占位≈原文,回收无意义)。
	big := strings.Repeat("x", 8000)
	convo := []ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "t"}}
	for k := 0; k < 6; k++ { // 更旧:小 Bash
		id := fmt.Sprintf("old%d", k)
		convo = append(convo, asstCall(id, "Bash", `{"command":"echo"}`))
		convo = append(convo, toolMsg(id, "Bash", "小结果"))
	}
	for k := 0; k < 5; k++ { // 较新:大 Read
		id := fmt.Sprintf("new%d", k)
		convo = append(convo, asstCall(id, "Read", fmt.Sprintf(`{"path":"f%d.go"}`, k)))
		convo = append(convo, toolMsg(id, "Read", big))
	}
	_, _ = reclaimToolOutputs(convo, 2048) // keepBudget≈409;5 条大 Read 远超,更旧的进入回收判定

	reclaimedRead := false
	for _, m := range convo {
		if m.Role != "tool" || !strings.HasPrefix(m.Content, reclaimMarkerPrefix) {
			continue
		}
		if m.Name == "Bash" {
			t.Fatal("小的旧 Bash 输出不应被回收(低于 reclaimMinMsgTokens)")
		}
		if m.Name == "Read" {
			reclaimedRead = true
		}
	}
	if !reclaimedRead {
		t.Fatal("大的旧 Read 输出应被回收(sanity)")
	}
}

func TestReclaim_BelowTotalFloorNoChange(t *testing.T) {
	// 总回收量低于聚合下限(reclaimMinTotalPct 窗口)时,整趟不动 —— 护缓存。
	ctxWin := 100000                    // minTotal = 5000, keepBudget = 20000
	huge := strings.Repeat("x", 40000)  // ~10k token,4 条就撑爆 keepBudget
	medium := strings.Repeat("y", 1200) // > reclaimMinMsgTokens,但两条合计仍 < minTotal
	convo := []ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "t"}}
	for k := 0; k < 2; k++ { // 更旧:2 条 medium(会成为候选,但总量小)
		id := fmt.Sprintf("old%d", k)
		convo = append(convo, asstCall(id, "Read", `{"path":"o.go"}`))
		convo = append(convo, toolMsg(id, "Read", medium))
	}
	for k := 0; k < reclaimMinTailToolMsgs; k++ { // 较新:huge 占满 tail + 撑爆预算
		id := fmt.Sprintf("new%d", k)
		convo = append(convo, asstCall(id, "Read", `{"path":"n.go"}`))
		convo = append(convo, toolMsg(id, "Read", huge))
	}

	if n, _ := reclaimToolOutputs(convo, ctxWin); n > 0 {
		t.Fatal("总回收量低于聚合下限时不应回收(护缓存)")
	}
	for _, m := range convo {
		if m.Role == "tool" && strings.HasPrefix(m.Content, reclaimMarkerPrefix) {
			t.Fatal("聚合下限未过,任何工具输出都不应被回收")
		}
	}
}

func TestReclaim_PairingPreserved(t *testing.T) {
	// 回收后每条 tool 消息的 ToolCallID / Name 不变,配对完整。
	convo := buildConvo(10, "Read", strings.Repeat("w", 6000))
	ids := map[int]string{}
	for i, m := range convo {
		if m.Role == "tool" {
			ids[i] = m.ToolCallID
		}
	}
	_, _ = reclaimToolOutputs(convo, 2048)
	for i, m := range convo {
		if m.Role == "tool" {
			if m.ToolCallID != ids[i] {
				t.Fatalf("第 %d 条 ToolCallID 被改动", i)
			}
			if m.Name == "" {
				t.Fatalf("第 %d 条 Name 丢失", i)
			}
		}
	}
}
