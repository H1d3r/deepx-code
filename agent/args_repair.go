package agent

import (
	"encoding/json"
	"strings"
)

// === 工具调用 arguments 修复(issue #201 的 400 根因)===
//
// vLLM 等严格后端在预处理请求时,会对历史里每条 assistant 消息的 tool_calls[].function.arguments
// 做 json.loads:任何一条不是合法 JSON,整个请求 400,且会话存盘后永远 400(不可恢复)。
// 非法 arguments 的来源:模型输出空参数("")、小模型吐语法坏 JSON、截断信号缺失时残缺 JSON 落历史。
// DeepSeek 官方等宽松后端不做这个 parse,所以同样的历史在那边不报错——问题只在严格后端暴露。
//
// 修复原则:**结构合法是硬要求,语义恢复是尽力而为**。坏 arguments 早已执行过(当时已回
// "参数解析失败"给模型),留在历史里只是给模型回看上下文,修复不损失有效信息:
//   - ""/"null"        → "{}"(无参调用的规范形态)
//   - 截断的 JSON      → 确定性补全(闭合字符串/括号、悬空键补 null、裁尾逗号)
//   - 修不出来的垃圾   → 包成 {"_raw":"<原文>"} 兜底,原文不丢
//
// 两个使用层:
//   - 入历史(rewriteToolCallArgsForHistory):防新毒,保证今后写入的 arguments 永远合法;
//   - 发送前(streamAttempt / CallWithTools):救已中毒的存量会话——毒消息已 gob 存盘,
//     只有出站修复能让这些会话复活。修复是纯函数、确定性:同样输入永远修出同样字节,
//     历史逐字节稳定,前缀缓存不因修复而抖动。

// repairArgsJSON 把单个 arguments 字符串修成合法 JSON。合法输入原样返回(零字节变化)。
// 幂等:输出必为合法 JSON,再过一遍必然原样返回。
func repairArgsJSON(s string) string {
	if t := strings.TrimSpace(s); t == "" || t == "null" {
		return "{}"
	}
	if json.Valid([]byte(s)) {
		return s
	}
	if fixed, ok := completeTruncatedJSON(s); ok {
		return fixed
	}
	b, err := json.Marshal(map[string]string{"_raw": s})
	if err != nil { // string 编码不会失败;防御性兜底
		return "{}"
	}
	return string(b)
}

// repairToolCallArgs 对消息序列做发送前 arguments 修复。copy-on-write:msgs 及其 ToolCalls
// 底层数组与存储的 history 共享,绝不能就地改(否则 marshal 副作用悄悄改写会话状态);
// 无需修复时原样返回原切片(正常会话零开销、零字节变化)。
func repairToolCallArgs(msgs []ChatMessage) []ChatMessage {
	out := msgs
	copied := false
	for i := range msgs {
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		var fixed []ToolCall
		for j, tc := range msgs[i].ToolCalls {
			r := repairArgsJSON(tc.Function.Arguments)
			if r == tc.Function.Arguments {
				continue
			}
			if fixed == nil {
				fixed = append([]ToolCall(nil), msgs[i].ToolCalls...)
			}
			fixed[j].Function.Arguments = r
		}
		if fixed != nil {
			if !copied {
				out = append([]ChatMessage(nil), msgs...)
				copied = true
			}
			out[i].ToolCalls = fixed
		}
	}
	return out
}

