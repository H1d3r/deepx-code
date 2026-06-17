package agent

import (
	"context"
	"deepx/workflow"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// StartWorkflow 跑一个 workflow 脚本,返回 (cmd, ch);消费方式与 StartStream 完全一致
// (TUI 用 ListenToStream 逐条取),因此命令入口几乎零特判。
//
// 映射关系:
//   - 脚本里的 agent(prompt, opts) → 一次 runSubAgent(子 agent 隐藏中间过程,只回 summary);
//   - phase()/log() 与每个 agent 的起止 → 以 TokenMsg 流式打到助手回复里;
//   - 脚本 main(args) 的返回值 → 作为本回合助手最终输出;
//   - StreamDoneMsg 收尾(TUI 据此落盘 + 解锁输入)。
//
// schema 通过「把 schema 注册成 structured_output 工具,结果取自工具调用」强制实现(见 runSubAgent);
// parallel() 是真并发(见 workflow 引擎 __agentBatch)。
func StartWorkflow(
	ctx context.Context,
	models ModelConfig,
	history []ChatMessage,
	mode AgentMode,
	workspace string,
	skillCatalog string,
	script *workflow.Script,
	args any,
) (tea.Cmd, <-chan tea.Msg) {
	ch := make(chan tea.Msg, 128)

	go func() {
		defer close(ch)
		// emitText 可能被并发 worker(parallel)与 JS 线程同时调用 → 加锁守 transcript。
		// emitMsg 直接发结构化消息(PlanCreatedMsg 驱动步骤 overlay),channel 发送本身并发安全。
		var mu sync.Mutex
		var transcript strings.Builder
		emitText := func(s string) {
			mu.Lock()
			transcript.WriteString(s)
			mu.Unlock()
			ch <- TokenMsg(s)
		}
		emitMsg := func(m tea.Msg) { ch <- m }

		result, err := runWorkflowWithProgress(ctx, models, mode, workspace, skillCatalog, script, args, emitText, emitMsg)
		if err != nil {
			// 失败/中断:保留全部 journal(父+子),下次重跑可 resume。
			ch <- StreamErrMsg{Err: fmt.Errorf("workflow %s: %w", script.Name, err)}
			return
		}
		if s := strings.TrimSpace(result); s != "" {
			emitText("\n\n" + s + "\n")
		}
		emitText(fmt.Sprintf("\n\n**✓ workflow %s 完成**\n", script.Name))

		// 把整段输出作为助手回复并入 history,后续对话轮能引用 workflow 结果。
		newHistory := append(append([]ChatMessage(nil), history...),
			ChatMessage{Role: "assistant", Content: transcript.String()})
		ch <- HistoryUpdateMsg{History: newHistory}
		ch <- StreamDoneMsg{}
	}()

	return ListenToStream(ch), ch
}

// runWorkflowEmitting 跑一个 workflow,返回 main 的最终结果。命令路径和 Workflow 工具路径共用。
//
// 两路输出:
//   - emitText:文本进度(intro / phase 标题 / log / 结果),进助手回复 + transcript;
//   - emitMsg:结构化消息——每个 agent 当一个 plan 节点,用 PlanCreatedMsg 驱动现成的步骤
//     overlay(复选框状态 + 模型标签 [pro]/[flash]),实现「每步实时状态 + 用的模型」的展示。
//
// 进度文本格式要点(实测 deepx glamour):单换行会折叠、两空格硬换行无效、`##` 会留标记;
// 所以阶段用 **bold** 独立段、列表用 `- …`。
// wfProgress 是嵌套各级共享的运行态,解决两件事:
//  1. 步骤 overlay:把父 + 所有子 workflow 的 agent 步骤收进同一份 plan,按 phase 分组实时显示
//     (子步骤归到「子名 / 阶段」组),避免子级单发 PlanCreatedMsg 覆盖父 overlay。
//  2. resume:登记本次运行涉及的全部 journal(父 + 各子),整体成功时由顶层一并清理;
//     中途失败/中断则全部保留,下次重跑父 + 已完成的子都能命中缓存。
//
// 方法自带锁:emitMsg 经 channel 发送本身安全,但 plan/计数会被并发 worker(parallel)与 JS 线程触碰。
type wfProgress struct {
	mu          sync.Mutex
	emitMsg     func(tea.Msg)
	plan        []PlanItem     // per-agent(详细视图 / 小工作流 / 最终快照)
	idIndex     map[string]int // id → plan 下标;finish 时 O(1) 定位(plan 只 append 不重排,下标稳定)
	idSeq       int
	aggregate   bool // sticky:agent 数超阈值切「按 phase 聚合」,不回退(避免抖动)
	phaseOrder  []*wfPhaseAgg
	phaseByName map[string]*wfPhaseAgg
	journals    []*workflowJournal // 父 + 各子的 resume journal,顶层成功时统一 Discard
	childSeq    int                // RunChild 调用计数,给子 journal 加盐区分同名+同 args 的多次嵌套调用
	declared    []string           // 声明的全部阶段名(meta.phases);随每次 push 发给 UI 作上展示骨架
	seedKey     map[string]string  // "phase\x00label" → 预置占位项 id;实际 agent 跑起来时据此命中并点亮
}

func newWfProgress(emitMsg func(tea.Msg)) *wfProgress {
	return &wfProgress{emitMsg: emitMsg, idIndex: map[string]int{}, phaseByName: map[string]*wfPhaseAgg{}, seedKey: map[string]string{}}
}

func seedKeyOf(phase, label string) string { return phase + "\x00" + label }

// seedSkeleton 在运行前把「声明的阶段 + 静态解析出的预期步骤」预置成清单(步骤为 pending 占位项),
// 立刻 push 一次——UI 因此在任何 agent 开跑前就显示完整骨架(阶段 + 其下的 agent),逐步点亮。
// 实际 agent 跑起来时 start() 按 (phase,label) 命中占位项、原地点亮,不重复添加。只在顶层(depth 0)调一次。
func (p *wfProgress) seedSkeleton(phases []string, steps []workflow.ExpectedStep) {
	p.mu.Lock()
	p.declared = append([]string(nil), phases...)
	for _, s := range steps {
		key := seedKeyOf(s.Phase, s.Label)
		if _, dup := p.seedKey[key]; dup || s.Label == "" {
			continue // 同阶段同 label 只占一个位;无 label 的不预置(运行时再出现)
		}
		id := fmt.Sprintf("wf-%d", p.idSeq)
		p.idSeq++
		p.plan = append(p.plan, PlanItem{ID: id, Title: s.Label, Model: s.Model, Status: PlanStatusPending, Phase: s.Phase})
		p.idIndex[id] = len(p.plan) - 1
		p.seedKey[key] = id
		ph := p.getPhase(s.Phase)
		ph.total++ // 预期总数(start 命中占位项时不再 ++,避免重复计数)
	}
	if len(p.plan) > workflowDetailThreshold {
		p.aggregate = true
	}
	hasSkeleton := len(phases) > 0 || len(p.plan) > 0
	p.mu.Unlock()
	if hasSkeleton {
		p.mu.Lock()
		p.push()
		p.mu.Unlock()
	}
}

// nextChildSalt 给下一次嵌套调用分配一个稳定的盐(JS 单线程、确定性执行 → 跨次运行序号一致,resume 可用)。
func (p *wfProgress) nextChildSalt() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.childSeq
	p.childSeq++
	return fmt.Sprintf("c%d", s)
}

