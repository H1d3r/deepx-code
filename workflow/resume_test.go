package workflow

import (
	"context"
	"sync"
	"testing"
)

// memStore 是测试用的内存 ResultStore(并发安全)。
type memStore struct {
	mu sync.Mutex
	m  map[int]string
}

func newMemStore() *memStore { return &memStore{m: map[int]string{}} }
func (s *memStore) Get(seq int) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[seq]
	return r, ok
}
func (s *memStore) Put(seq int, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[seq] = result
}

func TestResume_SkipsCachedAgents(t *testing.T) {
	src := `
export const meta = { name: "t" };
export default async function main() {
  const r = await parallel([ () => agent("a"), () => agent("b"), () => agent("c") ]);
  const s = await agent("synth:" + r.join(","));
  return s;
}`
	store := newMemStore()

	// 第一次:模拟跑到一半中断——只让前 2 个 agent "成功"(被缓存),其余报错。
	var calls1 int
	ex1 := ExecutorFunc(func(ctx context.Context, c AgentCall) (string, error) {
		calls1++
		if calls1 <= 2 {
			return "R:" + c.Prompt, nil
		}
		return "", context.Canceled // 模拟中断
	})
	_, err := Run(context.Background(), &Script{Name: "t", Source: src}, RunOptions{Executor: ex1, Results: store})
	if err == nil {
		t.Fatal("第一次应因中断报错")
	}
	if len(store.m) != 2 {
		t.Fatalf("应缓存 2 个成功结果,实际 %d", len(store.m))
	}

	// 第二次 resume:已完成的 seq 命中缓存不再跑,只跑剩下的。
	var ran2 []string
	var mu sync.Mutex
	ex2 := ExecutorFunc(func(ctx context.Context, c AgentCall) (string, error) {
		mu.Lock()
		ran2 = append(ran2, c.Prompt)
		mu.Unlock()
		return "R2:" + c.Prompt, nil
	})
	got, err := Run(context.Background(), &Script{Name: "t", Source: src}, RunOptions{Executor: ex2, Results: store})
	if err != nil {
		t.Fatalf("resume 失败: %v", err)
	}
	// 第一次缓存了 seq0,1(a,b);第二次只该跑 seq2(c)+ seq3(synth)= 2 次。
	if len(ran2) != 2 {
		t.Fatalf("resume 应只真跑 2 个(c + synth),实际 %d: %v", len(ran2), ran2)
	}
	// 结果里 a/b 用的是第一次的缓存值 R:a / R:b。
	if got == "" || got[:3] != "R2:" {
		t.Fatalf("最终结果应来自第二次的 synth: %q", got)
	}
}
