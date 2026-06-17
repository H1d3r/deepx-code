package agent

import (
	"context"
	"deepx/tools"
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// streamOnceFn 是对 streamOnce 的可替换引用(seam),让测试能注入假模型响应,
// 确定性地验证「续写 / 退化重试」等子 agent 控制流,而不必真打网络。
var streamOnceFn = streamOnce

// buildSubAgentToolSpecs 子 agent 工具集。已不再按角色过滤工具,所以跟主 agent 的工具表
// 逐字节一致 —— 刻意如此,保前缀缓存(别为了"藏工具"去按角色裁,裁了工具表分叉就 cache miss)。
//
// CreatePlan / Todo / SwitchModel 子 agent 不该用,靠两层兜底(没有 subAgentToolDenylist 那种硬过滤,别去找):
//  1. runSubAgent 尾部系统提示词明确禁止 CreatePlan / Todo / SwitchModel;
//  2. 它们 Executor 为 nil,子 agent 走 executeTool 时纵深防护返回失败(不 panic、不生效)。
func buildSubAgentToolSpecs(mode AgentMode) []tools.OpenAIToolSpec {
	return buildToolSpecs(mode)
}

// subAgentInput 是一次子 agent 调用的全部依赖。
// 由 runDAG 的 exec 回调按节点上下文构造,主 agent 不直接调用。
type subAgentInput struct {
	Models       ModelConfig // 整套配置,留作扩展用(目前不直接消费)
	Entry        ModelEntry  // 本节点选定的连接参数 (BaseURL/Model/APIKey)
	NodeID       string
	NodeTitle    string
	UserTask     string            // 用户原始消息,作为背景给子 agent
	Predecessors map[string]string // 已完成上游节点的 summary
	Workspace    string
	SkillCatalog string // 与主 agent 同一份 skill 目录,使子 agent 也能用 LoadSkill
	Mode         AgentMode

	// FullOutput=true 时不限制「<200 字简短总结」,让子 agent 输出完整结果
	// (审查报告 / JSON / 列表等)。workflow 的 agent() 调用用它;CreatePlan 节点保持简短。
	FullOutput bool

	// WantJSON=true 表示这步声明了 schema、产出必须是合法 JSON。
	WantJSON bool

	// Schema 非空 → 注册一个 structured_output 工具(参数即此 schema),让子 agent 通过【调用工具】
	// 提交结构化结果(结果取自工具调用的 arguments,天生合法 JSON),而不是把 JSON 写在正文里再解析。
	// 这是对齐 Claude Code 的根本做法;WantJSON 的"校验正文+重试"退为模型不调工具时的兜底。
	Schema json.RawMessage
}

// subAgentResult 子 agent 完成后的产物。
type subAgentResult struct {
	Summary string
	Err     error
}

// maxSubAgentContinueRounds 是因 max_tokens 截断(finish_reason=length)而「续写」的最大轮数。
// 长报告(workflow synthesize / FullOutput)常一轮写不完,被截断后让模型接着写;有此上限 + ctxBudget
// 兜底,避免无限续。
const maxSubAgentContinueRounds = 8

// maxBadOutputRetries 是子 agent「输出不合格」时的最大重试次数。两类不合格:
//  1. FullOutput 退化:空 / 只有 {}/[]/null;
//  2. WantJSON(声明了 schema)却不是合法 JSON(返回了 markdown / 代码围栏 / 解释文字)。
//
// 模型偶发不听话,重试一两次通常就好;有上限避免死循环。
const maxBadOutputRetries = 2

// isDegenerateOutput 判断一段输出是否「退化」:空,或去掉所有空白后只剩空对象/空数组/null 这类
// 无正文的占位(容忍 `{ }`、`{\n}`、`[ ]` 等任意空白变体)。
func isDegenerateOutput(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	switch strings.Join(strings.Fields(t), "") { // 去掉所有空白(空格/换行/制表)再比
	case "{}", "[]", "null":
		return true
	}
	return false
}

// subAgentCtxBudgetPct 是子 agent convo 占模型上下文窗口的上限百分比;超过即中止本节点。
// 子 agent 不压缩,留 20% 余量给本轮输入+输出,避免撑爆窗口导致 API 脏失败。
// 不设固定轮数上限(对齐主 agent / Claude Code):靠这个 ctxBudget 兜底 + 无进展断路器
// (复用 maxNoProgressRounds)拦失败循环,跑到模型自己停为止。
const subAgentCtxBudgetPct = 80

// estimateConvoTokens 粗估一段 convo 的 token 数(沿用项目 ~3 字符/token 的口径)。
// 只算文本主体(Content + ReasoningContent + 工具调用参数),够做预算判断。
func estimateConvoTokens(convo []ChatMessage) int {
	chars := 0
	for _, m := range convo {
		chars += len([]rune(m.Content)) + len([]rune(m.ReasoningContent))
		for _, tc := range m.ToolCalls {
			chars += len([]rune(tc.Function.Arguments))
		}
	}
	return chars / 3
}

// runSubAgent 执行单个 plan/task 节点。
//
// 行为:
//   - 独立 history,只含 system prompt + 用户原始任务 + 节点 title
//   - 工具表与主 agent 一致;不该用的 CreatePlan/Todo/SwitchModel 靠系统提示词禁止 + nil-Executor 兜底(见 buildSubAgentToolSpecs)
//   - UpdatePlanStatus 调用被吞掉,scheduler 才是状态真实来源
//   - 不向 TUI 发 TokenMsg / ToolCallStartMsg 等可见事件,子 agent 中间过程完全隐藏
//   - 最终 assistant content 作为 Summary 返回;失败 → Err
func runSubAgent(ctx context.Context, in subAgentInput) subAgentResult {
	// 系统提示 = 与主 agent 共用的核心(身份+规则+workspace+skill)+ 子 agent 专属尾部。
	// 共用核心逐字节一致 → 与主 agent / 同模型兄弟节点共享缓存前缀;同时子 agent 也拿到了
	// 安全/模式规则和 skill 目录(LoadSkill 因此可用)。专属部分放尾部,只有它是 miss。
	var sb strings.Builder
	sb.WriteString(coreSystemPrompt(in.Workspace, in.SkillCatalog))
	sb.WriteString("\n\n# 子 agent 任务\n你是 deepx 的子 agent,只负责完成下面这一项,禁止 CreatePlan / Todo / SwitchModel / Workflow(只做被分派的事,不要再拆分、维护待办、换模型或创建运行 workflow)。")
	sb.WriteString("\n- 用户的原始任务背景: ")
	sb.WriteString(in.UserTask)
	sb.WriteString("\n- 你这一项的具体目标: ")
	sb.WriteString(in.NodeTitle)
	if len(in.Predecessors) > 0 {
		sb.WriteString("\n\n上游已完成节点的产出 (作为上下文使用):")
		for id, sum := range in.Predecessors {
			sb.WriteString("\n- [")
			sb.WriteString(id)
			sb.WriteString("] ")
			sb.WriteString(sum)
		}
	}
	if len(in.Schema) > 0 {
		// 结构化结果:走 structured_output 工具提交(对齐 Claude Code),不写在正文里。
		sb.WriteString("\n\n**本任务要求结构化结果:先按需调研(可读文件/跑命令),完成后【必须调用 `structured_output` 工具】提交最终结果——只调一次,参数须符合该工具的 schema;不要把结果写在正文里、不要用 markdown。**")
	} else if in.FullOutput {
		sb.WriteString("\n\n完成后直接输出你这一项的完整结果(按任务要求,可以是报告 / JSON / 列表等),不要写客套或额外说明。")
	} else {
		sb.WriteString("\n\n完成后只输出一段简短(<200 字)的结果总结。不要写多余的客套。")
	}

	convo := []ChatMessage{
		{Role: "system", Content: sb.String()},
		{Role: "user", Content: in.NodeTitle},
	}

	toolSpecs := buildSubAgentToolSpecs(in.Mode)
	if len(in.Schema) > 0 {
		// 把 schema 注册成一个工具:模型【调用它】来交结果,结果取自工具 arguments(天生合法 JSON),
		// 而非在正文里写 JSON 再解析。这是结构化输出可靠的根本(forced-tool-call,非 prompt+parse)。
		toolSpecs = append(toolSpecs, tools.OpenAIToolSpec{
			Type: "function",
			Function: tools.OpenAIFunctionSpec{
				Name:          "structured_output",
				Description:   "提交本次任务的最终结构化结果。完成调研后必须调用一次,参数即结果(须符合 schema)。",
				RawParameters: json.RawMessage(in.Schema),
			},
		})
	}

	// 静默 channel:streamOnce 的 TokenMsg 不进 UI,内部 drain 掉
	silent := make(chan tea.Msg, 64)
	drained := make(chan struct{})
	go func() {
		for range silent {
		}
		close(drained)
	}()

	// 上下文预算熔断:子 agent 不做压缩,convo 只增不减,读几个大文件就可能撑爆窗口。
	// 每轮前估算,超过窗口的 ctxBudgetPct 就主动中止(干净失败),而不是等 API 报错或耗满轮数。
	ctxWin := in.Entry.ContextWindow
	if ctxWin <= 0 {
		ctxWin = 65536
	}
	ctxBudget := ctxWin * subAgentCtxBudgetPct / 100

	// lastFile = 最近操作的文件路径,给 Update 漏 path 时兜底回填(issue #81)。
	var lastFile string

	// noProgressRounds = 连续「全部工具调用失败」的轮数;到 maxNoProgressRounds 判卡死中止。
	// 无固定轮数上限,跑到模型自己停为止;ctxBudget + 本断路器 + ctx 取消三道边界兜底。
	noProgressRounds := 0

	// report 累积每一轮的 assistant 文本。模型可能边输出边调工具(尤其 FullOutput 写长报告时),
	// 内容分散在多轮;只取最后一轮会丢掉之前那几轮的正文(issue:汇总报告前半段丢失)。
	//   - 工具调用穿插的多段正文之间补 "\n\n"(独立段落);
	//   - 因 length 截断而续写的下一段,紧接前一段(prevWasLength)不补分隔,避免词中断处插入空行。
	var report strings.Builder
	prevWasLength := false // 上一轮是否因 max_tokens 截断(下一段是它的续写)
	continueRounds := 0    // 已因 length 续写的轮数(上限 maxSubAgentContinueRounds)
	badOutputRetries := 0  // 已因输出不合格(退化 / 非合法 JSON)重试的次数(上限 maxBadOutputRetries)

	for {
		// 检查 context 是否取消(ESC/退出)
		if ctx.Err() != nil {
			close(silent)
			<-drained
			return subAgentResult{Err: ctx.Err()}
		}
		// 上下文预算熔断:超预算就停,避免脏失败(API 超长报错)和卡死时的 token 浪费。
		if est := estimateConvoTokens(convo); est >= ctxBudget {
			close(silent)
			<-drained
			return subAgentResult{Err: fmt.Errorf("子 agent [%s] 上下文超预算(~%d/%d tokens),中止", in.NodeID, est, ctxWin)}
		}
		// 不主动 strip reasoning:本轮锁定模型,thinking 模型仍正常回传,
		// 非 thinking 模型忽略 history 里的 reasoning_content 字段(omitempty 已处理空值)。
		content, reasoning, toolCalls, finishReason, _, err := streamOnceFn(
			ctx,
			in.Entry.APIKey, in.Entry.BaseURL, in.Entry.Model,
			convo, clampMaxTokens(in.Entry.MaxTokens, in.Entry.ContextWindow, convo), toolSpecs,
			in.Entry.ReasoningEffort, in.Entry.Thinking,
			silent,
		)
		if err != nil {
			close(silent)
			<-drained
			return subAgentResult{Err: err}
		}

		// 必须把 reasoning_content 存进 history,thinking 模型下一轮要求原样回传。
		// 之前丢这个字段是 sub-agent 400 "reasoning_content must be passed back" 的根因。
		convo = append(convo, ChatMessage{
			Role:             "assistant",
			Content:          content,
			ReasoningContent: reasoning,
			ToolCalls:        toolCalls,
		})
		if t := strings.TrimSpace(content); t != "" {
			if report.Len() > 0 && !prevWasLength {
				report.WriteString("\n\n") // 独立段落之间补空行;length 续写则紧接(见上方说明)
			}
			report.WriteString(t)
		}
		prevWasLength = false // 默认重置;仅 length 续写分支置回 true

		if len(toolCalls) == 0 {
			// 因 max_tokens 截断且没发起工具调用 → 长输出还没写完。让模型接着写,
			// 续写内容累积进 report(否则尾部丢失);有 maxSubAgentContinueRounds + ctxBudget 兜底。
			if finishReason == "length" && continueRounds < maxSubAgentContinueRounds {
				continueRounds++
				prevWasLength = true
				convo = append(convo, ChatMessage{
					Role:    "user",
					Content: "上次回答因长度上限被截断,请紧接着中断处继续输出剩余内容;不要重复任何已输出的文字,也不要重新开头或加客套。",
				})
				continue
			}
			summary := strings.TrimSpace(report.String())
			// 输出不合格 → 丢弃重来。两类:① FullOutput 退化(空/{}/[]/null);
			// ② WantJSON 却不是合法 JSON(模型返回了 markdown / ``` 围栏 / 解释文字)。
			bad := (in.FullOutput && isDegenerateOutput(summary)) ||
				(in.WantJSON && !json.Valid([]byte(extractJSONBlock(summary))))
			if bad && badOutputRetries < maxBadOutputRetries {
				badOutputRetries++
				report.Reset()
				prevWasLength = false
				nudge := "你上一条回复无效(为空,或只有 \"{}\" / \"null\" / \"[]\")。请严格按任务要求输出完整的正文内容,不要只返回空对象,也不要再调用工具。"
				if in.WantJSON {
					// 声明了 schema 的步骤:正确方式是【调用 structured_output 工具】,而不是在正文写 JSON。
					nudge = "你还没有用 `structured_output` 工具提交有效结果(把结果写在了正文 / 是 markdown / 为空)。请【现在调用 `structured_output` 工具】提交最终结果,参数须符合其 schema 且含实际内容,不要把结果写在正文里。"
				}
				convo = append(convo, ChatMessage{Role: "user", Content: nudge})
				continue
			}
			// 重试用尽:非 JSON 的最终步退化 → 给明确说明,别把 {} 当结果交出去;
			// JSON 仍不合法 → 原样返回(下游 JSON.parse 兜成 null,脚本应 `x?.findings || []` 防御)。
			if bad && in.FullOutput && !in.WantJSON {
				summary = fmt.Sprintf("(子 agent [%s] 连续 %d 次只返回空结果,模型可能对该输入不稳定——建议换更强模型或缩小该步骤输入)", in.NodeID, badOutputRetries+1)
			}
			// 子 agent 完成,返回累积的全部 assistant 文本作为 summary(不止最后一轮)
			close(silent)
			<-drained
			if summary == "" {
				summary = "(子 agent 未给出明确结论)"
			}
			return subAgentResult{Summary: summary}
		}

		// 边界:若本轮 finishReason=="length" 且**带**工具调用,工具参数可能被截断成半个 JSON。
		// 不特殊处理——executeTool 解析坏参数会失败、计入下方无进展断路器(maxNoProgressRounds)兜底,
		// 不会卡死。这种「截断处恰好在工具调用中」的情况罕见。
		roundProgress := false
		structured := "" // 本轮若调用了 structured_output,捕获其 arguments 作为结构化结果
		for _, tc := range toolCalls {
			var result tools.ToolResult
			switch tc.Function.Name {
			case "structured_output":
				// schema 结果由这个工具调用提交:arguments 即结果(API 按 schema 序列化,天生合法 JSON)。
				structured = tc.Function.Arguments
				result = tools.ToolResult{Output: "已收到结构化结果。", Success: true}
			case "UpdatePlanStatus":
				// 子 agent 想自报状态,吞掉给 OK。scheduler 才是状态来源。
				// 视为成功(进展),避免无进展断路器误伤纯状态上报。
				result = tools.ToolResult{Output: "已记录", Success: true}
			default:
				result = executeTool(tc, in.Mode, &lastFile)
			}
			if result.Success {
				roundProgress = true
			}
			convo = append(convo, ChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result.Output,
			})
		}

		// 调用了 structured_output → 拿到结构化结果即终止(结果是合法 JSON,无需解析正文)。
		if structured != "" {
			close(silent)
			<-drained
			return subAgentResult{Summary: structured}
		}

		// 无进展断路器:本轮工具全失败则累加,任一成功则归零;连续卡死到上限就中止本节点。
		if roundProgress {
			noProgressRounds = 0
		} else {
			noProgressRounds++
			if noProgressRounds >= maxNoProgressRounds {
				close(silent)
				<-drained
				return subAgentResult{Err: fmt.Errorf("子 agent [%s] 连续 %d 轮工具调用均未成功,疑似卡死,中止", in.NodeID, maxNoProgressRounds)}
			}
		}
	}
}

// resolveModelEntry 把 plan/task 里 "flash" / "pro" 字符串映射到 ModelConfig 里的完整 entry。
// roleHint 解析:
//   - "pro" / "Pro" → 返回 cfg.Pro(若有 model id)
//   - "flash" / "" / 其他 → 返回 cfg.Flash(若有 model id),否则退到 cfg.Pro
//
// 兜底逻辑保证不会返回空 entry,即使节点的 model 字段误填也能跑。
func resolveModelEntry(roleHint string, cfg ModelConfig) ModelEntry {
	switch strings.ToLower(strings.TrimSpace(roleHint)) {
	case "pro":
		if cfg.Pro.Model != "" {
			return cfg.Pro
		}
	case "flash", "":
		// 走默认
	}
	if cfg.Flash.Model != "" {
		return cfg.Flash
	}
	return cfg.Pro
}
