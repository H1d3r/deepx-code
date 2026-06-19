package tui

import (
	"deepx/agent"
	"deepx/config"
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// setupCustomFieldDefs 定义「其它」自定义表单的字段顺序与元信息。前 5 个属 flash,后 5 个属 pro。
var setupCustomFieldDefs = []struct {
	label       string
	placeholder string
	isInt       bool
}{
	{"base_url", "https://api.openai.com/v1", false},
	{"model", "gpt-4o-mini", false},
	{"api_key", "sk-...", false},
	{"max_tokens", "8192", true},
	{"context_window", "131072", true},
	{"base_url", "https://api.openai.com/v1", false},
	{"model", "gpt-4o", false},
	{"api_key", "sk-...", false},
	{"max_tokens", "8192", true},
	{"context_window", "131072", true},
}

// newSetupCustomFields 创建 10 个空的自定义字段输入框(只留 placeholder 提示,不预填当前配置 ——
// 预填后用户改的时候还得先删,反而麻烦)。焦点放第 0 个。
func newSetupCustomFields() []textinput.Model {
	fields := make([]textinput.Model, len(setupCustomFieldDefs))
	for i, def := range setupCustomFieldDefs {
		ti := textinput.New()
		ti.Prompt = "" // 去掉默认 "> ",避免和外层 [ ] 重复、并节省列宽
		ti.Placeholder = def.placeholder
		ti.CharLimit = 256
		ti.SetWidth(40)
		fields[i] = ti
	}
	if len(fields) > 0 {
		fields[0].Focus()
	}
	return fields
}

// atoiOr 把字符串解析为正整数;空 / 非法 / 非正 → 回退 def。
func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
		return n
	}
	return def
}

// overlayCentered 把 fg(modal)叠在 bg(主 UI)上居中显示。
// 实现:
//  1. 拆 bg 和 fg 成行;算出 fg 的最大显示宽度(以 ansi.StringWidth 测,跟终端实际渲染一致)
//  2. 居中位置:startY = (height - fgHeight)/2, startX = (width - fgWidth)/2
//  3. 对每一行 fg,用 ansi.Cut 把对应 bg 行的 [startX, startX+fgW) 区间挖掉换成 fg 内容
//  4. 重新 join 输出
//
// bg 太短(行数少于 startY+fgH)时,缺失行不补,modal 会被截断。这种情况下终端高度不够,
// 不在 modal 区也没什么意义。
func overlayCentered(bg, fg string, width, height int) string {
	fgLines := strings.Split(strings.TrimRight(fg, "\n"), "\n")
	fgH := len(fgLines)
	fgW := 0
	for _, ln := range fgLines {
		if w := ansi.StringWidth(ln); w > fgW {
			fgW = w
		}
	}

	startY := (height - fgH) / 2
	startX := (width - fgW) / 2
	if startY < 0 {
		startY = 0
	}
	if startX < 0 {
		startX = 0
	}

	bgLines := strings.Split(bg, "\n")
	for i, fgLine := range fgLines {
		y := startY + i
		if y < 0 || y >= len(bgLines) {
			continue
		}
		bgLines[y] = spliceLineCells(bgLines[y], fgLine, startX, fgW)
	}
	return strings.Join(bgLines, "\n")
}

// spliceLineCells 把 fg 的所有 cell 拼到 bg 的 [atCol, atCol+fgW) 区间,
// 保留 bg 在该区间前后的内容(连同 ANSI 转义)。
// 用 ansi.Cut 处理 ANSI 边界,避免 bg 的 SGR 状态污染 fg 或 fg 之后内容。
func spliceLineCells(bg, fg string, atCol, fgW int) string {
	pre := ansi.Cut(bg, 0, atCol)
	// bg 在 atCol 之前太短 → 补空格到 atCol 列,保证 fg 起始位置对齐
	if preW := ansi.StringWidth(pre); preW < atCol {
		pre += strings.Repeat(" ", atCol-preW)
	}
	post := ""
	if bgW := ansi.StringWidth(bg); atCol+fgW < bgW {
		post = ansi.Cut(bg, atCol+fgW, bgW)
	}
	return pre + fg + post
}

// providerDisplay 把供应商 id 映射成展示名(custom → 其它/Other)。
func providerDisplay(p string) string {
	if p == config.ProviderCustom {
		return T("setup.provider.custom")
	}
	return p
}

