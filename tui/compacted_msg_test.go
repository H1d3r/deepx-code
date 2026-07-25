package tui

import (
	"errors"
	"strings"
	"testing"

	"deepx/agent"

	tea "charm.land/bubbletea/v2"
)

// compactedTestModel 造一个能收 agent 消息的最小 model(无 session / 无 web hub)。
func compactedTestModel(usage *agent.UsageInfo) model {
	m := model{}
	m.chatContent = newChatLog(1 << 20)
	m.streamCh = make(chan tea.Msg, 4)
	m.lastUsage = usage
	return m
}

// mustModel 取 Update 的返回并断言回 model,省去每处 .(model)。
func mustModel(mm tea.Model, _ tea.Cmd) model { return mm.(model) }

// issue #201:轮内自动压缩此前只在 Turns>0 时往对话区打一行,而且完全不刷新状态栏 ——
// 压缩悄无声息地发生,右栏百分比还挂在压缩前的数字上,用户以为没压成功。
func TestCompactedMsg_LeavesTraceAndRefreshesUsage(t *testing.T) {
	m := compactedTestModel(&agent.UsageInfo{PromptTokens: 223_000, PromptCacheHitTokens: 100_000})

	next, _ := m.Update(agent.CompactedMsg{Summary: "摘要", Turns: 6, EstPromptTokens: 89_000})
	got := next.(model)

	if !strings.Contains(got.chatContent.String(), "已自动压缩会话历史") {
		t.Errorf("对话区应留下压缩痕迹, got=%q", got.chatContent.String())
	}
	if !strings.Contains(got.chatContent.String(), "6") {
		t.Error("痕迹里应带被压掉的轮数")
	}
	if got.lastUsage.PromptTokens != 89_000 {
		t.Errorf("状态栏 prompt 应降到压缩后的估算 89000, got %d", got.lastUsage.PromptTokens)
	}
	if got.lastUsage.PromptCacheHitTokens != 0 {
		t.Error("前缀已变,旧缓存命中数应清零(否则 cache% 失真)")
	}
	if got.summary != "摘要" {
		t.Errorf("摘要应存进 m.summary, got %q", got.summary)
	}
}

// 原子性:摘要和截断后的历史必须在同一条消息里一起落地。分两条发的时候,ESC 恰好卡在中间会把
// 后一条 drainAndDiscard 掉,留下"摘要存了、历史没截断"的半吊子状态 —— 下一轮摘要和完整历史
// 一起发出去,内容重复,上下文反而比压缩前更大(issue #201)。
func TestCompactedMsg_SummaryAndHistoryLandTogether(t *testing.T) {
	m := compactedTestModel(nil)
	m.history = []agent.ChatMessage{ // 压缩前:一长串
		{Role: "user", Content: "老任务"},
		{Role: "assistant", Content: "老回复"},
		{Role: "user", Content: "新任务"},
		{Role: "assistant", Content: "新回复"},
	}

	// agent 送来的是完整 convo(首条 system),截断后只剩最近一轮。
	next, _ := m.Update(agent.CompactedMsg{
		Summary: "摘要",
		Turns:   1,
		History: []agent.ChatMessage{
			{Role: "system", Content: "sys(含摘要)"},
			{Role: "user", Content: "新任务"},
			{Role: "assistant", Content: "新回复"},
		},
	})
	got := next.(model)

	if got.summary != "摘要" {
		t.Errorf("摘要应落地, got %q", got.summary)
	}
	if len(got.history) != 2 {
		t.Fatalf("历史应同时被截断成 2 条, got %d 条: %+v", len(got.history), got.history)
	}
	if got.history[0].Role == "system" {
		t.Error("首条 system 应被剥掉(每轮由 BuildSystemPrompt 现建)")
	}
	if got.history[0].Content != "新任务" {
		t.Errorf("截断后应只剩最近一轮, got %q", got.history[0].Content)
	}
}

// 轮内压缩落地后影子必须作废(与会话级压缩同一套收尾):影子的切点是按压缩**前**的长历史算的,
// 留着可能在冷重启时拿过期 checkpoint 去截断已经压过的历史。
func TestCompactedMsg_InvalidatesShadow(t *testing.T) {
	m := compactedTestModel(nil)
	m.compactGen = 3
	m.shadowDonePct = 45

	got := mustModel(m.Update(agent.CompactedMsg{
		Summary: "摘要", Turns: 1,
		History: []agent.ChatMessage{{Role: "user", Content: "新任务"}},
	}))
	if got.compactGen != 4 {
		t.Errorf("代数应 +1(丢弃期间在飞的影子结果), got %d", got.compactGen)
	}
	if got.shadowDonePct != 0 {
		t.Errorf("影子档位应归零, got %d", got.shadowDonePct)
	}
}

// reclaim 落地要和自动压缩一样有反馈:留痕迹 + 刷状态栏。长任务(轮数≤2、压缩不触发)里
// reclaim 是唯一在减负的一层,它此前完全静默,用户看到百分比不动就以为没压成功(issue #201)。
func TestContextReclaimedMsg_LeavesTraceAndRefreshesUsage(t *testing.T) {
	m := compactedTestModel(&agent.UsageInfo{PromptTokens: 200_000, PromptCacheHitTokens: 90_000})

	next, _ := m.Update(agent.ContextReclaimedMsg{Count: 12, Tokens: 45_000, EstPromptTokens: 60_000})
	got := next.(model)

	if !strings.Contains(got.chatContent.String(), "已回收") {
		t.Errorf("对话区应留下回收痕迹, got=%q", got.chatContent.String())
	}
	if !strings.Contains(got.chatContent.String(), "12") {
		t.Error("痕迹应带回收条数")
	}
	if got.lastUsage.PromptTokens != 60_000 {
		t.Errorf("状态栏应降到回收后估算 60000, got %d", got.lastUsage.PromptTokens)
	}
	if got.lastUsage.PromptCacheHitTokens != 0 {
		t.Error("前缀已变,缓存命中数应清零")
	}
}

