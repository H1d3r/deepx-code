package tui

import (
	"deepx/agent"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// planState 持有当前活跃的规划及其状态。
// 由 agent.PlanCreatedMsg 初始化,agent.TaskStatusMsg 增量更新。
// nil 表示当前无规划。
type planState struct {
	items  []agent.PlanItem
	phases []string // workflow 声明的全部阶段(meta.phases);非空时按它渲染全部阶段(含尚未开始的)
}

// apply 处理 TaskStatusMsg,把 plan 状态写到对应项。
// 找不到 id 时静默忽略(LLM 偶尔会把 id 拼错)。
func (p *planState) apply(msg agent.TaskStatusMsg) {
	if p == nil {
		return
	}
	for i := range p.items {
		if p.items[i].ID == msg.ID {
			p.items[i].Status = msg.Status
			if msg.Summary != "" {
				p.items[i].Summary = msg.Summary
			}
			return
		}
	}
}

// allFinished 报告所有 plan 节点是否都已经进入终态(done/failed/blocked)。
// 全部跑完后 UI 隐藏 plan overlay,把屏幕让给模型后续的总结/继续输出,
// 避免 checkbox 列表和流式 token 混在一起。
func (p *planState) allFinished() bool {
	if p == nil {
		return false
	}
	// workflow:只要还有声明的阶段一个步骤都没跑过,就不算完成(否则阶段之间的空档会误判完成、
	// 提前隐藏 overlay)。各步骤动态产生,以"该阶段是否出现过 item"判断它是否已开跑。
	if len(p.phases) > 0 {
		started := map[string]bool{}
		for _, it := range p.items {
			started[it.Phase] = true
		}
		for _, ph := range p.phases {
			if !started[ph] {
				return false
			}
		}
	}
	if len(p.items) == 0 {
		return false
	}
	for _, it := range p.items {
		switch it.Status {
		case agent.PlanStatusDone, agent.PlanStatusFailed, agent.PlanStatusBlocked:
			continue
		default:
			return false
		}
	}
	return true
}

// planStatusBox plan 状态用复选框风格渲染,固定 3 ANSI cell 宽。
//   - pending: [ ] 待执行
//   - running: [⏵] 跑中(着色)
//   - done:    [✓] 完成(绿色)
//   - failed:  [✗] 失败(红色)
//   - blocked: [⏸] 跳过(暗色)
func planStatusBox(s agent.PlanStatus) string {
	switch s {
	case agent.PlanStatusRunning:
		return lipgloss.NewStyle().Foreground(highlightColor).Render("[⏵]")
	case agent.PlanStatusDone:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("[✓]")
	case agent.PlanStatusFailed:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("[✗]")
	case agent.PlanStatusBlocked:
		return lipgloss.NewStyle().Foreground(dimColor).Render("[⏸]")
	case agent.PlanStatusPending:
		return lipgloss.NewStyle().Foreground(dimColor).Render("[ ]")
	}
	return "[ ]"
}

// renderPlanSummary 右栏极简摘要:始终显示完成进度 "X/Y";无规划时为 "0/0"。
func renderPlanSummary(p *planState, _ int) []string {
	total, done := 0, 0
	if p != nil {
		for _, pl := range p.items {
			total++
			if pl.Status == agent.PlanStatusDone {
				done++
			}
		}
	}
	return []string{fmt.Sprintf("%d/%d", done, total)}
}

// planModelTag 渲染一个 plan 节点的 model 标签,显示在 title 后。
//   - "pro"   → 高亮色,提醒用户这一步用了贵模型
//   - "flash" → 暗色,弱化展示,信息完整但不抢眼
//   - 空 / 其他 → 不渲染(老数据 / 模型瞎填,降级到无 tag)
func planModelTag(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "pro":
		return lipgloss.NewStyle().Foreground(accentColor).Render("[pro]")
	case "flash":
		return lipgloss.NewStyle().Foreground(dimColor).Render("[flash]")
	}
	return ""
}

