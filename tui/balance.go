package tui

import (
	"context"

	"deepx/agent"

	tea "charm.land/bubbletea/v2"
)

// 账户余额查询的接入层(配合右栏「模型厂商」段展示):
//
// 仅 DeepSeek / Kimi 提供可凭模型 Key 调用的余额接口(见 agent.ProbeBalance);其它供应商
// 回 Supported=false → 右栏显示 "-"。每次启动、每次 /config 改配置、每次 /provider 切换都重探一次,
// 结果经 balanceMsg 回灌当前会话(异步,拿不到不阻塞 UI)。

// balanceMsg 是一次余额查询的回执。
type balanceMsg struct {
	display   string // 可渲染金额串(supported 时有效)
	supported bool
}

// balanceProbeCmd 用当前 flash(无 key 则退 pro)的配置查余额。无 key/base_url 直接返回 nil 不发命令。
func balanceProbeCmd(models agent.ModelConfig) tea.Cmd {
	entry := models.Flash
	if entry.APIKey == "" {
		entry = models.Pro
	}
	if entry.APIKey == "" || entry.BaseURL == "" {
		return nil
	}
	return func() tea.Msg {
		res, err := agent.ProbeBalance(context.Background(), entry)
		if err != nil {
			return nil // 瞬时错误 → 不更新,保留上次值
		}
		return balanceMsg{display: res.Display, supported: res.Supported}
	}
}

// applyBalance 把回执落到当前会话:支持则存金额串,不支持存 "-"。
func (m *model) applyBalance(msg balanceMsg) {
	if msg.supported {
		m.balance = msg.display
		return
	}
	m.balance = "-"
}