// 只降不升:回收后的估算若比真实值还大(估算口径偏低通常不会,但防御),不顶替真实值。
func TestContextReclaimedMsg_UsageOnlyShrinks(t *testing.T) {
	m := compactedTestModel(&agent.UsageInfo{PromptTokens: 100_000})
	next, _ := m.Update(agent.ContextReclaimedMsg{Count: 3, Tokens: 1000, EstPromptTokens: 130_000})
	if got := next.(model).lastUsage.PromptTokens; got != 100_000 {
		t.Errorf("估算更大时不应改动真实 usage, got %d", got)
	}
}

// History 缺失(异常/老消息)时只更新摘要,绝不把历史清空 —— 清空等于丢光上下文。
func TestCompactedMsg_EmptyHistoryKeepsLocal(t *testing.T) {
	m := compactedTestModel(nil)
	m.history = []agent.ChatMessage{{Role: "user", Content: "任务"}}

	next, _ := m.Update(agent.CompactedMsg{Summary: "摘要", Turns: 1})
	if got := next.(model); len(got.history) != 1 {
		t.Fatalf("没带 History 时不应动本地历史, got %d 条", len(got.history))
	}
}

// 压缩轮数为 0(切点前没有完整 user 轮)时仍要留痕迹 —— 否则又回到"悄无声息"。
func TestCompactedMsg_TraceWithoutTurns(t *testing.T) {
	m := compactedTestModel(nil)

	next, _ := m.Update(agent.CompactedMsg{Summary: "摘要", Turns: 0, EstPromptTokens: 1000})
	if !strings.Contains(next.(model).chatContent.String(), "已自动压缩会话历史") {
		t.Error("Turns=0 时也应留下压缩痕迹")
	}
}

// 轮内压缩期间活动状态行要切成「压缩中…」:RunCompression 最长卡 2 分钟且不吐 token,
// 不切的话用户看到普通流式 spinner 挂着不动,只当卡死了。成功/失败都要复位。
func TestCompactingMsg_TogglesFooterState(t *testing.T) {
	m := compactedTestModel(nil)

	next, _ := m.Update(agent.CompactingMsg{})
	started := next.(model)
	if !started.compactingInTurn {
		t.Fatal("收到 CompactingMsg 后活动状态行应切到「压缩中…」")
	}
	if !strings.Contains(started.statusFooterLine(80), "压缩中") {
		t.Errorf("footer 文案应是「压缩中…」, got=%q", started.statusFooterLine(80))
	}

	done, _ := started.Update(agent.CompactedMsg{Summary: "摘要", Turns: 2})
	if done.(model).compactingInTurn {
		t.Error("压缩成功后应复位,否则状态行一直挂着「压缩中…」")
	}

	failed, _ := started.Update(agent.CompactFailedMsg{Reason: "摘要请求超时"})
	got := failed.(model)
	if got.compactingInTurn {
		t.Error("压缩失败后也应复位")
	}
	if !strings.Contains(got.chatContent.String(), "自动压缩失败") {
		t.Errorf("自动压缩失败不该静默,应留一行, got=%q", got.chatContent.String())
	}
	if !strings.Contains(got.chatContent.String(), "摘要请求超时") {
		t.Error("失败提示应带上原因")
	}
}

// 中断 / 出错时消息可能被 drainAndDiscard 丢掉,状态位必须有兜底复位 —— 否则活动状态行
// 会永远挂在「压缩中…」。
func TestCompactingInTurn_ClearedOnTurnEnd(t *testing.T) {
	for _, c := range []struct {
		name string
		msg  tea.Msg
	}{
		{"流结束", agent.StreamDoneMsg{}},
		{"流出错", agent.StreamErrMsg{Err: errTest}},
	} {
		m := compactedTestModel(nil)
		m.compactingInTurn = true
		next, _ := m.Update(c.msg)
		if next.(model).compactingInTurn {
			t.Errorf("%s 时应兜底复位 compactingInTurn", c.name)
		}
	}
}

var errTest = errors.New("boom")

// 只降不升:估算口径比真实 usage 偏低(不含图片渲染追加、消息结构开销),
// 压缩后的估算若比真实值还大,说明真实值仍有效,不能拿估算顶替它。
func TestCompactedMsg_UsageOnlyShrinks(t *testing.T) {
	m := compactedTestModel(&agent.UsageInfo{PromptTokens: 100_000})

	next, _ := m.Update(agent.CompactedMsg{Summary: "摘要", Turns: 3, EstPromptTokens: 150_000})
	if got := next.(model).lastUsage.PromptTokens; got != 100_000 {
		t.Errorf("估算更大时不应改动真实 usage, got %d", got)
	}

	// 估算缺失(老 agent / 异常)时同样不动真实值。
	m2 := compactedTestModel(&agent.UsageInfo{PromptTokens: 100_000})
	next2, _ := m2.Update(agent.CompactedMsg{Summary: "摘要", Turns: 3})
	if got := next2.(model).lastUsage.PromptTokens; got != 100_000 {
		t.Errorf("没带估算时不应改动真实 usage, got %d", got)
	}
}
