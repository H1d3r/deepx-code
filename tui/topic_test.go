package tui

import (
	"testing"

	"deepx/agent"
)

// 标签会被切在任意 chunk 边界上,所以逐字节穷举所有切法:可见文本和捕获到的主题都必须
// 与一次性喂入完全一致。两个真实 bug 是这个测试抓出来的 —— 闭合标签正好被切开时永远
// 收不了尾,以及标签落在段末时多吐一个换行。
func TestTopicFilter_ChunkBoundaryInvariant(t *testing.T) {
	cases := []struct {
		in    string
		want  string // 期望显示出来的正文
		topic string
		shift bool
	}{
		{"改好了。\n\n<topic shift=\"no\">修复分隔线对齐</topic>", "改好了。", "修复分隔线对齐", false},
		{"改好了。\n<topic shift=\"yes\">给博客加 RSS</topic>\n", "改好了。", "给博客加 RSS", true},
		{"纯正文,没有标签", "纯正文,没有标签", "", false},
		{"<topic>只有标签</topic>", "", "只有标签", false},
		{"前<topic>中</topic>后", "前后", "中", false},
		{"a<topic shift=yes>x</topic>", "a", "x", true},
		// 没闭合(被 max_tokens 截断 / 格式写错):原文必须还回来,不能把正文吞了
		{"正文\n<topic>半截", "正文\n<topic>半截", "", false},
		{"正文 <to", "正文 <to", "", false},
	}
	for _, c := range cases {
		for split := 0; split <= len(c.in); split++ {
			if split > 0 && split < len(c.in) && !utf8Boundary(c.in, split) {
				continue
			}
			var f topicFilter
			got := f.feed(c.in[:split]) + f.feed(c.in[split:]) + f.flush()
			if got != c.want {
				t.Errorf("split=%d in=%q\n got=%q\nwant=%q", split, c.in, got, c.want)
			}
			if f.topic != c.topic || f.shift != c.shift {
				t.Errorf("split=%d in=%q got topic=%q shift=%v, want %q %v",
					split, c.in, f.topic, f.shift, c.topic, c.shift)
			}
		}
	}
}

func utf8Boundary(s string, i int) bool { return s[i]&0xC0 != 0x80 }

func TestStripTopicTags(t *testing.T) {
	if got := stripTopicTags("正文\n\n<topic shift=\"no\">主题</topic>"); got != "正文" {
		t.Errorf("got %q, want %q", got, "正文")
	}
}

// 提醒 /new 的三个门:模型标了 shift、主题确实换了、上下文过线。少一个都不该弹,
// 同一个新主题也只弹一次。上下文那道门是这个功能的意义所在 —— 上下文还小的时候
// 提醒纯属噪音,所以专门盯住"只有 shift 没有量"这种情况。
func TestApplyTurnTopic_HintGating(t *testing.T) {
	const ctxWin = 131072
	overLine := agent.TopicHintTokens(ctxWin) + 1

	newM := func(prev string, used int) *model {
		return &model{
			topic:       prev,
			chatContent: newChatLog(maxChatBytes),
			lastUsage:   &agent.UsageInfo{PromptTokens: used},
		}
	}
	// hinted 返回本次是否往对话区打了系统提示。
	hinted := func(m *model, topic string, shift bool) bool {
		before := len(m.chatContent.segments)
		m.topicF = topicFilter{topic: topic, shift: shift}
		m.applyTurnTopic(ctxWin)
		return len(m.chatContent.segments) > before
	}

	t.Run("过线且切换才提醒", func(t *testing.T) {
		m := newM("老主题", overLine)
		if !hinted(m, "新主题", true) {
			t.Error("过线 + shift=yes,应该提醒")
		}
		if m.topic != "新主题" {
			t.Errorf("主题应更新为新主题,got %q", m.topic)
		}
	})
	t.Run("上下文没过线不提醒", func(t *testing.T) {
		m := newM("老主题", 1000)
		if hinted(m, "新主题", true) {
			t.Error("上下文还小,提醒是噪音")
		}
		if m.topic != "新主题" {
			t.Error("不提醒也要更新右栏主题")
		}
	})
	t.Run("shift=no 不提醒", func(t *testing.T) {
		if hinted(newM("老主题", overLine), "换了个说法的老主题", false) {
			t.Error("模型说没切主题,就不该提醒")
		}
	})
	t.Run("会话第一轮不提醒", func(t *testing.T) {
		if hinted(newM("", overLine), "第一个主题", true) {
			t.Error("没有上一个主题可比,不该提醒")
		}
	})
	t.Run("同一主题不重复提醒", func(t *testing.T) {
		m := newM("老主题", overLine)
		if !hinted(m, "新主题", true) {
			t.Fatal("第一次应该提醒")
		}
		// 用户不理会、继续聊这个新话题:模型下一轮 shift 回到 no,不该再响。
		if hinted(m, "新主题的子问题", false) {
			t.Error("同话题追问不该再提醒")
		}
		// 即使模型抽风又标了 shift=yes,主题串没变也不该响。
		if hinted(m, "新主题的子问题", true) {
			t.Error("主题没变就不该提醒")
		}
	})
	t.Run("没有标签时静默降级", func(t *testing.T) {
		m := newM("老主题", overLine)
		if hinted(m, "", true) {
			t.Error("模型没输出标签,不该有任何动静")
		}
		if m.topic != "老主题" {
			t.Error("没识别到主题时右栏保持旧值")
		}
	})
}

func TestLastTopicOf(t *testing.T) {
	h := []agent.ChatMessage{
		{Role: "user", Content: "问题一"},
		{Role: "assistant", Content: "答一 <topic>老主题</topic>"},
		{Role: "user", Content: "问题二"},
		{Role: "assistant", Content: "答二 <topic>新主题</topic>"},
		{Role: "assistant", Content: ""}, // 纯工具调用轮,没有正文
	}
	if got := lastTopicOf(h); got != "新主题" {
		t.Errorf("got %q, want %q", got, "新主题")
	}
	if got := lastTopicOf(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