// getPhase / push 必须持锁调用。
func (p *wfProgress) getPhase(name string) *wfPhaseAgg {
	a := p.phaseByName[name]
	if a == nil {
		a = &wfPhaseAgg{name: name}
		p.phaseByName[name] = a
		p.phaseOrder = append(p.phaseOrder, a)
	}
	return a
}

func (p *wfProgress) push() {
	p.emitMsg(PlanCreatedMsg{Plans: buildProgressItems(p.aggregate, p.plan, p.phaseOrder), Kind: "createplan", Phases: p.declared})
}

// start 登记一个开始运行的 agent 步骤,返回其节点 ID。
// 若命中预置占位项(seedSkeleton 解析出的预期步骤)→ 原地点亮(不新增、不重复计数);否则新增。
func (p *wfProgress) start(phase, label, model string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// 命中预置占位项 → 原地从 pending 变 running。
	if id, ok := p.seedKey[seedKeyOf(phase, label)]; ok {
		delete(p.seedKey, seedKeyOf(phase, label)) // 用过即移除,避免同 label 二次命中
		if i, ok2 := p.idIndex[id]; ok2 && p.plan[i].Status == PlanStatusPending {
			p.plan[i].Status = PlanStatusRunning
			p.plan[i].StartedAt = now
			if model != "" {
				p.plan[i].Model = model
			}
			if ph := p.phaseByName[phase]; ph != nil {
				ph.running++ // total 已在 seed 时计入,这里只加 running
			}
			p.push()
			return id
		}
	}
	// 新步骤(动态产生 / 未预置)。
	id := fmt.Sprintf("wf-%d", p.idSeq)
	p.idSeq++
	p.plan = append(p.plan, PlanItem{ID: id, Title: label, Model: model, Status: PlanStatusRunning, Phase: phase, StartedAt: now})
	p.idIndex[id] = len(p.plan) - 1
	ph := p.getPhase(phase)
	ph.total++
	ph.running++
	if len(p.plan) > workflowDetailThreshold {
		p.aggregate = true
	}
	p.push()
	return id
}

