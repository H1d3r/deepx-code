package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// mockExec 把每次 agent 调用记录下来,并按 prompt 回一个可预测的结果。
type mockExec struct {
	mu    sync.Mutex
	calls []AgentCall
	reply func(call AgentCall) (string, error)
}

func (m *mockExec) Agent(ctx context.Context, call AgentCall) (string, error) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	if m.reply != nil {
		return m.reply(call)
	}
	return "echo:" + call.Prompt, nil
}

func run(t *testing.T, src string, opts RunOptions) (string, error) {
	t.Helper()
	if opts.Executor == nil {
		opts.Executor = &mockExec{}
	}
	return Run(context.Background(), &Script{Name: "t", Source: src}, opts)
}

func TestRun_SingleAgent(t *testing.T) {
	src := `
export const meta = { name: "t", description: "x" };
export default async function main(args) {
  const r = await agent("hello");
  return r;
}`
	got, err := run(t, src, RunOptions{})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if got != "echo:hello" {
		t.Fatalf("got %q, want %q", got, "echo:hello")
	}
}

func TestRun_Args(t *testing.T) {
	src := `
export const meta = { name: "t" };
export default async function main(args) {
  return await agent("v=" + args.version);
}`
	got, err := run(t, src, RunOptions{Args: map[string]any{"version": "1.2.0"}})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if got != "echo:v=1.2.0" {
		t.Fatalf("got %q", got)
	}
}

func TestRun_Parallel(t *testing.T) {
	src := `
export const meta = { name: "t" };
export default async function main() {
  const r = await parallel([
    () => agent("a"),
    () => agent("b"),
    () => agent("c"),
  ]);
  return r.join(",");
}`
	got, err := run(t, src, RunOptions{})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if got != "echo:a,echo:b,echo:c" {
		t.Fatalf("got %q", got)
	}
}

func TestRun_Pipeline(t *testing.T) {
	ex := &mockExec{reply: func(c AgentCall) (string, error) { return strings.ToUpper(c.Prompt), nil }}
	src := `
export const meta = { name: "t" };
export default async function main() {
  const out = await pipeline(
    ["x", "y"],
    (item) => agent(item),
    (prev, item, i) => agent(prev + "-" + item + "-" + i),
  );
  return out.join(",");
}`
	got, err := run(t, src, RunOptions{Executor: ex})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	// stage1: x->X, y->Y ; stage2: agent("X-x-0")->"X-X-0", agent("Y-y-1")->"Y-Y-1"
	if got != "X-X-0,Y-Y-1" {
		t.Fatalf("got %q", got)
	}
}

func TestRun_Schema(t *testing.T) {
	ex := &mockExec{reply: func(c AgentCall) (string, error) {
		if len(c.Schema) == 0 {
			t.Errorf("expected schema passed through")
		}
		return `{"score": 7, "title": "ok"}`, nil
	}}
	src := `
export const meta = { name: "t" };
export default async function main() {
  const r = await agent("review", { schema: { type: "object" } });
  return "score=" + r.score + " title=" + r.title;
}`
	got, err := run(t, src, RunOptions{Executor: ex})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if got != "score=7 title=ok" {
		t.Fatalf("got %q", got)
	}
}

func TestRun_LogPhase(t *testing.T) {
	var logs, phases []string
	src := `
export const meta = { name: "t" };
export default async function main() {
  phase("Collect");
  log("got 3 items");
  phase("Analyze");
  return "done";
}`
	_, err := run(t, src, RunOptions{
		OnLog:   func(m string) { logs = append(logs, m) },
		OnPhase: func(p string) { phases = append(phases, p) },
	})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if fmt.Sprint(phases) != "[Collect Analyze]" {
		t.Fatalf("phases = %v", phases)
	}
	if fmt.Sprint(logs) != "[got 3 items]" {
		t.Fatalf("logs = %v", logs)
	}
}

func TestRun_PhaseFlowsToAgentCall(t *testing.T) {
	ex := &mockExec{}
	src := `
export const meta = { name: "t" };
export default async function main() {
  phase("Review");
  await agent("p");
  return "ok";
}`
	_, err := run(t, src, RunOptions{Executor: ex})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if len(ex.calls) != 1 || ex.calls[0].Phase != "Review" {
		t.Fatalf("expected phase Review on agent call, got %+v", ex.calls)
	}
}

