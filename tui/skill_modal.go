package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"deepx/skill"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// === /skill-add 重构为 search-and-install ===
//
// 单 modal 多阶段:
//   - 阶段 1 query:textinput 单行,Enter 提交
//       · 若输入是 GitHub URL / 本地路径 → 直接 skill.Install 走老路
//       · 否则 → 跨所有 enabled 源 SearchSkills,8s timeout
//   - 阶段 2 searching:loading 提示(spinner 复用主 m.spinner 帧)
//   - 阶段 3 results:列表,↑↓ 选,Enter 安装当前项(异步 tea.Cmd → skill.InstallFromSource)
//   - 阶段 4 installing:loading 提示;完成后关 modal,chat 出"✓ 装好"/"✗ 失败"
//
// Esc 永远关 modal(不分阶段)。
//
// === /skill-source-list ===  直接 chat 输出(像 /mcp-list),不开 modal
// === /skill-source-add  ===  单行输入 "[name] URL",Enter 保存
// === /skill-source-delete === 弹列表选删(内置 Clawhub 不入列)

// 异步消息:在 goroutine 跑完搜索 / 安装后送回 Update。
type skillSearchDoneMsg struct {
	query   string
	results []skill.RemoteSkillInfo
	err     error
}

type skillInstallDoneMsg struct {
	skillName string
	err       error
}

// === /skill-add ===

func (m *model) openSkillAddModal() {
	m.showSkillAdd = true
	m.skillAddErr = ""
	m.skillAddInput.SetValue("")
	m.skillAddInput.Focus()
	m.skillAddResults = nil
	m.skillAddIdx = 0
	m.skillAddSearching = false
	m.skillAddInstalling = false
	m.input.Blur()
}

// isDirectInstallSrc 判断输入是不是直装格式(URL / 本地路径)—— 是就跳过搜索,
// 复用老 Install 逻辑(git clone / 拷贝);否则跨源搜。
func isDirectInstallSrc(s string) bool {
	return strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "/") ||
		strings.HasPrefix(s, "./") ||
		strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "~")
}

// submitSkillAdd 阶段 1 Enter:派发到搜索或直装。
// 返回 tea.Cmd 在 goroutine 跑(避免 TUI 阻塞);完成后 skillSearchDoneMsg / skillInstallDoneMsg 回 Update。
func (m *model) submitSkillAdd() tea.Cmd {
	q := strings.TrimSpace(m.skillAddInput.Value())
	if q == "" {
		m.skillAddErr = "输入关键词搜索,或粘贴 GitHub URL / 本地路径直接安装"
		return nil
	}
	m.skillAddErr = ""
	if isDirectInstallSrc(q) {
		m.skillAddInstalling = true
		return func() tea.Msg {
			name, err := skill.Install(q)
			return skillInstallDoneMsg{skillName: name, err: err}
		}
	}
	m.skillAddSearching = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		results, err := skill.SearchSkills(ctx, q, "")
		return skillSearchDoneMsg{query: q, results: results, err: err}
	}
}

// installSelectedSkillResult 阶段 3 Enter:装当前选中项。
func (m *model) installSelectedSkillResult() tea.Cmd {
	if m.skillAddIdx < 0 || m.skillAddIdx >= len(m.skillAddResults) {
		return nil
	}
	r := m.skillAddResults[m.skillAddIdx]
	m.skillAddInstalling = true
	sourceID := r.SourceID
	remoteRef := r.RemoteRef
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		name, err := skill.InstallFromSource(ctx, sourceID, remoteRef)
		return skillInstallDoneMsg{skillName: name, err: err}
	}
}

// === /skill-delete(未变,仍按已装列表选删)===

func (m *model) openSkillDeleteModal() {
	list, err := skill.InstalledList()
	if err != nil {
		m.appendChat("System", "读取已装 skill 失败:"+err.Error())
		return
	}
	if len(list) == 0 {
		m.appendChat("System", "~/.deepx/skills/ 下没有 deepx 管理的 skill 可删除。"+
			"\n注:/skill-delete 只动 deepx 自己装的,~/.claude/skills 等其他工具的 skill 不会被列出。")
		return
	}
	m.skillDelNames = m.skillDelNames[:0]
	for _, s := range list {
		m.skillDelNames = append(m.skillDelNames, s.Name)
	}
	m.skillDelIdx = 0
	m.showSkillDelete = true
	m.input.Blur()
}

func (m *model) submitSkillDelete() {
	if m.skillDelIdx < 0 || m.skillDelIdx >= len(m.skillDelNames) {
		m.showSkillDelete = false
		return
	}
	name := m.skillDelNames[m.skillDelIdx]
	if err := skill.Delete(name); err != nil {
		m.appendChat("System", "删除失败:"+err.Error())
	} else {
		m.appendChat("System", fmt.Sprintf("已删除 skill「%s」。", name))
	}
	m.showSkillDelete = false
	m.input.Focus()
}

