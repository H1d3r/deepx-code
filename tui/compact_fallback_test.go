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
		err:              errors.New("user 轮数不足,无需压缩"),
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