// renderPlanForChat 把 plan 列表渲染成 chat 区使用的字符串(多行)。
// 每次都用当前 planState 的实际状态(checkbox 反映 done / running / pending),
// refreshViewport 每次 tick / token / TaskStatusMsg 都重新渲染一遍,实现 live overlay。
// 流结束时再固化一次到 chatContent,这样滚回历史也能看到最终结果。
func renderPlanForChat(p *planState) string {
	if p == nil || (len(p.items) == 0 && len(p.phases) == 0) {
		return ""
	}
	var sb strings.Builder
	dim := lipgloss.NewStyle().Foreground(dimColor).Render
	phaseStyle := lipgloss.NewStyle().Foreground(highlightColor).Bold(true)

	// 按「阶段」(PlanItem.Phase)分组:workflow 步骤带阶段名,渲染成「阶段小标题 + 缩进的步骤」;
	// CreatePlan / Todo 无 Phase(空串)→ 单组、不出标题、平铺(保持原行为)。
	var itemOrder []string
	seen := map[string]bool{}
	buckets := map[string][]agent.PlanItem{}
	for _, pl := range p.items {
		if !seen[pl.Phase] {
			seen[pl.Phase] = true
			itemOrder = append(itemOrder, pl.Phase)
		}
		buckets[pl.Phase] = append(buckets[pl.Phase], pl)
	}

	// 阶段顺序:workflow 有声明(p.phases)→ 先按声明顺序列出**全部**阶段(含尚未开始、无步骤的),
	// 再补 items 里出现但未声明的(如嵌套子 workflow 的「子名/阶段」);无声明 → 按 item 首次出现顺序。
	var order []string
	if len(p.phases) > 0 {
		order = append(order, p.phases...)
		declared := map[string]bool{}
		for _, ph := range p.phases {
			declared[ph] = true
		}
		for _, ph := range itemOrder {
			if ph != "" && !declared[ph] {
				order = append(order, ph)
			}
		}
	} else {
		order = itemOrder
	}

	for _, ph := range order {
		grouped := ph != ""
		if grouped {
			// 阶段小标题前带状态框:未开始=待执行、跑中=⏵、完成=✓——实现「全部阶段先列出、逐步点亮」;
			// 标题后带阶段墙钟耗时(跑中实时、完成定格)。
			header := "  " + planStatusBox(phaseStatusFromItems(buckets[ph])) + " " + phaseStyle.Render(ph)
			if d := durTag(phaseElapsed(buckets[ph])); d != "" {
				header += " " + d
			}
			sb.WriteString(header + "\n")
		}
		for _, pl := range buckets[ph] {
			if grouped {
				sb.WriteString("    ") // 有阶段标题时步骤再缩进一层
			} else {
				sb.WriteString("  ")
			}
			sb.WriteString(planStatusBox(pl.Status))
			sb.WriteString(" ")
			sb.WriteString(pl.Title)
			if tag := planModelTag(pl.Model); tag != "" {
				sb.WriteString(" ")
				sb.WriteString(tag)
			}
			if d := planDurTag(pl); d != "" {
				sb.WriteString(" ")
				sb.WriteString(d)
			}
			if len(pl.DependsOn) > 0 && pl.Status == agent.PlanStatusPending {
				sb.WriteString(dim("  (deps: " + strings.Join(pl.DependsOn, ",") + ")"))
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// itemElapsed 返回一个步骤的耗时:完成→定格 Elapsed;跑中→实时 now-StartedAt(随 600ms tick 跳秒);
// 其余(pending)→0。
func itemElapsed(pl agent.PlanItem) time.Duration {
	if pl.Elapsed > 0 {
		return pl.Elapsed
	}
	if pl.Status == agent.PlanStatusRunning && !pl.StartedAt.IsZero() {
		return time.Since(pl.StartedAt)
	}
	return 0
}

// phaseElapsed 返回一个阶段的墙钟耗时:从该阶段最早步骤开跑算起。跑中→到现在(实时);
// 全部完成→到最后一个步骤结束(并行步骤取整体跨度,不是各步相加)。无已开跑步骤→0。
func phaseElapsed(items []agent.PlanItem) time.Duration {
	var earliest, latestEnd time.Time
	running := false
	for _, it := range items {
		if it.StartedAt.IsZero() {
			continue
		}
		if earliest.IsZero() || it.StartedAt.Before(earliest) {
			earliest = it.StartedAt
		}
		if it.Status == agent.PlanStatusRunning {
			running = true
		}
		if it.Elapsed > 0 {
			if end := it.StartedAt.Add(it.Elapsed); end.After(latestEnd) {
				latestEnd = end
			}
		}
	}
	switch {
	case earliest.IsZero():
		return 0
	case running:
		return time.Since(earliest)
	case latestEnd.IsZero():
		return 0
	default:
		return latestEnd.Sub(earliest)
	}
}

// planDurTag 渲染一个 workflow 步骤的耗时标签(暗色,显示在 model 标签后)。跑中实时、完成定格;pending 不显示。
// 格式化复用 agent.FmtDur,与固化快照口径一致。
func planDurTag(pl agent.PlanItem) string {
	s := agent.FmtDur(itemElapsed(pl))
	if s == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(dimColor).Render(s)
}

// durTag 把一段耗时渲染成暗色标签(空耗时→空串)。
func durTag(d time.Duration) string {
	s := agent.FmtDur(d)
	if s == "" {
		return ""
	}
	return lipgloss.NewStyle().Foreground(dimColor).Render(s)
}

// phaseStatusFromItems 由一个阶段下已出现的步骤推导该阶段的展示状态(用于阶段小标题的状态框)。
// 注:步骤动态产生、未开始的步骤还不存在,所以「无步骤」= 该阶段尚未开始 = 待执行。
func phaseStatusFromItems(items []agent.PlanItem) agent.PlanStatus {
	if len(items) == 0 {
		return agent.PlanStatusPending
	}
	running, failed, done := 0, 0, 0
	for _, it := range items {
		switch it.Status {
		case agent.PlanStatusRunning:
			running++
		case agent.PlanStatusFailed:
			failed++
		case agent.PlanStatusDone:
			done++
		}
	}
	switch {
	case running > 0:
		return agent.PlanStatusRunning
	case failed > 0:
		return agent.PlanStatusFailed
	case done > 0:
		return agent.PlanStatusDone
	default:
		return agent.PlanStatusPending
	}
}

// truncate 用 … 截断超长字符串。按 rune 计宽 (中文每字 ~2 cell 暂不精确折算,差几格能接受)。
func truncate(s string, max int) string {
	if max <= 1 {
		return "…"
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
