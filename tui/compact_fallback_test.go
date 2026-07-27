package tui

import (
	"errors"
	"strings"
	"testing"

	"deepx/agent"
)

// B:手动 /compact 压不动(轮数不足)时,若 reclaim 兜底回收了工具输出,
// 应替换 history、刷状态栏、提示回收量,而不是干巴巴报"压缩跳过"。
func TestCompressionResult_ReclaimFallback(t *testing.T) {
	m := model{}
	m.chatContent = newChatLog(1 << 20)
	m.lastUsage = &agent.UsageInfo{PromptTokens: 200_000, PromptCacheHitTokens: 90_000}
	// reclaim 就地改副本、不增删消息,故回收后长度与触发时的 m.history 相等 —— 过期保护正是靠这个等长
	// 判断。测试数据须反映这点:同样 2 条,回收前带原文、回收后带引用标记。
	m.history = []agent.ChatMessage{
		{Role: "user", Content: "任务"},
		{Role: "tool", Content: "一大段原始工具输出…"},
	}
	newHist := []agent.ChatMessage{
		{Role: "user", Content: "任务"},
		{Role: "tool", Content: "[已回收] Bash 的旧输出…"},
	}
	next, _ := m.Update(compressionResultMsg{
		err:              errors.New("历史不足 15% 窗口,无需压缩"),
		manual:           true,
		reclaimedCount:   7,
		reclaimedFreed:   40_000,
		reclaimedHistory: newHist,
	})
	got := next.(model)

	if len(got.history) != 2 || got.history[0].Content != "任务" {
		t.Fatalf("history 应被替换为回收后的版本, got %+v", got.history)
	}
	if !strings.Contains(got.chatContent.String(), "已改回收 7 处") {
		t.Errorf("应提示回收量而非'压缩跳过', got=%q", got.chatContent.String())
	}
	if strings.Contains(got.chatContent.String(), "压缩跳过") {
		t.Error("有 reclaim 兜底时不该再报'压缩跳过'")
	}
	// 提示里要带**真实**失败原因:这个分支对任何压缩失败都会走到,写死"轮数不足"会误导
	// (轮数按 user|assistant 计之后,失败多是"历史不足 N% 窗口"或摘要超时)。
	if !strings.Contains(got.chatContent.String(), "历史不足 15% 窗口") {
		t.Errorf("提示应带上真实失败原因, got=%q", got.chatContent.String())
	}
	if strings.Contains(got.chatContent.String(), "轮数不足") {
		t.Errorf("不该把原因写死成'轮数不足', got=%q", got.chatContent.String())
	}
}

// 压缩完成提示:轮内切点压掉的可能整个都在同一个 user 轮内部,真实 user 轮数为 0 ——
// 长任务会话第二次以后的压缩必然如此(live 实测坐实)。这时显示"0 轮→摘要"看着像没压成。
func TestCompactDoneNote(t *testing.T) {
	cases := []struct {
		auto      bool
		turns     int
		want      string
		wantNoSub string
	}{
		{false, 3, "已压缩会话历史（3 轮 → 摘要）", "0 轮"},
		{false, 0, "已压缩会话历史（摘要已更新）", "0 轮"},
		{true, 6, "已自动压缩会话历史（6 轮 → 摘要）", "0 轮"},
		{true, 0, "已自动压缩会话历史（摘要已更新）", "0 轮"},
	}
	for _, c := range cases {
		got := compactDoneNote(c.auto, c.turns)
		if got != c.want {
			t.Errorf("compactDoneNote(%v, %d) = %q, want %q", c.auto, c.turns, got, c.want)
		}
		if strings.Contains(got, c.wantNoSub) {
			t.Errorf("提示不该出现 %q: %q", c.wantNoSub, got)
		}
	}
}

// 压缩压不动、reclaim 也没得回收(碎片/无大输出):回落到原来的"压缩跳过"提示。
func TestCompressionResult_NoReclaimFallsBackToSkip(t *testing.T) {
	m := model{}
	m.chatContent = newChatLog(1 << 20)

	next, _ := m.Update(compressionResultMsg{
		err:    errors.New("user 轮数不足,无需压缩"),
		manual: true,
		// reclaimedCount == 0
	})
	if !strings.Contains(next.(model).chatContent.String(), "压缩跳过") {
		t.Error("reclaim 也没回收时应报'压缩跳过'")
	}
}

// 自动压缩失败提示:两种处置的说法必须不同。原来统一写"本轮改用撞窗口保护"——
// 既看不懂,又偏乐观(那不是防护,是硬撑到撞满窗口由 API 400 收场)。
func TestCompactFailNote(t *testing.T) {
	const reason = "历史不足 15% 窗口,无需压缩"

	retry := compactFailNote(reason, true)
	if !strings.Contains(retry, reason) || !strings.Contains(retry, "重试") {
		t.Errorf("会重试时应说明稍后重试, got %q", retry)
	}
	if strings.Contains(retry, "本轮不再压缩") {
		t.Errorf("会重试时不该说本轮不再压缩, got %q", retry)
	}

	give := compactFailNote(reason, false)
	if !strings.Contains(give, reason) || !strings.Contains(give, "本轮不再压缩") {
		t.Errorf("不再重试时应说明本轮不再压缩, got %q", give)
	}
	if !strings.Contains(give, "增长") {
		t.Errorf("应说清后果(上下文会继续涨), got %q", give)
	}
	for _, s := range []string{retry, give} {
		if strings.Contains(s, "撞窗口保护") {
			t.Errorf("不该再出现看不懂的'撞窗口保护', got %q", s)
		}
	}
}
