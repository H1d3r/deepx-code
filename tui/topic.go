package tui

import (
	"fmt"
	"strings"

	"deepx/agent"
)

// 会话主题跟踪。模型每轮回复的最后一行固定输出:
//
//	<topic shift="yes|no">一句话主题</topic>
//
// 这行是给 DeepX 看的元信息、不是给用户看的正文,显示前要剥掉。剥离发生在流式增量上,
// 标签可能被切在任意两个 chunk 之间(甚至切在 "<to" / "pic>" 中间),所以用状态机而不是正则。
//
// 为什么「语义判断交给模型、条件动作交给代码」:实测让模型自己"发现主题变了就改口提醒
// 用户 /new",换了三种措辞、多轮测试一次都没触发过 —— 它总是优先去帮用户干活,条件分支被
// 稀释掉;而让它无条件多吐一个属性则稳定生效(含有工具调用的轮次)。所以模型只负责如实标注,
// 要不要提示用户由 StreamDoneMsg 里的代码决定(还要叠一个它不知道的条件:上下文用量)。
const (
	topicOpen  = "<topic" // 开标签只匹配到名字,后面可能跟 shift 属性
	topicClose = "</topic>"
)

// topicFilter 的三个状态:正文 / 开标签内(<topic 之后、> 之前)/ 标签体内。
const (
	topicStateText = iota
	topicStateOpen
	topicStateBody
)

type topicFilter struct {
	state   int
	pending string          // 未决尾巴:可能是开标签的部分前缀,或标签前的空白
	raw     strings.Builder // 开标签以来吃掉的原文,标签没闭合时用它还原,不吞正文
	body    strings.Builder // 标签体
	attr    string          // 开标签里的属性串

	topic    string // 最近一次捕获到的主题
	shift    bool   // 该次捕获是否标了 shift="yes"
	trimLead bool   // 刚吃掉一个标签,紧跟其后的空白也要吞掉
}

// feed 吃进一段流式增量,返回其中应当显示给用户的部分。
func (f *topicFilter) feed(chunk string) string {
	var out strings.Builder
	s := f.pending + chunk
	f.pending = ""
	for s != "" {
		switch f.state {
		case topicStateText:
			i := strings.Index(s, topicOpen)
			if i < 0 {
				keep := topicHoldBack(s)
				out.WriteString(f.emit(s[:len(s)-keep]))
				f.pending = s[len(s)-keep:]
				return out.String()
			}
			// 标签前的空白连标签一起丢,否则回复末尾会留下空行;丢掉的空白进 raw,
			// 万一标签没闭合还能原样还回去。
			lead := s[:i]
			kept := strings.TrimRight(lead, " \t\r\n")
			out.WriteString(f.emit(kept))
			s = s[i+len(topicOpen):]
			f.raw.Reset()
			f.raw.WriteString(lead[len(kept):] + topicOpen)
			f.state = topicStateOpen

		case topicStateOpen:
			j := strings.IndexByte(s, '>')
			if j < 0 {
				f.attr += s
				f.raw.WriteString(s)
				return out.String()
			}
			f.attr += s[:j]
			f.raw.WriteString(s[:j+1])
			s = s[j+1:]
			f.state = topicStateBody

		case topicStateBody:
			// 闭合标签同样可能被切开,所以在累积的标签体里找,而不是只在本段里找。
			f.body.WriteString(s)
			f.raw.WriteString(s)
			before, after, found := strings.Cut(f.body.String(), topicClose)
			if !found {
				return out.String()
			}
			s = after
			f.body.Reset()
			f.body.WriteString(before)
			f.commit()
			f.state = topicStateText
		}
	}
	return out.String()
}

// flush 收尾:吐出未决尾巴。标签没闭合(模型被截断 / 写错格式)时把吃进去的原文原样还回去 ——
// 宁可让用户看到半个标签,也不能把正文吞掉。
func (f *topicFilter) flush() string {
	out := f.emit(f.pending)
	f.pending = ""
	if f.state != topicStateText {
		out += f.raw.String()
		f.raw.Reset()
		f.body.Reset()
		f.attr = ""
		f.state = topicStateText
	}
	return out
}