// === modal 渲染 ===

func (m model) skillAddModalBlock() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(highlightColor).Render("安装 Skill")

	switch {
	case m.skillAddInstalling:
		body := lipgloss.NewStyle().Foreground(subtleColor).Render("正在下载并安装…(最多 90s)")
		return wrapModal(title+"\n\n"+body, 64, m.width)

	case m.skillAddSearching:
		body := lipgloss.NewStyle().Foreground(subtleColor).Render("跨源搜索中…(最多 15s)")
		return wrapModal(title+"\n\n"+body, 64, m.width)

	case len(m.skillAddResults) > 0:
		hint := lipgloss.NewStyle().Foreground(subtleColor).Render(
			fmt.Sprintf("搜到 %d 个结果(↑↓ 选 · Enter 安装 · Esc 关)", len(m.skillAddResults)))
		rows := make([]string, 0, len(m.skillAddResults))
		// 限到前 12 行避免超屏
		maxRows := 12
		end := len(m.skillAddResults)
		if end > maxRows {
			end = maxRows
		}
		for i := 0; i < end; i++ {
			r := m.skillAddResults[i]
			marker := "  "
			style := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
			if i == m.skillAddIdx {
				marker = "▸ "
				style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9")).Background(lipgloss.Color("236"))
			}
			meta := r.SourceID
			if r.Downloads > 0 || r.Stars > 0 {
				meta = fmt.Sprintf("%s · ⭐%d · 📥%d", meta, r.Stars, r.Downloads)
			}
			line := fmt.Sprintf("%s%s  [%s]", marker, r.Name, meta)
			rows = append(rows, style.Render(line))
			if r.Description != "" {
				desc := r.Description
				if len(desc) > 70 {
					desc = desc[:67] + "…"
				}
				rows = append(rows, lipgloss.NewStyle().Foreground(dimColor).Render("      "+desc))
			}
		}
		if len(m.skillAddResults) > maxRows {
			rows = append(rows, lipgloss.NewStyle().Foreground(dimColor).Render(
				fmt.Sprintf("  … 还有 %d 条未显示,请优化查询关键词", len(m.skillAddResults)-maxRows)))
		}
		parts := append([]string{title, "", hint, ""}, rows...)
		return wrapModal(lipgloss.JoinVertical(lipgloss.Left, parts...), 80, m.width)

	default:
		hint := lipgloss.NewStyle().Foreground(subtleColor).Render(
			"输入关键词搜索 Clawhub,或直接粘贴:\n" +
				"  - https://github.com/owner/repo (单 skill 仓库,根含 SKILL.md)\n" +
				"  - https://github.com/anthropics/skills/tree/main/skills/docx (子目录)\n" +
				"  - ~/path/to/skill 或 /abs/path 或 ./relative")
		inputBlock := lipgloss.NewStyle().Foreground(dimColor).Render("Query / URL / Path:") + "\n  " + m.skillAddInput.View()
		parts := []string{title, "", hint, "", inputBlock}
		if m.skillAddErr != "" {
			parts = append(parts, "", lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("✗ "+m.skillAddErr))
		}
		parts = append(parts, "", lipgloss.NewStyle().Foreground(dimColor).Render("Enter 搜索 / 安装 · Esc 取消"))
		return wrapModal(lipgloss.JoinVertical(lipgloss.Left, parts...), 64, m.width)
	}
}

func (m model) skillDeleteModalBlock() string {
	title := lipgloss.NewStyle().Bold(true).Foreground(highlightColor).Render("删除 Skill")
	subtitle := lipgloss.NewStyle().Foreground(subtleColor).Render("仅列 ~/.deepx/skills/ 下的(deepx 自己装的)")
	rows := make([]string, 0, len(m.skillDelNames))
	for i, name := range m.skillDelNames {
		marker := "  "
		style := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
		if i == m.skillDelIdx {
			marker = "▸ "
			style = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9")).Background(lipgloss.Color("236"))
		}
		rows = append(rows, style.Render(marker+name))
	}
	footer := lipgloss.NewStyle().Foreground(dimColor).Render("↑/↓ 选择 · Enter 删除 · Esc 取消")
	parts := append([]string{title, subtitle, ""}, rows...)
	parts = append(parts, "", footer)
	return wrapModal(lipgloss.JoinVertical(lipgloss.Left, parts...), 50, m.width)
}

// wrapModal 把内容包成 modal 边框,宽度 prefer/preferred,窗口太窄时退化到 width-4。
func wrapModal(content string, preferred, screenWidth int) string {
	w := preferred
	if maxW := screenWidth - 4; w > maxW {
		w = maxW
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(highlightColor).Padding(1, 2).Width(w).Render(content)
}