func TestRun_Budget(t *testing.T) {
	src := `
export const meta = { name: "t" };
export default async function main() {
  return JSON.stringify({ total: budget.total, spent: budget.spent(), remaining: budget.remaining() });
}`
	got, err := run(t, src, RunOptions{Budget: &Budget{Total: 1000, Spent: func() int { return 300 }}})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	var b struct{ Total, Spent, Remaining int }
	if err := json.Unmarshal([]byte(got), &b); err != nil {
		t.Fatalf("bad json %q: %v", got, err)
	}
	if b.Total != 1000 || b.Spent != 300 || b.Remaining != 700 {
		t.Fatalf("budget = %+v", b)
	}
}

func TestRun_AgentErrorRejects(t *testing.T) {
	ex := &mockExec{reply: func(c AgentCall) (string, error) { return "", fmt.Errorf("boom") }}
	src := `
export const meta = { name: "t" };
export default async function main() {
  try {
    await agent("x");
    return "no-error";
  } catch (e) {
    return "caught";
  }
}`
	got, err := run(t, src, RunOptions{Executor: ex})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if got != "caught" {
		t.Fatalf("got %q, want caught", got)
	}
}

func TestRun_AgentDeferredCatch(t *testing.T) {
	ex := &mockExec{reply: func(c AgentCall) (string, error) { return "", fmt.Errorf("boom") }}
	// 直接在 agent() 返回值上 .catch(不经过 .then),验证 thenable 的 catch 兼容。
	src := `
export const meta = { name: "t" };
export default async function main() {
  return await agent("x").catch(function(e){ return "recovered"; });
}`
	got, err := run(t, src, RunOptions{Executor: ex})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if got != "recovered" {
		t.Fatalf("got %q, want recovered", got)
	}
}

func TestRun_MissingDefaultExport(t *testing.T) {
	src := `export const meta = { name: "t" };`
	_, err := run(t, src, RunOptions{})
	if err == nil {
		t.Fatal("expected error for missing default export")
	}
}

func TestRun_MaxAgentsCap(t *testing.T) {
	// 无限循环刷 agent → 应在撞上限时抛错中止,而不是无限跑。
	src := `
export const meta = { name: "runaway" };
export default async function main() {
  let n = 0;
  for (;;) { await agent("step " + n); n++; }
}`
	calls := 0
	ex := ExecutorFunc(func(ctx context.Context, c AgentCall) (string, error) {
		calls++
		return "ok", nil
	})
	_, err := Run(context.Background(), &Script{Name: "runaway", Source: src}, RunOptions{Executor: ex, MaxAgents: 5})
	if err == nil {
		t.Fatal("预期撞上限报错,实际无错")
	}
	if calls != 5 {
		t.Fatalf("预期恰好跑 5 个 agent(上限),实际 %d", calls)
	}
}

func TestRun_MaxAgentsCap_Parallel(t *testing.T) {
	// parallel 批量超过剩余额度 → 整批拒绝。
	src := `
export const meta = { name: "burst" };
export default async function main() {
  await parallel([0,1,2,3,4,5].map(i => () => agent("p" + i)));
}`
	ex := ExecutorFunc(func(ctx context.Context, c AgentCall) (string, error) { return "ok", nil })
	_, err := Run(context.Background(), &Script{Name: "burst", Source: src}, RunOptions{Executor: ex, MaxAgents: 5})
	if err == nil {
		t.Fatal("预期 6 个并发 agent 超过上限 5 时报错,实际无错")
	}
}

func TestRun_ParallelFailureBecomesNull(t *testing.T) {
	// 三个并发 agent,中间一个失败 → 结果数组对应位置 null,其余正常,整批不 reject。
	src := `
export const meta = { name: "resilient" };
export default async function main() {
  const r = await parallel([
    () => agent("a"),
    () => agent("boom"),
    () => agent("c"),
  ]);
  return JSON.stringify([r[0], r[1], r[2]]);
}`
	ex := ExecutorFunc(func(ctx context.Context, c AgentCall) (string, error) {
		if c.Prompt == "boom" {
			return "", fmt.Errorf("视角失败")
		}
		return "ok:" + c.Prompt, nil
	})
	got, err := Run(context.Background(), &Script{Name: "resilient", Source: src}, RunOptions{Executor: ex})
	if err != nil {
		t.Fatalf("parallel 单项失败不应让整批 reject,却报错: %v", err)
	}
	if got != `["ok:a",null,"ok:c"]` {
		t.Fatalf("失败项应为 null、其余保留,实际 got=%s", got)
	}
}

func TestAgentCall_HasSchema(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{``, false},
		{`null`, false},   // prelude 没传 schema 时发的就是 null
		{` null `, false},
		{`{}`, true},
		{`{"type":"object"}`, true},
	}
	for _, c := range cases {
		got := AgentCall{Schema: []byte(c.raw)}.HasSchema()
		if got != c.want {
			t.Errorf("HasSchema(%q)=%v want %v", c.raw, got, c.want)
		}
	}
}
