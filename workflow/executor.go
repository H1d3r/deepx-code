// Package workflow 是 deepx 的 workflow 引擎:把一个 JavaScript 脚本(导出 meta +
// 一个 async main)当作「固定流程的编排脚本」来跑,脚本里通过 agent() / parallel() /
// pipeline() / phase() / log() / budget / args 这套全局函数驱动子 agent。
//
// 这套全局 API 对齐 Claude Code 的 workflow 脚本约定(公开的事实标准),因此为该约定
// 写的脚本可以直接放进 deepx 的 workflow 目录运行。
//
// 设计要点:
//   - 本包**不导入 deepx 的任何业务包**(agent / tui …),只依赖 qjs + 标准库。
//     真正干活的「跑一个子 agent」由调用方实现 Executor 注入,从根上杜绝 import 环。
//   - JS 运行时用 github.com/fastschema/qjs(QuickJS 编译成 WASM、跑在 wazero 上,纯 Go
//     无 cgo),脚本在 WASM 沙箱里执行,只能通过我们显式注入的几个函数触达外界。
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
)

// AgentCall 是 JS 里一次 agent(prompt, opts) 调用解析后的全部参数。
// 字段名对齐公开的 workflow 脚本约定(label / phase / model / schema /
// capabilities / max_tool_iters / max_tool_calls)。
type AgentCall struct {
	Prompt string `json:"prompt"`
	Label  string `json:"label"`
	Phase  string `json:"phase"`
	Model  string `json:"model"`

	// Schema 非空时,要求子 agent 产出符合该 JSON Schema 的结构化结果;
	// 引擎会把返回的 JSON 字符串在 JS 侧 JSON.parse 成对象。
	Schema json.RawMessage `json:"schema,omitempty"`

	// Capabilities 限定子 agent 可用的工具子集(空 = 不限制)。
	Capabilities []string `json:"capabilities,omitempty"`

	MaxToolIters int `json:"max_tool_iters,omitempty"`
	MaxToolCalls int `json:"max_tool_calls,omitempty"`
}

// HasSchema 判断这次调用是否真带了 schema。注意:prelude 对没传 schema 的调用会发 `"schema":null`,
// json.RawMessage 会把字面量 `null`(4 字节)收进来——所以不能用 len()>0 判断,得排除 null/空。
func (c AgentCall) HasSchema() bool {
	s := bytes.TrimSpace(c.Schema)
	return len(s) > 0 && !bytes.Equal(s, []byte("null"))
}

// Executor 由调用方实现:跑一个子 agent,返回它的最终文本结果。
//
// 当 call.Schema 非空时,返回值应当是一段**合法 JSON 字符串**(符合该 schema),
// 引擎会在 JS 侧 JSON.parse;否则返回普通文本。返回 error 会在脚本里变成
// agent() 的 rejected promise。
type Executor interface {
	Agent(ctx context.Context, call AgentCall) (string, error)
}

// ResultStore 是 resume 用的「已完成 agent 结果」存储:按 agent 在脚本里的确定性执行序号(seq)
// 缓存成功结果。中断后重跑同一 workflow 时,已完成的 seq 直接命中缓存、不再真跑,只跑剩下的。
//
// seq 由引擎按 JS 执行顺序分配(单线程确定);确定性靠引擎屏蔽 Math.random/Date.now 保证——
// 否则重放时调用序列会错位,缓存失效。只缓存成功结果;失败/未完成的 seq 下次重跑。
type ResultStore interface {
	Get(seq int) (string, bool)
	Put(seq int, result string)
}

// ExecutorFunc 让普通函数直接当 Executor 用。
type ExecutorFunc func(ctx context.Context, call AgentCall) (string, error)

func (f ExecutorFunc) Agent(ctx context.Context, call AgentCall) (string, error) {
	return f(ctx, call)
}

// Budget 把一轮的 token 预算暴露给脚本的 budget 对象。
// Total <= 0 表示「未设上限」,脚本里 budget.total 读到 null、remaining() 读到 Infinity。
// Spent 为 nil 时按 0 计。
type Budget struct {
	Total int
	Spent func() int
}

func (b *Budget) total() int {
	if b == nil || b.Total <= 0 {
		return -1
	}
	return b.Total
}

func (b *Budget) spent() int {
	if b == nil || b.Spent == nil {
		return 0
	}
	return b.Spent()
}

// RunOptions 是 Run 的全部可选依赖。Executor 必填,其余可空。
type RunOptions struct {
	// Args 透传给脚本的 main(args) 和全局 args(JSON 往返,任意可序列化值)。
	Args any
	// Executor 跑子 agent 的实现(必填)。
	Executor Executor
	// OnPhase / OnLog 接收脚本里 phase() / log() 的调用,驱动 UI 进度(可空)。
	OnPhase func(title string)
	OnLog   func(message string)
	// Budget 本轮预算(可空 = 不限制)。
	Budget *Budget
	// MaxConcurrency 限制 parallel() 同时在跑的 agent 数(子 agent 重,默认 8)。<=0 取默认。
	MaxConcurrency int
	// MaxAgents 是单次运行能创建的子 agent 总数硬上限(失控/死循环兜底)。<=0 取默认 defaultMaxAgents。
	// 超过后 agent()/parallel() 直接抛错中止整个 workflow。按「执行序号」计:resume 命中缓存的
	// agent 也占一个序号(replay 不会超过真实调用数,但失控的无限循环会撞上限)。注:每个嵌套层各有
	// 独立计数(限一层,总量有界)。
	MaxAgents int
	// Results 非空时启用 resume:命中缓存的 agent 不再真跑(见 ResultStore)。
	Results ResultStore
	// RunChild 实现脚本里的 workflow(name, args):按名加载并运行另一个 workflow,返回其结果。
	// nil = 不支持嵌套(workflow() 直接报错)。调用方负责「限一层」(子 workflow 的 RunChild 应为 nil)。
	RunChild func(ctx context.Context, name string, args any) (string, error)
}

// concurrency 返回有效并发上限(默认 8)。
func (o RunOptions) concurrency() int {
	if o.MaxConcurrency <= 0 {
		return 8
	}
	return o.MaxConcurrency
}

// defaultMaxAgents 是子 agent 总数兜底上限:远高于任何正常 workflow(通常几个~几十个),
// 又能拦住失控的无限循环。子 agent 是真实 LLM 调用,无上限的脚本死循环会持续烧 token。
const defaultMaxAgents = 500

// maxAgents 返回有效的子 agent 总数上限(默认 defaultMaxAgents)。
func (o RunOptions) maxAgents() int {
	if o.MaxAgents <= 0 {
		return defaultMaxAgents
	}
	return o.MaxAgents
}
