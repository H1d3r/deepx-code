package agent

import (
	"fmt"
	"strings"
	"testing"
)

// TestWriteContentLimitBytes_ScalesWithWindow 上限随窗口自适应,并被上下限钳住。
func TestWriteContentLimitBytes_ScalesWithWindow(t *testing.T) {
	cases := []struct{ ctxWin, wantMin, wantMax int }{
		{0, writeLimitDefault, writeLimitDefault},  // 窗口未知 → 保守默认
		{-1, writeLimitDefault, writeLimitDefault}, // 非法值同上
		{8192, writeLimitFloor, writeLimitFloor},   // 极小窗口 → 触底
		{32768, 9 * 1024, 11 * 1024},               // 32K → 约 9.6KB
		{65536, 19 * 1024, 21 * 1024},              // 64K → 约 19.2KB
		{131072, 38 * 1024, 40 * 1024},             // 128K → 约 38.4KB
		{1 << 20, writeLimitCap, writeLimitCap},    // 1M → 触顶
	}
	for _, c := range cases {
		got := WriteContentLimitBytes(c.ctxWin)
		if got < c.wantMin || got > c.wantMax {
			t.Errorf("窗口 %d: 上限 %d 不在期望区间 [%d, %d]", c.ctxWin, got, c.wantMin, c.wantMax)
		}
	}
}

// TestWriteContentLimitBytes_FitsInKeepBudget 核心不变量:一次写满上限的内容,
// token 数不超过压缩保留预算 —— 否则它落进尾部保护区时压缩就压不动了。
func TestWriteContentLimitBytes_FitsInKeepBudget(t *testing.T) {
	for _, ctxWin := range []int{32768, 65536, 131072, 262144} {
		limit := WriteContentLimitBytes(ctxWin)
		budget := CompactKeepTokens(ctxWin)

		// 中文是最坏情况(字节/token 比最低),用它做上界检验
		zh := strings.Repeat("中", limit/3)
		ascii := strings.Repeat("x", limit)

		tzh, tascii := EstTokens(zh), EstTokens(ascii)
		fmt.Printf("  窗口 %7d: 上限 %6d B  保留预算 %6d tok  |  写满(中文) %6d tok  写满(ASCII) %6d tok\n",
			ctxWin, limit, budget, tzh, tascii)

		if tzh > budget {
			t.Errorf("窗口 %d: 写满上限的中文内容 %d token 超过保留预算 %d —— 压缩将压不动它",
				ctxWin, tzh, budget)
		}
	}
}

// TestWriteContentLimitBytes_Monotonic 窗口越大上限不应变小。
func TestWriteContentLimitBytes_Monotonic(t *testing.T) {
	prev := 0
	for _, w := range []int{8192, 16384, 32768, 65536, 131072, 262144, 1 << 20} {
		got := WriteContentLimitBytes(w)
		if got < prev {
			t.Fatalf("窗口 %d 的上限 %d 小于更小窗口的 %d", w, got, prev)
		}
		prev = got
	}
}

// TestWriteContentLimitFor_UsesSmallerWindow 上限是安全阀,得按 flash / pro 里
// **较小**的窗口定 —— 同一会话两个模型会来回切,写入落在哪一轮事先不知道。
func TestWriteContentLimitFor_UsesSmallerWindow(t *testing.T) {
	small, big := 32768, 262144
	cfg := ModelConfig{
		Flash: ModelEntry{ContextWindow: small},
		Pro:   ModelEntry{ContextWindow: big},
	}
	got := WriteContentLimitFor(cfg)
	if want := WriteContentLimitBytes(small); got != want {
		t.Errorf("应按较小窗口(%d)算 = %d, got %d(按大的算会是 %d)",
			small, want, got, WriteContentLimitBytes(big))
	}

	// 只配了一个:用配了的那个,别被 0 拉低
	if got, want := WriteContentLimitFor(ModelConfig{Pro: ModelEntry{ContextWindow: big}}),
		WriteContentLimitBytes(big); got != want {
		t.Errorf("只配 pro 时应按 pro 算 = %d, got %d", want, got)
	}
	if got, want := WriteContentLimitFor(ModelConfig{Flash: ModelEntry{ContextWindow: small}}),
		WriteContentLimitBytes(small); got != want {
		t.Errorf("只配 flash 时应按 flash 算 = %d, got %d", want, got)
	}
	// 都没配 → 保守默认
	if got := WriteContentLimitFor(ModelConfig{}); got != writeLimitDefault {
		t.Errorf("都没配窗口时应回落 %d, got %d", writeLimitDefault, got)
	}
}
