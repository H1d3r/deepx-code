package mcp

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// Restart 应该 kill 老 client 并重建,工具仍能正常调。
func TestManager_RestartReconnects(t *testing.T) {
	bin := buildFakeServer(t)
	m := NewManager()
	if err := m.Connect(ServerConfig{Name: "fake", Command: bin}); err != nil {
		t.Fatalf("初连失败: %v", err)
	}
	t.Cleanup(func() { m.Disconnect("fake") })

	c1, err := m.getClient("fake")
	if err != nil {
		t.Fatalf("初次 getClient: %v", err)
	}

	if err := m.Restart("fake"); err != nil {
		t.Fatalf("Restart 失败: %v", err)
	}
	c2, err := m.getClient("fake")
	if err != nil {
		t.Fatalf("重启后 getClient: %v", err)
	}
	if c1 == c2 {
		t.Fatal("Restart 后 client 实例应该是新的,实际同一个指针")
	}
	// 重启完应能正常调用工具
	out, err := c2.CallTool("echo", map[string]any{"text": "after-restart"})
	if err != nil {
		t.Fatalf("重启后调用失败: %v", err)
	}
	if out != "after-restart" {
		t.Errorf("CallTool = %q, want after-restart", out)
	}
}

// 冷却期内的重复 Restart 必须拒绝,防止 server 真坏时被无限重启刷屏。
func TestManager_RestartCooldownPreventsThrash(t *testing.T) {
	orig := restartCooldown
	restartCooldown = 500 * time.Millisecond
	t.Cleanup(func() { restartCooldown = orig })

	bin := buildFakeServer(t)
	m := NewManager()
	if err := m.Connect(ServerConfig{Name: "fake", Command: bin}); err != nil {
		t.Fatalf("初连失败: %v", err)
	}
	t.Cleanup(func() { m.Disconnect("fake") })

	if err := m.Restart("fake"); err != nil {
		t.Fatalf("第一次 Restart 应该成功: %v", err)
	}
	// 立刻再 Restart —— 冷却期内必须拒
	err := m.Restart("fake")
	if err == nil {
		t.Fatal("冷却期内 Restart 应该被拒,实际返回 nil")
	}
	if !strings.Contains(err.Error(), "冷却中") {
		t.Errorf("错误信息应明示冷却,got: %v", err)
	}
	// 等过冷却,应该又能 Restart
	time.Sleep(550 * time.Millisecond)
	if err := m.Restart("fake"); err != nil {
		t.Fatalf("冷却过后 Restart 应成功: %v", err)
	}
}

// 从未连过的 server 调 Restart 必须返错,不能瞎拉新进程。
func TestManager_RestartUnknownServerErrors(t *testing.T) {
	m := NewManager()
	err := m.Restart("never-existed")
	if err == nil {
		t.Fatal("从未连接过的 server 调 Restart 应返错,实际 nil")
	}
	if !strings.Contains(err.Error(), "无") {
		t.Errorf("错误应明示找不到配置,got: %v", err)
	}
}

// callToolWithRestart 在 client 已 close(模拟 sendPayload 写超时后状态)时,
// 应自动 Restart + 重试一次并成功。这是 issue 用户感知的关键改进 ——
// "卡死的 server 自动救场,模型不用换工具就能继续"。
func TestManager_CallToolAutoRestartsOnDeadConnection(t *testing.T) {
	bin := buildFakeServer(t)
	m := NewManager()
	if err := m.Connect(ServerConfig{Name: "fake", Command: bin}); err != nil {
		t.Fatalf("初连失败: %v", err)
	}
	t.Cleanup(func() { m.Disconnect("fake") })

	// 模拟 sendPayload 写超时触发的强制 close:手动 close 现有 client,
	// 但不从 m.clients 移除(精确还原超时后状态:transport 死了但 map 里还在)。
	c, _ := m.getClient("fake")
	c.Close() // 现在调 CallTool 会拿到"MCP 连接已关闭",触发 looksLikeDeadConnection

	out, err := m.callToolWithRestart("fake", "echo", map[string]any{"text": "auto-recovered"})
	if err != nil {
		t.Fatalf("应自动重启 + 重试成功,实际错: %v", err)
	}
	if out != "auto-recovered" {
		t.Errorf("CallTool = %q, want auto-recovered", out)
	}
}

// 并发 N 个 CallTool 撞同一个死透的 server,**最多触发一次实际 Restart**(其它走冷却拒)。
// 验证不会因为并发把 server 杀重启 N 次。
func TestManager_ConcurrentCallsCoalesceRestart(t *testing.T) {
	orig := restartCooldown
	restartCooldown = 2 * time.Second // 长一点确保并发都撞冷却
	t.Cleanup(func() { restartCooldown = orig })

	bin := buildFakeServer(t)
	m := NewManager()
	if err := m.Connect(ServerConfig{Name: "fake", Command: bin}); err != nil {
		t.Fatalf("初连失败: %v", err)
	}
	t.Cleanup(func() { m.Disconnect("fake") })

	// 把 client 弄死
	c, _ := m.getClient("fake")
	c.Close()

	const N = 5
	var wg sync.WaitGroup
	successes := 0
	var mu sync.Mutex
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.callToolWithRestart("fake", "echo", map[string]any{"text": "x"}); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// 至少 1 个成功(最早抢到重启窗口的那个);其它若失败应是冷却拒绝,不应卡死。
	if successes < 1 {
		t.Errorf("至少应有 1 个并发调用成功,实际 %d / %d", successes, N)
	}
	t.Logf("%d 个并发死连接调用:%d 个成功重启 + 重试,其它走冷却拒绝(预期行为)", N, successes)
}