// emit 输出正文。标签独占一行,它前后的换行都是为它服务的:前面的空白由调用方 TrimRight 掉,
// 后面的空白在这里 TrimLeft 掉,不给回复留下空行。
func (f *topicFilter) emit(s string) string {
	if !f.trimLead {
		return s
	}
	t := strings.TrimLeft(s, " \t\r\n")
	if t != "" {
		f.trimLead = false
	}
	return t
}

func (f *topicFilter) commit() {
	if t := strings.TrimSpace(f.body.String()); t != "" {
		f.topic = t
		f.shift = topicAttrYes(f.attr)
	}
	f.body.Reset()
	f.raw.Reset()
	f.attr = ""
	f.trimLead = true
}

// topicHoldBack 返回尾部要留到下一段再定夺的字节数:可能是 "<topic" 的部分前缀,
// 以及它前面的空白(标签真的来了就连空白一起丢)。
func topicHoldBack(s string) int {
	n := 0
	for k := len(topicOpen) - 1; k > 0; k-- {
		if len(s) >= k && strings.HasSuffix(s, topicOpen[:k]) {
			n = k
			break
		}
	}
	rest := s[:len(s)-n]
	w := 0
	for w < len(rest) && isASCIISpaceByte(rest[len(rest)-1-w]) {
		w++
	}
	return n + w
}

func isASCIISpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// topicAttrYes 解析 shift 属性,容忍 shift="yes" / shift='yes' / shift=yes / shift=true。
func topicAttrYes(attr string) bool {
	a := strings.ToLower(attr)
	i := strings.Index(a, "shift")
	if i < 0 {
		return false
	}
	a = strings.TrimLeft(a[i+len("shift"):], " \t=\"'")
	return strings.HasPrefix(a, "yes") || strings.HasPrefix(a, "true")
}

// stripTopicTags 一次性剥离整段文本里的主题标签,用于从 history 重放对话区。
func stripTopicTags(s string) string {
	var f topicFilter
	return strings.TrimRight(f.feed(s)+f.flush(), " \t\r\n")
}

// applyTurnTopic 落地本轮捕获到的主题,并决定要不要提醒用户开新会话。
//
// 三个条件同时成立才提醒:模型标了 shift="yes"、主题确实换了、上下文用量已经过线。
// 最后一条是关键 —— 上下文还小的时候换不换会话都无所谓,提醒纯属噪音;过了线,旧话题的
// 上下文对新问题就只是噪音,还会被接下来的压缩花钱有损保留下来,这时候提醒最值钱。
//
// 不用额外记「提醒过没有」:shift 是**相对上一轮**的增量信号,用户不理会、继续聊这个新话题时
// 下一轮 shift 自然变回 "no",所以一次切换天然只响一次;真再响一次,那就是真又切了一次话题。
func (m *model) applyTurnTopic(ctxWin int) {
	t := m.topicF.topic
	if t == "" {
		return // 模型没输出标签(小模型/被截断):静默降级,右栏保持旧值
	}
	prev := m.topic
	m.topic = t
	if !m.topicF.shift || prev == "" || t == prev {
		return
	}
	used := m.lastPromptTokens()
	if ctxWin <= 0 || used < agent.TopicHintTokens(ctxWin) {
		return
	}
	m.chatContent.Open(kindSystem, fmt.Sprintf(T("topic.shift"), used*100/ctxWin))
}

// lastTopicOf 从历史里倒着找最近一条带主题标签的助手消息,用于冷启动 / 切会话后恢复右栏主题。
func lastTopicOf(history []agent.ChatMessage) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role != "assistant" || history[i].Content == "" {
			continue
		}
		var f topicFilter
		f.feed(history[i].Content)
		f.flush()
		if f.topic != "" {
			return f.topic
		}
	}
	return ""
}