// finish 标记一个 agent 步骤完成/失败。
func (p *wfProgress) finish(id string, failed bool, summary string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	phase := ""
	if i, ok := p.idIndex[id]; ok {
		phase = p.plan[i].Phase
		if failed {
			p.plan[i].Status = PlanStatusFailed
			p.plan[i].Summary = summary
		} else {
			p.plan[i].Status = PlanStatusDone
		}
		if !p.plan[i].StartedAt.IsZero() {
			p.plan[i].Elapsed = time.Since(p.plan[i].StartedAt) // 子 agent 耗时(Go 侧计时)
		}
	}
	if ph := p.phaseByName[phase]; ph != nil {
		ph.running--
		if failed {
			ph.failed++
		} else {
			ph.done++
		}
	}
	p.push()
}

func (p *wfProgress) addJournal(j *workflowJournal) {
	p.mu.Lock()
	p.journals = append(p.journals, j)
	p.mu.Unlock()
}

// discardAll 删除本次运行(父 + 所有子)的全部 journal:整体成功后调用,使下次是全新运行。
func (p *wfProgress) discardAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, j := range p.journals {
		j.Discard()
	}
}

// snapshotText 把最终步骤渲染成固化文本:大工作流按 phase 聚合,小工作流逐项(均含父子全量)。
func (p *wfProgress) snapshotText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.aggregate && len(p.phaseOrder) > 0:
		return renderPhaseSnapshot(p.phaseOrder)
	case !p.aggregate && len(p.plan) > 0:
		return renderPlanSnapshot(p.plan)
	}
	return ""
}

// runWorkflowWithProgress 跑一个顶层 workflow:建进度态、加载 resume journal、跑、整体成功则清 journal。
// 命令路径(StartWorkflow)和工具路径(handleWorkflowTool run)共用,消除两处同构 setup。
// 返回 main 的最终结果与错误;失败时保留全部 journal(父+子)以便下次 resume。
func runWorkflowWithProgress(
	ctx context.Context,
	models ModelConfig,
	mode AgentMode,
	workspace, skillCatalog string,
	script *workflow.Script,
	args any,
	emitText func(string),
	emitMsg func(tea.Msg),
) (string, error) {
	prog := newWfProgress(emitMsg)
	prog.seedSkeleton(script.Phases, script.Steps) // 运行前就把全部阶段 + 其下步骤显示成清单,逐步点亮

	// resume:加载磁盘 journal(同名+同 args+同脚本);命中则跳过已完成步骤。登记进 prog,
	// 嵌套的子 journal 也会登记,整体成功时一并清理。
	journal, cached, persisted := openWorkflowJournal(script, args, "")
	prog.addJournal(journal)
	if !persisted {
		emitText("⚠ resume 缓存目录不可写,本次中断后将无法续跑(其余功能不受影响)\n\n")
	}
	if cached > 0 {
		emitText(fmt.Sprintf("↻ resume:复用 %d 个已完成步骤的缓存,只跑剩余\n\n", cached))
	}

	result, err := runWorkflowEmitting(ctx, models, mode, workspace, skillCatalog, script, args, journal, prog, emitText, 0)
	if err != nil {
		return "", err // 保留全部 journal(父+子),下次重跑可 resume
	}
	prog.discardAll() // 整体成功:清掉父+所有子 journal,下次是全新运行
	return result, nil
}

