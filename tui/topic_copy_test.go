package tui

import (
	"strings"
	"testing"
)

// === issue #226 回归 ===
//
// model.Update 是值接收者,bubbletea 每次事件都整份拷贝 model(topicF 是值字段)。
// 既有的 TestTopicFilter_ChunkBoundaryInvariant 已经覆盖了跨 chunk 切分,但它全程
// 用同一个 topicFilter 走指针方法,没有值拷贝 —— 所以功能逻辑测到了,内存语义没测到,
// strings.Builder 的 copyCheck panic 就这么漏了出去。
//
// 下面两个用例专门在每次 feed 之间插入一次值拷贝,复刻 Update 的真实行为。

// feedCopied 逐段喂入 chunks,每段之前把过滤器值拷贝到一个**新地址**上再处理,
// 复刻 model.Update 的完整来回:拿到一份拷贝 → 在拷贝上处理 → 拷贝成为新状态。
//
// 每份副本都必须落在不同地址,否则测不出东西:如果写成循环内 `cp := f`,
// 编译器会让 cp 每轮复用同一个栈槽,strings.Builder 的 copyCheck 比的是
// "addr 是否等于自身地址",地址没变就不会触发 —— 这样的测试在有 bug 的代码上
// 照样通过。所以这里显式堆分配,并用 keep 持有全部历史副本挡住分配器复用地址。
func feedCopied(chunks []string) (string, topicFilter) {
	var out strings.Builder
	cur := new(topicFilter)
	keep := []*topicFilter{cur}
	for _, c := range chunks {
		next := new(topicFilter)
		*next = *cur // 值拷贝
		keep = append(keep, next)
		out.WriteString(next.feed(c))
		cur = next
	}
	out.WriteString(cur.flush())
	_ = keep
	return out.String(), *cur
}

// TestTopicFilter_SurvivesValueCopy 验证标签被切在 chunk 之间、且过滤器每轮被值拷贝时,
// 既不 panic,剥离与捕获的结果也和不拷贝时一致。
func TestTopicFilter_SurvivesValueCopy(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
	}{
		// 切在开标记之后 —— 退出 feed 时 state=Open、raw 非空,原实现在这里崩
		{"切在 <topic 之后", []string{"修好了。\n<topic", ` shift="no">路由优化</topic>`}},
		// 切在属性串中间
		{"切在属性中间", []string{"修好了。\n<topic shift=", `"yes">新话题</topic>`}},
		// 切在标签体中间 —— 退出 feed 时 state=Body、body 非空
		{"切在标签体中间", []string{"修好了。\n<topic shift=\"no\">路由", "优化</topic>"}},
		// 切在闭合标签中间
		{"切在闭合标签中间", []string{"修好了。\n<topic shift=\"no\">路由优化</to", "pic>"}},
		// 逐字节喂,最极端
		{"逐字节", strings.Split("修好了。\n<topic shift=\"no\">路由优化</topic>", "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, f := feedCopied(c.chunks)
			if strings.Contains(got, "<topic") || strings.Contains(got, "</topic>") {
				t.Errorf("标签应被剥离干净, got %q", got)
			}
			if got != "修好了。" {
				t.Errorf("可见正文 = %q, want %q", got, "修好了。")
			}
			if f.topic == "" {
				t.Error("应捕获到主题")
			}
		})
	}
}

// TestTopicFilter_ValueCopyMatchesNoCopy 直接对拍:同一串 chunk,拷贝与不拷贝
// 必须得到完全一样的可见输出和主题,确保修复没有改变原有语义。
func TestTopicFilter_ValueCopyMatchesNoCopy(t *testing.T) {
	chunks := []string{"先看一下。\n", "<top", `ic shift="yes">`, "重构", "路由", "</topic>"}

	copied, fc := feedCopied(chunks)

	var direct strings.Builder
	var fd topicFilter
	for _, c := range chunks {
		direct.WriteString(fd.feed(c))
	}
	direct.WriteString(fd.flush())

	if copied != direct.String() {
		t.Errorf("可见输出不一致: 拷贝 %q vs 不拷贝 %q", copied, direct.String())
	}
	if fc.topic != fd.topic || fc.shift != fd.shift {
		t.Errorf("主题捕获不一致: 拷贝 (%q,%v) vs 不拷贝 (%q,%v)", fc.topic, fc.shift, fd.topic, fd.shift)
	}
}

// TestTopicFilter_UnclosedTagValueCopy 标签没闭合(模型被截断)时,flush 要把吃进去的
// 原文原样还回来 —— 这条路径读 f.raw,同样要扛得住值拷贝。
func TestTopicFilter_UnclosedTagValueCopy(t *testing.T) {
	got, _ := feedCopied([]string{"正文。\n<topic", ` shift="no">没闭合`})
	if !strings.Contains(got, "正文。") {
		t.Errorf("正文不能被吞掉, got %q", got)
	}
	if !strings.Contains(got, "<topic") {
		t.Errorf("未闭合标签应原样还回, got %q", got)
	}
}
