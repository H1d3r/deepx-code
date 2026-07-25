package agent

import "testing"

// 动态阈值:主流窗口(128K~256K)保持 ~70%,大窗口触发线升高(少丢上下文),
// 极小窗口触发线降低(单轮暴涨占比大,该早压)。且必须单调:窗口越大,触发的绝对 token 越大。
func TestCompactTriggerTokens(t *testing.T) {
	cases := []struct {
		ctxWin  int
		wantPct int // 期望触发线约占窗口的百分比(±1)
	}{
		{40_000, 40},    // 极小:headroom 30K 撞 60% 上限(→24K) → 40% 触发
		{64_000, 53},    // headroom 撞 30K 下限 → 53%
		{128_000, 70},   // 主流下限:headroom=38.4K → 70%
		{256_000, 70},   // step-flash:headroom=76.8K → 70%
		{600_000, 70},   // headroom=180K 正好=30% → 70%
		{1_048_576, 82}, // 1M:headroom 封顶 180K → ~82%
	}
	for _, c := range cases {
		got := CompactTriggerTokens(c.ctxWin)
		pct := got * 100 / c.ctxWin
		if pct < c.wantPct-1 || pct > c.wantPct+1 {
			t.Errorf("CompactTriggerTokens(%d) = %d (%d%%), 期望 ~%d%%", c.ctxWin, got, pct, c.wantPct)
		}
	}

	// 单调:窗口越大,触发绝对阈值越大(不会出现大窗口反而更早压)。
	prev := 0
	for _, w := range []int{64_000, 128_000, 256_000, 512_000, 1_048_576} {
		got := CompactTriggerTokens(w)
		if got <= prev {
			t.Errorf("非单调:窗口 %d 触发 %d 不大于更小窗口的 %d", w, got, prev)
		}
		prev = got
	}

	// headroom 恒为正(触发线永远 < 窗口),否则等于不压。
	for _, w := range []int{1000, 20_000, 256_000, 2_000_000} {
		if got := CompactTriggerTokens(w); got >= w || got <= 0 {
			t.Errorf("CompactTriggerTokens(%d) = %d,应在 (0, %d) 内", w, got, w)
		}
	}

	if CompactTriggerTokens(0) != 0 || CompactTriggerTokens(-1) != 0 {
		t.Error("ctxWin<=0 应返回 0")
	}
}