// completeTruncatedJSON 确定性补全被截断的 JSON:单遍扫描维护括号栈 + 字符串/转义状态,
// 在截断点闭合未完的字符串、补全悬空的键(:null)/值(null)/字面量(tru→true)、裁掉尾逗号,
// 再按栈逆序补闭合括号。只处理"合法前缀被截断"——前缀本身语法错乱的,最终 json.Valid
// 校验不过,返回 ok=false 交给 _raw 兜底。
func completeTruncatedJSON(s string) (string, bool) {
	const (
		phExpectKeyOrClose = iota // 对象:等键或 '}'(含 '{' 起始与 ',' 之后)
		phAfterKey                // 对象:键完成,等 ':'
		phExpectValue             // 等值('{' 的 ':' 之后;数组起始与 ',' 之后)
		phAfterValue              // 值完成,等 ',' 或闭合
	)
	type frame struct {
		kind  byte // '{' 或 '['
		phase int
	}
	var stack []frame
	inStr, esc := false, false
	strIsKey := false
	inLit := false
	litStart := 0
	pendingComma := -1 // 最近一个其后无新 token 的 ',' 下标;-1 = 无

	top := func() *frame {
		if len(stack) == 0 {
			return nil
		}
		return &stack[len(stack)-1]
	}
	closeValue := func() { // 一个值(字符串/字面量/对象/数组)完成 → 推进所在容器状态
		if f := top(); f != nil {
			f.phase = phAfterValue
		}
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
				if strIsKey {
					top().phase = phAfterKey
				} else {
					closeValue()
				}
			}
			continue
		}
		if inLit {
			if isLitByte(c) {
				continue
			}
			inLit = false
			closeValue()
			// c 本身还是结构字符,落到下面的 switch 继续处理
		}
		switch c {
		case ' ', '\t', '\n', '\r':
		case '"':
			inStr = true
			f := top()
			strIsKey = f != nil && f.kind == '{' && f.phase == phExpectKeyOrClose
			pendingComma = -1
		case '{':
			stack = append(stack, frame{'{', phExpectKeyOrClose})
			pendingComma = -1
		case '[':
			stack = append(stack, frame{'[', phExpectValue})
			pendingComma = -1
		case '}', ']':
			if len(stack) == 0 {
				return "", false // 多余闭合,前缀本身错乱
			}
			stack = stack[:len(stack)-1]
			closeValue()
			pendingComma = -1
		case ':':
			if f := top(); f != nil && f.kind == '{' {
				f.phase = phExpectValue
			}
			pendingComma = -1
		case ',':
			if f := top(); f != nil {
				if f.kind == '{' {
					f.phase = phExpectKeyOrClose
				} else {
					f.phase = phExpectValue
				}
			}
			pendingComma = i
		default:
			inLit = true
			litStart = i
			pendingComma = -1
		}
	}

	var b strings.Builder
	end := len(s)
	if pendingComma >= 0 && !inStr && !inLit {
		end = pendingComma // 尾逗号后无内容:裁掉,否则 {"a":1,} / [1,] 非法
	}
	b.WriteString(s[:end])
	if inStr {
		if esc {
			b.WriteByte('\\') // 悬空转义补成 '\\',再闭合
		}
		b.WriteByte('"')
		if strIsKey {
			top().phase = phAfterKey
		} else {
			closeValue()
		}
	}
	if inLit {
		lit := s[litStart:]
		b.WriteString(litCompletion(lit)[len(lit):]) // 前缀已含 lit 本体,只追加补全增量
		closeValue()
	}
	if f := top(); f != nil && f.kind == '{' {
		switch f.phase {
		case phAfterKey: // {"key"  → 键悬空,补 :null
			b.WriteString(":null")
		case phExpectValue: // {"key":  → 值悬空,补 null
			b.WriteString("null")
		}
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].kind == '{' {
			b.WriteByte('}')
		} else {
			b.WriteByte(']')
		}
	}
	out := b.String()
	if !json.Valid([]byte(out)) {
		return "", false
	}
	return out, true
}

// litCompletion 补全被截断的字面量/数字:true/false/null 的前缀补成完整词;
// 数字断在 -/+/./e/E 处补 '0';其余原样(交给最终 json.Valid 裁决)。
func litCompletion(lit string) string {
	for _, w := range [...]string{"true", "false", "null"} {
		if len(lit) < len(w) && strings.HasPrefix(w, lit) {
			return w
		}
	}
	switch lit[len(lit)-1] {
	case '-', '+', '.', 'e', 'E':
		return lit + "0"
	}
	return lit
}

// isLitByte 判断是否为字面量/数字 token 的组成字符。
func isLitByte(c byte) bool {
	return c >= '0' && c <= '9' ||
		c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c == '-' || c == '+' || c == '.' || c == '_'
}