// curProvider 返回当前选中的供应商 id。
func (m model) curProvider() string {
	if m.setupProviderIdx >= 0 && m.setupProviderIdx < len(config.ProviderOptions) {
		return config.ProviderOptions[m.setupProviderIdx]
	}
	return config.ProviderOptions[0]
}

// setupModalBlock 只渲染 modal 本身(不放置),供 overlay 使用。
// 两步:setupStep==0 选供应商;==1 填配置(预设供应商单填 api_key,custom 填 10 字段表单)。
func (m model) setupModalBlock() string {
	// 标题 + 括号写明保存路径(去掉所有说明性提示文字)。
	savePath := "~/.deepx/model.yaml"
	if p, err := config.Path(); err == nil {
		savePath = abbreviatePath(p, 48)
	}
	title := lipgloss.NewStyle().Bold(true).Foreground(highlightColor).Render(T("setup.title")) +
		lipgloss.NewStyle().Foreground(dimColor).Render("  ("+fmt.Sprintf(T("setup.save_path_hint"), savePath)+")")

	var body, footer string
	if m.setupStep == 0 {
		// 供应商竖排,选中项行首高亮标记。
		providerLabel := lipgloss.NewStyle().Foreground(dimColor).Render(T("setup.provider_label"))
		rows := make([]string, 0, len(config.ProviderOptions))
		for i, p := range config.ProviderOptions {
			if i == m.setupProviderIdx {
				rows = append(rows, lipgloss.NewStyle().Foreground(highlightColor).Bold(true).Render("  ▸ "+providerDisplay(p)))
			} else {
				rows = append(rows, lipgloss.NewStyle().Foreground(subtleColor).Render("    "+providerDisplay(p)))
			}
		}
		body = providerLabel + "\n" + strings.Join(rows, "\n")
		footer = T("setup.footer.step_provider")
	} else {
		provName := lipgloss.NewStyle().Foreground(subtleColor).Render(T("setup.cur_provider") + " " + providerDisplay(m.curProvider()))
		if m.curProvider() == config.ProviderCustom {
			body = provName + "\n\n" + m.setupCustomFormBlock()
			footer = T("setup.footer.step_custom")
		} else {
			inputLabel := lipgloss.NewStyle().Foreground(dimColor).Render(T("setup.input_label"))
			body = provName + "\n\n" + inputLabel + "\n  " + m.setupInput.View()
			footer = T("setup.footer.step_preset")
		}
	}

	parts := []string{title, "", body}
	if m.setupErr != "" {
		parts = append(parts, "", lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("✗ "+m.setupErr))
	}
	parts = append(parts, "", lipgloss.NewStyle().Foreground(dimColor).Render(footer))

	content := lipgloss.JoinVertical(lipgloss.Left, parts...)

	// 整框 72(含边框+内边距);内容区 = 72-2-4 = 66,容得下自定义表单每行(标签+方括号输入框)不换行。
	modalWidth := 72
	if maxW := m.width - 4; modalWidth > maxW {
		modalWidth = maxW
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(highlightColor).
		Padding(1, 2).
		Width(modalWidth).
		Render(content)
}

// setupCustomFormBlock 渲染「其它」自定义的 10 字段表单:标签右对齐 + 方括号输入框,
// 焦点字段行首 ▸ 并高亮括号,flash/pro 分组。常规表单观感。
func (m model) setupCustomFormBlock() string {
	const labelW = 14
	var b strings.Builder
	for i, def := range setupCustomFieldDefs {
		if i == 0 {
			b.WriteString(lipgloss.NewStyle().Foreground(dimColor).Bold(true).Render("Flash") + "\n")
		}
		if i == 5 {
			b.WriteString("\n" + lipgloss.NewStyle().Foreground(dimColor).Bold(true).Render("Pro") + "\n")
		}
		focused := i == m.setupFieldIdx

		marker := "  "
		labelStyle := lipgloss.NewStyle().Foreground(subtleColor).Width(labelW).Align(lipgloss.Right)
		bracketStyle := lipgloss.NewStyle().Foreground(dimColor)
		if focused {
			marker = lipgloss.NewStyle().Foreground(highlightColor).Render("▸ ")
			labelStyle = labelStyle.Foreground(highlightColor)
			bracketStyle = lipgloss.NewStyle().Foreground(highlightColor)
		}

		view := ""
		if i < len(m.setupCustomFields) {
			view = m.setupCustomFields[i].View()
		}
		field := bracketStyle.Render("[ ") + view + bracketStyle.Render(" ]")
		b.WriteString(marker + labelStyle.Render(def.label) + "  " + field + "\n")
	}
	return b.String()
}

// buildCustomConfig 从自定义表单的 10 个字段构造 Config。
// flash 必须填全 base_url/model/api_key;pro 的 base_url/api_key 留空则继承 flash、model 留空则同 flash。
// max_tokens / context_window 留空用通用默认。返回 (cfg, "") 成功,(nil, errMsg) 失败。
func (m *model) buildCustomConfig() (*config.Config, string) {
	v := func(i int) string {
		if i < len(m.setupCustomFields) {
			return strings.TrimSpace(m.setupCustomFields[i].Value())
		}
		return ""
	}
	flash := config.ModelEntry{
		BaseURL:       v(0),
		Model:         v(1),
		APIKey:        v(2),
		MaxTokens:     atoiOr(v(3), config.CustomDefaultMaxTokens),
		ContextWindow: atoiOr(v(4), config.CustomDefaultContextWindow),
	}
	if flash.BaseURL == "" || flash.Model == "" || flash.APIKey == "" {
		return nil, T("setup.error.custom_flash")
	}
	pro := config.ModelEntry{
		BaseURL:       v(5),
		Model:         v(6),
		APIKey:        v(7),
		MaxTokens:     atoiOr(v(8), config.CustomDefaultMaxTokens),
		ContextWindow: atoiOr(v(9), config.CustomDefaultContextWindow),
	}
	if pro.BaseURL == "" {
		pro.BaseURL = flash.BaseURL
	}
	if pro.APIKey == "" {
		pro.APIKey = flash.APIKey
	}
	if pro.Model == "" {
		pro.Model = flash.Model
	}
	return &config.Config{Flash: flash, Pro: pro}, ""
}

// focusCustomField 把焦点移到第 idx 个自定义字段(环绕越界),其余 Blur。
func (m *model) focusCustomField(idx int) {
	n := len(m.setupCustomFields)
	if n == 0 {
		return
	}
	idx = (idx%n + n) % n
	for i := range m.setupCustomFields {
		m.setupCustomFields[i].Blur()
	}
	m.setupCustomFields[idx].Focus()
	m.setupFieldIdx = idx
}

// submitSetup 处理 modal 内 Enter 的提交逻辑:
//   - 校验输入非空
//   - 按选中的供应商用 config.DefaultFor 构造 yaml
//   - 落盘
//   - 重新 Load(保证内存版本和磁盘一致)
//   - 把 model 内的 m.models 替换为新配置
//   - 关闭 modal,把焦点交回主输入框
//
// 失败时设置 setupErr,modal 留着等用户重试。
func (m *model) submitSetup() tea.Cmd {
	provider := m.curProvider()

	var cfg *config.Config
	if provider == config.ProviderCustom {
		built, errMsg := m.buildCustomConfig()
		if errMsg != "" {
			m.setupErr = errMsg
			return nil
		}
		cfg = built
	} else {
		val := strings.TrimSpace(m.setupInput.Value())
		if val == "" {
			m.setupErr = T("setup.error.empty")
			return nil
		}
		cfg = config.DefaultFor(provider, val) // 预设供应商:套 modelConfig 默认 + 该 key
	}
	if err := config.Save(cfg); err != nil {
		m.setupErr = fmt.Sprintf(T("setup.error.save"), err)
		return nil
	}
	// 同步存档到 provider.yaml(按供应商名),供 /provider 快捷切换。存档失败不致命,不挡 /config。
	_ = config.SaveProvider(provider, cfg)
	loaded, err := config.Load()
	if err != nil {
		m.setupErr = fmt.Sprintf(T("setup.error.reload"), err)
		return nil
	}
	m.models = agent.ModelConfig{
		Flash: agent.ModelEntry(loaded.Flash),
		Pro:   agent.ModelEntry(loaded.Pro),
	}
	m.activeModelRole = "flash"
	m.activeModelID = m.models.Flash.Model
	if m.activeModelID == "" {
		m.activeModelRole = "pro"
		m.activeModelID = m.models.Pro.Model
	}
	// 模型换了:视觉能力可能变,重置 —— 先用新模型的缓存值垫初值,下面返回探测命令立刻重探。
	m.visionByModel = loadVisionCaps(m.models)
	// 重置 modal 状态
	m.showSetup = false
	m.setupRequired = false
	m.setupErr = ""
	m.setupStep = 0
	m.setupCustomFields = nil
	m.setupFieldIdx = 0
	m.setupInput.Reset()
	m.setupInput.Blur()
	m.input.Focus()

	path, _ := config.Path()
	// 反斜杠转义已在 renderMarkdown 渲染层统一处理(见 backslashSentinel),这里不必再包反引号。
	m.appendChat("System", T("setup.saved_to")+path)
	// 对新配置的模型重探视觉能力(结果经 visionCapMsg 回灌当前会话 + 覆盖缓存)。
	// 余额也重探:换了供应商/Key,旧值已无意义,先清空待新值回灌。
	m.balance = ""
	cmds := visionProbeCmds(m.models)
	if cmd := balanceProbeCmd(m.models); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// openSetupModal 给 /config 命令用:把当前面板切到 modal(从选供应商那步开始),允许 Esc 取消。
func (m *model) openSetupModal() {
	m.showSetup = true
	m.setupRequired = false
	m.setupErr = ""
	m.setupStep = 0
	m.setupFieldIdx = 0
	m.setupCustomFields = nil
	m.setupInput.SetValue("")
	m.setupInput.Blur()
	m.input.Blur()
}

// handleProviderCommand 分发 /provider(弹选择器)与 /provider <名字>(直切)。
// 供应商名来自 provider.yaml(已 /config 过的才在);没有任何存档则提示先 /config。
func (m *model) handleProviderCommand(input string) tea.Cmd {
	names, err := config.ProviderNames()
	if err != nil {
		m.appendChat("System", fmt.Sprintf(T("provider.error.load"), err))
		return nil
	}
	if len(names) == 0 {
		m.appendChat("System", T("provider.empty"))
		return nil
	}
	// /provider <名字> → 直切
	if fields := strings.Fields(strings.TrimSpace(input)); len(fields) >= 2 {
		return m.applyProvider(strings.ToLower(fields[1]))
	}
	// 裸 /provider → 弹选择器,光标默认停在与当前 model.yaml 匹配的供应商上(匹配不到停 0)。
	m.providerNames = names
	m.providerModalIdx = 0
	for i, n := range names {
		if cfg, ok, _ := config.LoadProvider(n); ok &&
			cfg.Flash.Model == m.models.Flash.Model && cfg.Pro.Model == m.models.Pro.Model {
			m.providerModalIdx = i
			break
		}
	}
	m.showProviderModal = true
	return nil
}

// applyProvider 把 provider.yaml 中指定供应商的 flash/pro 写回 model.yaml 并热切换:
// 重载、刷新 m.models、重探视觉。返回视觉探测命令。逻辑与 submitSetup 的模型更新段一致。
func (m *model) applyProvider(name string) tea.Cmd {
	cfg, ok, err := config.LoadProvider(name)
	if err != nil {
		m.appendChat("System", fmt.Sprintf(T("provider.error.load"), err))
		return nil
	}
	if !ok {
		m.appendChat("System", fmt.Sprintf(T("provider.unknown"), name))
		return nil
	}
	if err := config.Save(cfg); err != nil {
		m.appendChat("System", fmt.Sprintf(T("provider.error.save"), err))
		return nil
	}
	loaded, err := config.Load()
	if err != nil {
		m.appendChat("System", fmt.Sprintf(T("provider.error.save"), err))
		return nil
	}
	m.models = agent.ModelConfig{
		Flash: agent.ModelEntry(loaded.Flash),
		Pro:   agent.ModelEntry(loaded.Pro),
	}
	m.activeModelRole = "flash"
	m.activeModelID = m.models.Flash.Model
	if m.activeModelID == "" {
		m.activeModelRole = "pro"
		m.activeModelID = m.models.Pro.Model
	}
	// 换了供应商 → 视觉能力可能变,先用缓存垫初值,再返回探测命令重探。
	m.visionByModel = loadVisionCaps(m.models)
	m.appendChat("System", fmt.Sprintf(T("provider.switched"), name, m.models.Flash.Model, m.models.Pro.Model))
	m.refreshViewport()
	// 换供应商 → 余额变了,先清空待重探回灌。
	m.balance = ""
	cmds := visionProbeCmds(m.models)
	if cmd := balanceProbeCmd(m.models); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}
