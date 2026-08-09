package llm

import (
	"context"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"echobase/internal/config"
)

// newTestHub 构造一个不依赖真实 API client 的 rateHub（不触发 probe 路径）
func newTestHub(t *testing.T) *rateHub {
	return newTestHubWithStore(t, &memUsageStore{data: make(map[string]int)})
}

func newTestHubWithStore(t *testing.T, store usageStore) *rateHub {
	h := &rateHub{
		genLimiter:   rate.NewLimiter(rate.Limit(float64(config.TextModelRPM)/60.0), config.TextModelRPM),
		embedLimiter: rate.NewLimiter(rate.Limit(float64(config.EmbedModelRPM)/60.0), config.EmbedModelRPM),
		today:        time.Now().Format("2006-01-02"),
		store:        store,
	}
	h.dailyUsed = map[requestKind]int{kindGenerate: 0, kindEmbed: 0}
	h.dailyBlocked = map[requestKind]time.Time{}
	return h
}

// 令牌桶：burst 容量内立即放行，超出后阻塞排队（不突破每分钟上限）
func TestAcquireTokenBucket(t *testing.T) {
	h := newTestHub(t)

	// 文本模型每桶容量 15：前 15 个立即通过
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < config.TextModelRPM; i++ {
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

// 每日计数：达到上限后触发熔断（dailyBlocked 被设置到次日 0 点）
func TestNoteUsageCircuitBreak(t *testing.T) {
	h := newTestHub(t)

	now := time.Now()
	h.noteUsage(kindEmbed, config.EmbedModelRPD)
	until, ok := h.dailyBlocked[kindEmbed]
	if !ok {
		t.Fatal("每日配额打满后应触发熔断")
	}
	expect := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	if !until.Equal(expect) {
		t.Fatalf("熔断应到次日 0 点，得到 %v", until)
	}

	// 另一个种类的配额不受影响（相互独立）
	if h.dailyBlocked[kindGenerate] != (time.Time{}) {
		t.Fatal("向量熔断不应影响文本配额的熔断状态")
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
	h.dailyUsed[kindGenerate] = config.TextModelRPD
	h.dailyBlocked[kindGenerate] = time.Now().Add(time.Hour)

	h.noteUsage(kindGenerate, 1)
	if h.dailyUsed[kindGenerate] != 1 {
		t.Fatalf("跨天后计数应重置为本次用量 1，得到 %d", h.dailyUsed[kindGenerate])
	}
	if h.dailyBlocked[kindGenerate] != (time.Time{}) {
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

// 每日计数持久化：noteUsage 后手动 flush，再从存储恢复（模拟进程重启）
func TestUsagePersistence(t *testing.T) {
	store := &memUsageStore{data: make(map[string]int)}
	h := newTestHubWithStore(t, store)
	h.noteUsage(kindGenerate, 7)
	h.noteUsage(kindEmbed, 3)
	h.flushIfDirty()
	if h.dirty {
		t.Fatal("flush 成功后 dirty 标记应清除")
	}

	// 新实例从同一存储恢复计数（模拟容器重建后从 DB 读回）
	h2 := newTestHubWithStore(t, store)
	h2.loadUsageFromStore()
	if h2.dailyUsed[kindGenerate] != 7 {
		t.Fatalf("应从存储恢复文本计数 7，得到 %d", h2.dailyUsed[kindGenerate])
	}
	if h2.dailyUsed[kindEmbed] != 3 {
		t.Fatalf("应从存储恢复向量计数 3，得到 %d", h2.dailyUsed[kindEmbed])
	}
}

// 异步批量刷盘：noteUsage 只累加内存并标记 dirty，不触发同步写
func TestNoteUsageNoSyncWrite(t *testing.T) {
	store := &memUsageStore{data: make(map[string]int)}
	h := newTestHubWithStore(t, store)
	h.noteUsage(kindGenerate, 3)
	if !h.dirty {
		t.Fatal("noteUsage 后应标记 dirty 等待批量刷盘")
	}
	if store.data[h.today+"|generate"] != 0 {
		t.Fatalf("noteUsage 不应同步写存储，得到 %d", store.data[h.today+"|generate"])
	}
}

// flush 失败保留 dirty 标记，下个周期重试
func TestFlushRetryOnError(t *testing.T) {
	failing := &failStore{}
	h := newTestHubWithStore(t, failing)
	h.noteUsage(kindGenerate, 5)
	h.flushIfDirty()
	if !h.dirty {
		t.Fatal("flush 失败后应保留 dirty 标记以便重试")
	}
}

// 文本/向量每日配额相互独立
func TestPerKindDailyLimit(t *testing.T) {
	if dailyLimitFor(kindGenerate) != config.TextModelRPD {
		t.Fatalf("文本每日配额应为 %d", config.TextModelRPD)
	}
	if dailyLimitFor(kindEmbed) != config.EmbedModelRPD {
		t.Fatalf("向量每日配额应为 %d", config.EmbedModelRPD)
	}
}

type failStore struct{}

func (f *failStore) Load(_ context.Context, _, _ string) (int, error) { return 0, nil }
func (f *failStore) Save(_ context.Context, _, _ string, _ int) error {
	return context.DeadlineExceeded
}