func runWorkflowEmitting(
	ctx context.Context,
	models ModelConfig,
	mode AgentMode,
	workspace, skillCatalog string,
	script *workflow.Script,
	args any,
	journal *workflowJournal, // 本级 resume 缓存(已登记进 prog,顶层成功统一清理)
	prog *wfProgress, // 嵌套各级共享:步骤进度 + journal 登记
	emitText func(string),
	depth int, // 嵌套深度;>=1 时禁止再嵌套(限一层)
) (string, error) {
	emitText(fmt.Sprintf("**▶ workflow %s 开始**\n\n", script.Name))

	exec := workflow.ExecutorFunc(func(actx context.Context, call workflow.AgentCall) (string, error) {
		label := call.Label
		if label == "" {
			label = snippetText(call.Prompt, 48)
		}
		modelTag := "flash" // 节点模型标签:脚本指定 pro 才显示 pro,否则默认 flash
		if strings.EqualFold(strings.TrimSpace(call.Model), "pro") {
			modelTag = "pro"
		}
		phaseName := firstNonEmpty(call.Phase, "agent")
		if depth > 0 {
			phaseName = script.Name + " / " + phaseName // 子 workflow 步骤归到「子名 / 阶段」组,与父步骤区分
		}

		id := prog.start(phaseName, label, modelTag)

		// schema 不再拼进 prompt:它作为 structured_output 工具的参数下发(见 runSubAgent),
		// 模型通过调用该工具交结果 → 天生合法 JSON。注意用 HasSchema():没声明 schema 的步骤
		// (如最终汇总)不能误判成有 schema,否则会被塞个 structured_output 工具、把 markdown 裹成 JSON。
		var schema json.RawMessage
		if call.HasSchema() {
			schema = call.Schema
		}
		res := runSubAgent(actx, subAgentInput{
			Models:       models,
			Entry:        resolveModelEntry(call.Model, models),
			NodeID:       id,
			NodeTitle:    call.Prompt,
			UserTask:     "workflow: " + script.Name,
			Workspace:    workspace,
			SkillCatalog: skillCatalog,
			Mode:         mode,
			FullOutput:   true, // workflow 的 agent 要完整结果(报告/JSON/列表),不限 200 字
			WantJSON:     call.HasSchema(),
			Schema:       schema, // 非空 → 注册 structured_output 工具,结果取自工具调用
		})

		errStr := ""
		if res.Err != nil {
			errStr = res.Err.Error()
		}
		prog.finish(id, res.Err != nil, errStr)

		if res.Err != nil {
			return "", res.Err
		}
		out := res.Summary
		if call.HasSchema() {
			out = extractJSONBlock(out)
		}
		return out, nil
	})

	result, err := workflow.Run(ctx, script, workflow.RunOptions{
		Args:     args,
		Executor: exec,
		Results:  journal,
		// 不再把 phase 打成单独文本行 —— 阶段改由步骤 overlay / 最终快照按 Phase 分组体现。
		OnLog: func(m string) { emitText(fmt.Sprintf("- %s\n", m)) },
		// 嵌套:workflow(name,args) → 加载并跑子 workflow(限一层)。子步骤进同一 prog(与父混排显示);
		// 子级自带 journal(按子名+args 单独键),登记进 prog → 嵌套也参与 resume,顶层成功时统一清理。
		RunChild: func(cctx context.Context, name string, cargs any) (string, error) {
			if depth >= 1 {
				return "", fmt.Errorf("workflow 嵌套仅限一层,子 workflow 里不能再调 workflow()")
			}
			child, err := workflowLoaderFor(workspace).Load(name)
			if err != nil {
				return "", err
			}
			cj, _, _ := openWorkflowJournal(child, cargs, prog.nextChildSalt())
			prog.addJournal(cj)
			return runWorkflowEmitting(cctx, models, mode, workspace, skillCatalog, child, cargs, cj, prog, emitText, depth+1)
		},
	})

	// 子 workflow 完成打个边界标记(顶层的「✓ 完成」由调用方发,避免重复)。
	if depth > 0 && err == nil {
		emitText(fmt.Sprintf("\n**↳ 子 workflow %s 完成**\n\n", script.Name))
	}

	// 最终步骤快照只在顶层固化一次(父子混排的全量);子级不重复发(live overlay 完成即隐藏)。
	if depth == 0 {
		if snap := prog.snapshotText(); snap != "" {
			emitText("\n\n**■ 步骤**\n\n" + snap)
		}
	}

	return result, err
}

// workflowDetailThreshold:agent 数超过它就从「每-agent 详细视图」切到「按 phase 聚合」。
const workflowDetailThreshold = 16

// wfPhaseAgg 是一个 phase 的实时计数(用于聚合视图)。
type wfPhaseAgg struct {
	name                         string
	total, running, done, failed int
}

// buildProgressItems 按当前规模产出进度节点:小工作流逐 agent(带模型),大工作流每 phase 一行。
func buildProgressItems(aggregate bool, plan []PlanItem, phases []*wfPhaseAgg) []PlanItem {
	if !aggregate {
		return append([]PlanItem(nil), plan...)
	}
	items := make([]PlanItem, 0, len(phases))
	for _, a := range phases {
		items = append(items, PlanItem{ID: "phase-" + a.name, Title: wfPhaseTitle(a), Status: wfPhaseStatus(a)})
	}
	return items
}

