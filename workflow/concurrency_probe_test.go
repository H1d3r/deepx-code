package workflow

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// sleepExec 每次 agent 调用 sleep d,并记录峰值并发数,用来验证 parallel 真并发。
type sleepExec struct {
	d        time.Duration
	inflight int32
	peak     int32
}

func (e *sleepExec) Agent(ctx context.Context, call AgentCall) (string, error) {
	n := atomic.AddInt32(&e.inflight, 1)
	for {
		p := atomic.LoadInt32(&e.peak)
		if n <= p || atomic.CompareAndSwapInt32(&e.peak, p, n) {
			break
		}
	}
	time.Sleep(e.d)
	atomic.AddInt32(&e.inflight, -1)
	return "done:" + call.Prompt, nil
}

func TestRun_ParallelIsConcurrent(t *testing.T) {
	// 预热:首个 qjs.New() 要 wazero 编译 QuickJS(~数秒),先付掉再计时,免污染下方耗时断言。
	_, _ = Run(context.Background(), &Script{Name: "warm",
		Source: `export const meta={name:"warm"}; export default async function main(){return "ok";}`},
		RunOptions{Executor: &mockExec{}})

	ex := &sleepExec{d: 100 * time.Millisecond}
	src := `
export const meta = { name: "t" };
export default async function main() {
  const r = await parallel([
    () => agent("a"),
    () => agent("b"),
    () => agent("c"),
    () => agent("d"),
  ]);
  return r.join(",");
}`
	start := time.Now()
	got, err := Run(context.Background(), &Script{Name: "t", Source: src}, RunOptions{Executor: ex})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)

	if got != "done:a,done:b,done:c,done:d" {
		t.Fatalf("wrong/ordered result: %q", got)
	}
	// 峰值并发数 = 真并发的硬证据(不受冷启动影响)。
	if peak := atomic.LoadInt32(&ex.peak); peak < 4 {
		t.Fatalf("expected 4 concurrent agents, peak=%d", peak)
	}
	// 4×100ms 顺序 = 400ms;并发应 ~100ms。预热后这条才有意义。
	if elapsed > 250*time.Millisecond {
		t.Fatalf("NOT concurrent: elapsed=%v", elapsed)
	}
}

func TestRun_ParallelRespectsConcurrencyLimit(t *testing.T) {
	ex := &sleepExec{d: 50 * time.Millisecond}
	src := `
export const meta = { name: "t" };
export default async function main() {
  return (await parallel([
    () => agent("a"), () => agent("b"), () => agent("c"),
    () => agent("d"), () => agent("e"), () => agent("f"),
  ])).length;
}`
	got, err := Run(context.Background(), &Script{Name: "t", Source: src}, RunOptions{
		Executor:       ex,
		MaxConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "6" {
		t.Fatalf("expected 6 results, got %q", got)
	}
	if peak := atomic.LoadInt32(&ex.peak); peak > 2 {
		t.Fatalf("concurrency limit breached: peak=%d, want <=2", peak)
	}
}
