package agent

import (
	"strings"
	"testing"
)

// 工作模式渲染的核心不变量是「前缀缓存友好」:每条 user 消息按**它自己记录的**模式渲染后缀,
// 切换当前模式不改写历史消息 → 历史逐字节稳定。下面的测试就盯这个。

// 历史消息各自带不同模式标签时,应各按自己的标签渲染,互不串味。
func TestRenderWorkingMode_PerMessageTag(t *testing.T) {
	convo := []ChatMessage{
		{Role: "user", Content: "第一轮", WorkingMode: WorkingModeKarpathy},
		{Role: "assistant", Content: "回复一"},
		{Role: "user", Content: "第二轮", WorkingMode: WorkingModeOpenSpec},
		{Role: "user", Content: "第三轮", WorkingMode: WorkingModeSuperpowers},
	}
	out := renderWorkingMode(convo, WorkingModeKarpathy)

	if !strings.Contains(out[0].Content, "karpathy-guidelines") {
		t.Errorf("第一轮应渲染 karpathy 后缀,得到:%q", out[0].Content)
	}
	// 用模式横幅判别(后缀正文里会提到别的模式名做"不要使用"约束,不能用 skill 名判别)
	if !strings.Contains(out[2].Content, "[工作模式 openspec]") || strings.Contains(out[2].Content, "[工作模式 karpathy]") {
		t.Errorf("第二轮应只渲染 openspec 后缀,得到:%q", out[2].Content)
	}
	if !strings.Contains(out[3].Content, "superpowers") {
		t.Errorf("第三轮应渲染 superpowers 后缀,得到:%q", out[3].Content)
	}
	// assistant 消息不应被改动
	if out[1].Content != "回复一" {
		t.Errorf("assistant 消息不应被改动,得到:%q", out[1].Content)
	}
	// 不写回原 convo
	if convo[0].Content != "第一轮" {
		t.Errorf("renderWorkingMode 不应改动原 convo,得到:%q", convo[0].Content)
	}
}

// 缓存关键:切换「当前模式」(fallback)不应改变已带标签的历史消息的渲染结果 —— 逐字节一致。
func TestRenderWorkingMode_SwitchModeKeepsHistoryStable(t *testing.T) {
	history := []ChatMessage{
		{Role: "user", Content: "老消息", WorkingMode: WorkingModeKarpathy},
		{Role: "assistant", Content: "回复"},
	}
	// 第一次发送时当前模式是 kp
	a := renderWorkingMode(history, WorkingModeKarpathy)
	// 之后用户切到 superpowers 又发了一轮;老消息标签没变,只是 fallback 变了
	b := renderWorkingMode(history, WorkingModeSuperpowers)

	if a[0].Content != b[0].Content {
		t.Errorf("切换当前模式后历史消息渲染应逐字节不变(缓存依赖此):\n  before=%q\n  after =%q",
			a[0].Content, b[0].Content)
	}
}

// 无标签(老 gob / exec 入口)的消息用 fallback 渲染,且 fallback 确定 → 字节稳定。
func TestRenderWorkingMode_UntaggedUsesFallback(t *testing.T) {
	convo := []ChatMessage{{Role: "user", Content: "无标签"}}

	out := renderWorkingMode(convo, WorkingModeOpenSpec)
	if !strings.Contains(out[0].Content, "openspec") {
		t.Errorf("无标签消息应按 fallback(openspec)渲染,得到:%q", out[0].Content)
	}
	// fallback 为空 → 归一到默认 kp(不 panic、不留空)
	out2 := renderWorkingMode([]ChatMessage{{Role: "user", Content: "x"}}, "")
	if !strings.Contains(out2[0].Content, "karpathy-guidelines") {
		t.Errorf("空 fallback 应归一到默认 kp,得到:%q", out2[0].Content)
	}
}