func wfPhaseTitle(a *wfPhaseAgg) string {
	t := fmt.Sprintf("%s  %d/%d", a.name, a.done+a.failed, a.total)
	if a.failed > 0 {
		t += fmt.Sprintf("  ✗%d", a.failed)
	}
	return t
}

func wfPhaseStatus(a *wfPhaseAgg) PlanStatus {
	switch {
	case a.running > 0:
		return PlanStatusRunning
	case a.total > 0 && a.done+a.failed >= a.total:
		if a.failed > 0 {
			return PlanStatusFailed
		}
		return PlanStatusDone
	default:
		return PlanStatusPending
	}
}

// renderPhaseSnapshot 把最终 phase 聚合渲染成 markdown 列表,固化进历史(大工作流用)。
func renderPhaseSnapshot(phases []*wfPhaseAgg) string {
	var sb strings.Builder
	for _, a := range phases {
		sym := "✓"
		switch {
		case a.failed > 0:
			sym = "✗"
		case a.done < a.total:
			sym = "•"
		}
		fmt.Fprintf(&sb, "- %s %s  %d/%d", sym, a.name, a.done+a.failed, a.total)
		if a.failed > 0 {
			fmt.Fprintf(&sb, " (✗%d)", a.failed)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// renderPlanSnapshot 把最终 plan 渲染成 markdown,固化进对话历史:按 Phase 分组(粗体阶段名 +
// 该阶段的步骤列表)。无 Phase(CreatePlan)则不分组、平铺。用纯符号而非 `[x]`,避免被 glamour
// 当 GFM 任务列表特殊处理。
func renderPlanSnapshot(plan []PlanItem) string {
	// 按首次出现顺序收集阶段 + 分桶
	var order []string
	seen := map[string]bool{}
	buckets := map[string][]PlanItem{}
	for _, p := range plan {
		if !seen[p.Phase] {
			seen[p.Phase] = true
			order = append(order, p.Phase)
		}
		buckets[p.Phase] = append(buckets[p.Phase], p)
	}

	var sb strings.Builder
	for _, ph := range order {
		if ph != "" {
			fmt.Fprintf(&sb, "\n**%s**\n\n", ph) // 阶段小标题(粗体,glamour 去标记)
		}
		for _, p := range buckets[ph] {
			sym := "•"
			switch p.Status {
			case PlanStatusDone:
				sym = "✓"
			case PlanStatusFailed:
				sym = "✗"
			case PlanStatusBlocked:
				sym = "⏸"
			case PlanStatusRunning:
				sym = "⏵"
			}
			sb.WriteString("- " + sym + " " + p.Title)
			if p.Model != "" {
				sb.WriteString(" · " + p.Model)
			}
			if d := FmtDur(p.Elapsed); d != "" {
				sb.WriteString(" · " + d)
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// FmtDur 把子 agent 耗时格式化成简短串(<1s→ms,<1m→s,否则 m+s);<=0 返回空(不显示)。
// 导出供 TUI 渲染步骤耗时复用,避免两处各写一份格式化。
func FmtDur(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// snippetText 把一段文本压成单行预览(超出截断加省略号)。
func snippetText(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// extractJSONBlock 从子 agent 文本里抠出 JSON:剥 ```json 围栏,再取首个 { / [ 到末个 } / ]。
// 失败就原样返回(交给 JS 侧 JSON.parse 报错 → agent() reject)。
func extractJSONBlock(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	// 优先:从第一个 {/[ 起按括号深度切出第一个**配平**的 JSON 值(跳过字符串字面量与转义)。
	// 这样「解释文字 {真正的 JSON} 后面又有含 } 的文字」也能正确取中间那段,而非贪婪跨块误取。
	if v := firstBalancedJSON(s); v != "" && json.Valid([]byte(v)) {
		return v
	}
	// 兜底:首个 {/[ 到末个 }/](老行为),交给 JS 侧 JSON.parse 决定成败。
	start := strings.IndexAny(s, "{[")
	end := strings.LastIndexAny(s, "}]")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

// firstBalancedJSON 返回 s 中第一个括号配平的 JSON 值(对象/数组)子串;找不到或被截断不配平则返回 ""。
// 正确处理字符串字面量内的括号与转义(里面的 {}[]" 不参与计数)。只匹配 ASCII 控制字符,多字节 UTF-8 安全。
func firstBalancedJSON(s string) string {
	start := strings.IndexAny(s, "{[")
	if start < 0 {
		return ""
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}
