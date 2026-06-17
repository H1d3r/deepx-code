package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/fastschema/qjs"
)

// batchResult 是 __agentBatch 回给 JS 的单个结果(JSON:{ok, result, error})。
type batchResult struct {
	OK bool `json:"ok"`
	// Result 不加 omitempty:空串结果也要显式带上,否则 JS 侧 r.result 变 undefined,
	// parallel 回填后拼进字符串会成 "undefined"。
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// runBatch 并发跑一批 agent payload,保序返回结果。
//
// 并发只在本函数内的 worker goroutine 里发生(纯 Go 的 Executor.Agent);调用方(JS 线程)
// 阻塞等 wg.Wait,因此不存在跨 goroutine 触碰 wasm 的问题。并发度由 opts.concurrency() 限。
// baseSeq 是这批 payload 的起始执行序号:第 i 个 payload 的 seq = baseSeq+i(用于 resume 缓存)。
func runBatch(ctx context.Context, baseSeq int, payloads []string, opts RunOptions) []batchResult {
	results := make([]batchResult, len(payloads))
	sem := make(chan struct{}, opts.concurrency())
	var wg sync.WaitGroup
	for i, p := range payloads {
		wg.Add(1)
		go func(i int, payload string) {
			defer wg.Done()
			seq := baseSeq + i
			// resume:命中缓存直接用,不占并发槽、不真跑(Get 需并发安全,见 ResultStore)。
			if opts.Results != nil {
				if r, ok := opts.Results.Get(seq); ok {
					results[i] = batchResult{OK: true, Result: r}
					return
				}
			}
			sem <- struct{}{}
			defer func() { <-sem }()

			var call AgentCall
			if err := json.Unmarshal([]byte(payload), &call); err != nil {
				results[i] = batchResult{Error: "参数解析失败: " + err.Error()}
				return
			}
			if err := ctx.Err(); err != nil {
				results[i] = batchResult{Error: err.Error()}
				return
			}
			res, err := opts.Executor.Agent(ctx, call)
			if err != nil {
				results[i] = batchResult{Error: err.Error()}
				return
			}
			if opts.Results != nil {
				opts.Results.Put(seq, res)
			}
			results[i] = batchResult{OK: true, Result: res}
		}(i, p)
	}
	wg.Wait()
	return results
}

// Run 执行一个 workflow 脚本,返回它 main(args) 的最终结果(字符串;若返回对象则为其 JSON)。
//
// 流程:建 qjs runtime → 注入原语 __agent/__log/__phase/__budget* → 跑 prelude 定义全局 API
// → 注入 args 全局 → 以 ES module 形式加载用户脚本 → 取 default 导出(main)→ invoke 并 await。
//
// ctx 取消会传递给 Executor.Agent;脚本本身在 WASM 沙箱里跑,只能通过注入的原语触达外界。
func Run(ctx context.Context, script *Script, opts RunOptions) (string, error) {
	if script == nil {
		return "", fmt.Errorf("workflow: script 为空")
	}
	if opts.Executor == nil {
		return "", fmt.Errorf("workflow: 未提供 Executor")
	}

	rt, err := qjs.New()
	if err != nil {
		return "", fmt.Errorf("workflow: 创建 JS 运行时失败: %w", err)
	}
	defer rt.Close()
	qc := rt.Context()

	bindPrimitives(ctx, qc, opts)

	// prelude:全局脚本(非 module),把对外 API 定义到 globalThis。
	// 说明:本函数用的是一次性 runtime,结束时 rt.Close() 会整体回收所有 JS 值。
	// qjs 的值是引用计数,手动 Free 一旦重复就触发 WASM trap,所以这里不逐个 Free,
	// 统一交给 Close —— runtime 短命,代价只是延迟到函数返回时回收,安全得多。
	if _, err := qc.Eval("deepx-workflow-prelude.js", qjs.Code(preludeJS)); err != nil {
		return "", fmt.Errorf("workflow: 加载内置 prelude 失败: %w", err)
	}

	// 注入 args 全局(JSON 往返;nil → undefined)。
	if err := setArgsGlobal(qc, opts.Args); err != nil {
		return "", err
	}

	// 用户脚本以 ES module 加载;qjs 的 module eval 直接返回 default 导出本身,
	// 即脚本里 `export default async function main(args)` 的那个函数。
	mainFn, err := qc.Eval(script.fileName(), qjs.Code(script.Source), qjs.TypeModule())
	if err != nil {
		return "", fmt.Errorf("workflow %q: 脚本解析/执行失败: %w", script.Name, err)
	}
	if !mainFn.IsFunction() {
		return "", fmt.Errorf("workflow %q: 脚本必须 `export default async function main(args)`", script.Name)
	}

	argsVal := qc.Global().GetPropertyStr("args")
	res, err := qc.Invoke(mainFn, qc.NewUndefined(), argsVal)
	if err != nil {
		return "", fmt.Errorf("workflow %q: 执行 main 失败: %w", script.Name, err)
	}

	// async function → 返回 promise,await 到落定。
	if res.IsPromise() {
		settled, err := res.Await()
		if err != nil {
			return "", fmt.Errorf("workflow %q: %w", script.Name, err)
		}
		return valueToString(settled), nil
	}
	return valueToString(res), nil
}

// bindPrimitives 把引擎底层原语注入 globalThis,供 prelude 里的 API 调用。
func bindPrimitives(ctx context.Context, qc *qjs.Context, opts RunOptions) {
	// seq 是 agent 执行序号:每个 agent 按 JS 执行顺序分配一个,作为 resume 缓存的 key。
	// __agentSync / __agentBatch 都在 JS 线程上被同步调用(单线程),所以这个计数器无需加锁。
	seq := 0
	maxAgents := opts.maxAgents() // 子 agent 总数兜底:失控/死循环时中止(见 RunOptions.MaxAgents)

	// __agentSync(payloadJSON) -> resultString:同步阻塞执行一个子 agent。
	// 用于单独 await 和 pipeline 顺序场景(全在 JS 线程上,无并发)。
	qc.SetFunc("__agentSync", func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		if len(args) == 0 {
			return nil, fmt.Errorf("__agentSync: 缺少参数")
		}
		if seq >= maxAgents {
			return nil, fmt.Errorf("workflow: 子 agent 总数已达上限 %d(疑似脚本死循环/失控),已中止", maxAgents)
		}
		s := seq
		seq++
		// resume:命中缓存直接返回,不真跑。
		if opts.Results != nil {
			if r, ok := opts.Results.Get(s); ok {
				return this.Context().NewString(r), nil
			}
		}
		var call AgentCall
		if err := json.Unmarshal([]byte(args[0].String()), &call); err != nil {
			return nil, fmt.Errorf("__agentSync: 参数解析失败: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result, err := opts.Executor.Agent(ctx, call)
		if err != nil {
			return nil, err // → JS 异常 → agent() 的 rejected promise
		}
		if opts.Results != nil {
			opts.Results.Put(s, result)
		}
		return this.Context().NewString(result), nil
	})

	// __agentBatch(payloadsJSON) -> resultsJSON:parallel() 的并发执行原语。
	// 真并发的安全前提:整个并发只发生在这一次同步 Go 调用「内部」——JS 线程在此阻塞,
	// 不会有别的 goroutine 触碰 wasm;worker 跑的是纯 Go 的 Executor.Agent。
	qc.SetFunc("__agentBatch", func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		if len(args) == 0 {
			return nil, fmt.Errorf("__agentBatch: 缺少参数")
		}
		var payloads []string
		if err := json.Unmarshal([]byte(args[0].String()), &payloads); err != nil {
			return nil, fmt.Errorf("__agentBatch: 参数解析失败: %w", err)
		}
		base := seq // 这批的起始序号,逐个 base+i;批内 seq 连续(JS 单线程,确定)
		seq += len(payloads)
		if seq > maxAgents {
			return nil, fmt.Errorf("workflow: 子 agent 总数超过上限 %d(parallel 批量,疑似脚本失控),已中止", maxAgents)
		}
		results := runBatch(ctx, base, payloads, opts)
		out, err := json.Marshal(results)
		if err != nil {
			return nil, err
		}
		return this.Context().NewString(string(out)), nil
	})

	// __workflow(name, argsJSON) -> resultString:嵌套调用另一个 workflow(同步阻塞跑完子 workflow)。
	qc.SetFunc("__workflow", func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		if len(args) < 2 {
			return nil, fmt.Errorf("__workflow: 缺少参数")
		}
		if opts.RunChild == nil {
			return nil, fmt.Errorf("workflow(): 当前不支持嵌套调用")
		}
		name := args[0].String()
		var childArgs any
		if s := args[1].String(); s != "" && s != "null" {
			_ = json.Unmarshal([]byte(s), &childArgs)
		}
		res, err := opts.RunChild(ctx, name, childArgs)
		if err != nil {
			return nil, err
		}
		return this.Context().NewString(res), nil
	})

	qc.SetFunc("__log", func(this *qjs.This) (*qjs.Value, error) {
		if a := this.Args(); len(a) > 0 && opts.OnLog != nil {
			opts.OnLog(a[0].String())
		}
		return this.Context().NewUndefined(), nil
	})

	qc.SetFunc("__phase", func(this *qjs.This) (*qjs.Value, error) {
		if a := this.Args(); len(a) > 0 && opts.OnPhase != nil {
			opts.OnPhase(a[0].String())
		}
		return this.Context().NewUndefined(), nil
	})

	qc.SetFunc("__budgetTotal", func(this *qjs.This) (*qjs.Value, error) {
		return this.Context().NewInt64(int64(opts.Budget.total())), nil
	})
	qc.SetFunc("__budgetSpent", func(this *qjs.This) (*qjs.Value, error) {
		return this.Context().NewInt64(int64(opts.Budget.spent())), nil
	})
}

// setArgsGlobal 把 Go 侧 args 经 JSON 往返注入成 JS 全局 args。
func setArgsGlobal(qc *qjs.Context, args any) error {
	if args == nil {
		qc.Global().SetPropertyStr("args", qc.NewUndefined())
		return nil
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("workflow: args 序列化失败: %w", err)
	}
	qc.Global().SetPropertyStr("args", qc.ParseJSON(string(raw)))
	return nil
}

// valueToString 把脚本返回值转成字符串:字符串原样,其余尝试 JSON 序列化。
func valueToString(v *qjs.Value) string {
	if v == nil || v.IsUndefined() || v.IsNull() {
		return ""
	}
	if v.IsString() {
		return v.String()
	}
	if s, err := v.JSONStringify(); err == nil && s != "" {
		return s
	}
	return v.String()
}
