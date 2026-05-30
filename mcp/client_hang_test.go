package mcp

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// 把 issue 现场重现:对端 MCP server 死锁不读 stdin,deepx 这边对它发 RPC。
// 老行为:enc.Encode 在 sh stdin pipe buffer 写满后**永久阻塞**,持着 mu,
// 后续所有 call/notify 全堵在 Lock 上 → 整个 transport 卡死。
// 新行为:writerLoop 单独的 goroutine 持有 stdin 写,提交方走 sendPayload 双段超时,
// 卡 > writeTimeout 主动 close transport,后续调用立即报"已关闭"而非继续卡。
func TestStdioTransport_WriteHangIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 sh sleep 模拟不读 stdin 的对端,Windows 跳过")
	}
	orig := writeTimeout
	writeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { writeTimeout = orig })

	// 起一个永远不读 stdin 的"伪 server":stdin pipe 接到我们这边,但它只是 sleep。
	// 我们的写超过 pipe buffer 容量(默认 64KB)后,enc.Encode 就会卡。
	tr, err := newStdioTransport("sh", []string{"-c", "sleep 30"}, nil)
	if err != nil {
		t.Fatalf("newStdioTransport: %v", err)
	}
	t.Cleanup(tr.close)

	// 构造一个肯定塞不进 pipe buffer 的大 payload(>>64KB)→ writerLoop 必然在 enc.Encode 卡住
	huge := strings.Repeat("x", 200_000)

	start := time.Now()
	err = tr.sendPayload(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "huge",
		"params":  map[string]any{"data": huge},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("应该报写入超时,实际 nil(意味着会卡到 server 退出)")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误应明确为超时,got: %v", err)
	}
	// 上界:writeTimeout 一倍的合理余量(测试机抖动)
	if elapsed > 500*time.Millisecond {
		t.Errorf("应在 ~%v 返回,实际用了 %v(说明卡死保护没生效)", writeTimeout, elapsed)
	}

	// 超时后 transport 应该被 close(异步),稍等再发一次 → 立即报"已关闭",而不再卡
	time.Sleep(80 * time.Millisecond)
	start2 := time.Now()
	err2 := tr.sendPayload(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "after-hang"})
	elapsed2 := time.Since(start2)

	if err2 == nil {
		t.Fatal("transport 已 close 后第二次调用应立即报错,实际 nil")
	}
	if !strings.Contains(err2.Error(), "已关闭") {
		t.Errorf("第二次错误应为'已关闭',got: %v", err2)
	}
	if elapsed2 > 100*time.Millisecond {
		t.Errorf("第二次应立即返回(<100ms),实际 %v(说明 close 后还在卡)", elapsed2)
	}
}

// 并发调用场景:多个 goroutine 同时调用同一个卡死的 transport,**必须每个都各自快速返错**,
// 不能因为一个卡住就堵住所有人(这是 issue 用户感知的"一直卡"症状)。
func TestStdioTransport_ConcurrentCallsAllReturnOnHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("用 sh 模拟,Windows 跳过")
	}
	orig := writeTimeout
	writeTimeout = 150 * time.Millisecond
	t.Cleanup(func() { writeTimeout = orig })

	tr, err := newStdioTransport("sh", []string{"-c", "sleep 30"}, nil)
	if err != nil {
		t.Fatalf("newStdioTransport: %v", err)
	}
	t.Cleanup(tr.close)

	const N = 8
	var wg sync.WaitGroup
	errors := make([]error, N)
	huge := strings.Repeat("x", 200_000)

	start := time.Now()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errors[i] = tr.sendPayload(map[string]any{
				"jsonrpc": "2.0",
				"id":      i,
				"method":  "huge",
				"params":  map[string]any{"data": huge},
			})
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// 都得有错(超时 或 已关闭),都不能 nil
	gotErrs := 0
	for i, e := range errors {
		if e == nil {
			t.Errorf("goroutine %d 应该报错,实际 nil", i)
			continue
		}
		gotErrs++
	}
	if gotErrs != N {
		t.Errorf("应该全部 %d 个都返错,实际 %d 个", N, gotErrs)
	}
	// 所有 goroutine 都要在合理时间内退出 —— 关键断言:不能"一直卡"
	if elapsed > 1*time.Second {
		t.Errorf("%d 个并发调用应全部 <1s 返错,实际 %v(老行为是永远卡)", N, elapsed)
	}
}
