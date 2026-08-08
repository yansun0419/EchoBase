package llm

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// newTestHub 构造一个不依赖真实 API client 的 rateHub（不触发 probe 路径）
func newTestHub(t *testing.T) *rateHub {
	h := &rateHub{
		genLimiter:    rate.NewLimiter(rate.Limit(float64(genRatePerMin)/60.0), genRatePerMin),
		embedLimiter:  rate.NewLimiter(rate.Limit(float64(embedRatePerMin)/60.0), embedRatePerMin),
		today:         time.Now().Format("2006-01-02"),
		usageFilePath: filepath.Join(t.TempDir(), "gemini_usage.json"),
	}
	return h
}

// 令牌桶：burst 容量内立即放行，超出后阻塞排队（不突破每分钟上限）
func TestAcquireTokenBucket(t *testing.T) {
	h := newTestHub(t)

	// 文本模型每桶容量 15：前 15 个立即通过
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < genRatePerMin; i++ {
		if err := h.acquire(ctx, kindGenerate); err != nil {
			t.Fatalf("第 %d 个请求应在 burst 内立即放行: %v", i, err)
		}
	}

	// 第 16 个：令牌耗尽，应阻塞排队直到 ctx 超时
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if err := h.acquire(ctx2, kindGenerate); err == nil {
		t.Fatal("超出 burst 的请求不应立即放行")
	} else if err == context.DeadlineExceeded || err != nil {
		// rate.Limiter 超时会返回 "rate: Wait(n=1) would exceed context deadline"
		t.Logf("超出 burst 被阻塞排队（符合预期）: %v", err)
	}
}

// 每日计数：达到上限后触发熔断（dailyBlockedUntil 被设置到次日 0 点）
func TestNoteUsageCircuitBreak(t *testing.T) {
	h := newTestHub(t)

	now := time.Now()
	h.noteUsage(dailyLimit)
	if h.dailyBlockedUntil.IsZero() {
		t.Fatal("每日配额打满后应触发熔断")
	}
	expect := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	if !h.dailyBlockedUntil.Equal(expect) {
		t.Fatalf("熔断应到次日 0 点，得到 %v", h.dailyBlockedUntil)
	}

	// 熔断状态下 acquire 应立即感知（probe 用极短 context 验证不 panic 即逻辑正确）
	h.client = nil // probe 不走到，只验证熔断分支不会无限空转
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.acquire(ctx, kindEmbed); err == nil {
		t.Fatal("熔断期间不应放行请求")
	}
}

// 跨天重置：日期变化后每日计数清零、熔断解除
func TestRollover(t *testing.T) {
	h := newTestHub(t)
	h.today = "2020-01-01" // 伪造昨天
	h.dailyUsed = dailyLimit
	h.dailyBlockedUntil = time.Now().Add(time.Hour)

	h.noteUsage(1)
	if h.dailyUsed != 1 {
		t.Fatalf("跨天后计数应重置为本次用量 1，得到 %d", h.dailyUsed)
	}
	if !h.dailyBlockedUntil.IsZero() {
		t.Fatal("跨天后熔断应解除")
	}
}

// 429 熔断广播：noteThrottle 把冷却时间设置到全局
func TestNoteThrottle(t *testing.T) {
	h := newTestHub(t)
	h.noteThrottle(30 * time.Second)
	if h.cooldownUntil.Before(time.Now().Add(25 * time.Second)) {
		t.Fatalf("冷却截止时间应 >= now+30s，得到 %v", h.cooldownUntil)
	}

	// 更长的冷却会覆盖，更短的不会缩短已有冷却
	before := h.cooldownUntil
	h.noteThrottle(5 * time.Second)
	if !h.cooldownUntil.Equal(before) {
		t.Fatal("更短的冷却不应缩短全局冷却")
	}
	h.noteThrottle(time.Minute)
	if !h.cooldownUntil.After(before) {
		t.Fatal("更长的冷却应扩展全局冷却")
	}
}

// 每日计数持久化：noteUsage 后从文件恢复
func TestUsagePersistence(t *testing.T) {
	h := newTestHub(t)
	h.noteUsage(7)

	h2 := newTestHub(t)
	h2.usageFilePath = h.usageFilePath
	h2.loadUsage()
	if h2.dailyUsed != 7 {
		t.Fatalf("应从文件恢复每日计数 7，得到 %d", h2.dailyUsed)
	}
}
