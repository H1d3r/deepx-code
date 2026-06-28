package agent

import (
	"context"
	"testing"
	"time"
)

func TestIsRetryableStatus(t *testing.T) {
	retry := []int{408, 409, 425, 429, 500, 502, 503, 504}
	noRetry := []int{200, 201, 400, 401, 403, 404, 422}
	for _, c := range retry {
		if !isRetryableStatus(c) {
			t.Errorf("HTTP %d 应可重试", c)
		}
	}
	for _, c := range noRetry {
		if isRetryableStatus(c) {
			t.Errorf("HTTP %d 不应重试", c)
		}
	}
}

func TestRetryBackoff(t *testing.T) {
	// Retry-After(秒)优先且精确(无抖动)
	if got := retryBackoff(0, "5"); got != 5*time.Second {
		t.Errorf("Retry-After=5 应为 5s,got %v", got)
	}
	// Retry-After 超大值夹到 60s
	if got := retryBackoff(0, "9999"); got != 60*time.Second {
		t.Errorf("Retry-After 超大应夹到 60s,got %v", got)
	}
	// 坏 Retry-After → 退回指数退避(attempt 0:1s 基数 + 0~20% 抖动)
	if got := retryBackoff(0, "abc"); got < time.Second || got > 1200*time.Millisecond {
		t.Errorf("坏 Retry-After 应退回 ~1s,got %v", got)
	}
	// 指数增长:attempt 3 基数 8s
	if got := retryBackoff(3, ""); got < 8*time.Second || got > 96*time.Second/10 {
		t.Errorf("attempt=3 应 ~8s(含抖动),got %v", got)
	}
	// 封顶 30s(+ 抖动 ≤ 36s)
	if got := retryBackoff(20, ""); got < 30*time.Second || got > 36*time.Second {
		t.Errorf("attempt=20 应封顶 ~30s,got %v", got)
	}
}

func TestSleepCtx(t *testing.T) {
	// 正常等待:短延时返回 true
	if !sleepCtx(context.Background(), 10*time.Millisecond) {
		t.Errorf("正常等待应返回 true")
	}
	// 已取消的 ctx:立刻返回 false,不空等
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepCtx(ctx, 10*time.Second) {
		t.Errorf("已取消的 ctx 应返回 false")
	}
	if time.Since(start) > time.Second {
		t.Errorf("取消应立刻返回,不该等满 10s")
	}
}
