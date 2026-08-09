package llm

import (
	"context"
	"testing"
	"time"

	"echobase/internal/config"
)

// newTestHub 构造一个不依赖真实 API client 的 rateHub（不触发 probe 路径）
func newTestHub(t *testing.T) *rateHub {
	return newTestHubWithStore(t, &memUsageStore{data: make(map[string]int)})
}

func newTestHubWithStore(t *testing.T, store usageStore) *rateHub {
	h := &rateHub{
		genLimiter:   newPrioLimiter(float64(config.TextModelRPM)),
		embedLimiter: newPrioLimiter(float64(config.EmbedModelRPM)),
		genSoft:      newSoftLimiter(kindGenerate),
		embedSoft:    newSoftLimiter(kindEmbed),
		today:        time.Now().Format("2006-01-02"),
		store:        store,
	}
	h.dailyHigh = map[requestKind]int{kindGenerate: 0, kindEmbed: 0}
	h.dailyLow = map[requestKind]int{kindGenerate: 0, kindEmbed: 0}
	h.highBlocked = map[requestKind]time.Time{}
	h.lowBlocked = map[requestKind]time.Time{}
	return h
}

// 物理令牌桶：burst 容量内立即放行，超出后阻塞排队（不突破每分钟上限）
func TestAcquireTokenBucket(t *testing.T) {
	h := newTestHub(t)

	// 文本模型每桶容量 15：前 15 个立即通过
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < config.TextModelRPM; i++ {
		if err := h.acquire(ctx, kindGenerate, prioHigh); err != nil {
			t.Fatalf("第 %d 个请求应在 burst 内立即放行: %v", i, err)
		}
	}

	// 第 16 个：令牌耗尽，应阻塞排队直到 ctx 超时
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if err := h.acquire(ctx2, kindGenerate, prioHigh); err == nil {
		t.Fatal("超出 burst 的请求不应立即放行")
	} else if err == context.DeadlineExceeded || err != nil {
		t.Logf("超出 burst 被阻塞排队（符合预期）: %v", err)
	}
}

// 每日物理总量：达到上限后触发熔断（highBlocked 被设置到次日 0 点）
func TestNoteUsageCircuitBreak(t *testing.T) {
	h := newTestHub(t)

	now := time.Now()
	h.noteUsage(kindEmbed, prioHigh, config.EmbedModelRPD)
	until, ok := h.highBlocked[kindEmbed]
	if !ok {
		t.Fatal("每日物理配额打满后应触发熔断")
	}
	expect := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	if !until.Equal(expect) {
		t.Fatalf("熔断应到次日 0 点，得到 %v", until)
	}

	// 另一个种类的配额不受影响（相互独立）
	if h.highBlocked[kindGenerate] != (time.Time{}) {
		t.Fatal("向量熔断不应影响文本配额的熔断状态")
	}

	// 熔断状态下 acquire 应立即感知（probe 用极短 context 验证不 panic 即逻辑正确）
	h.client = nil // probe 不走到，只验证熔断分支不会无限空转
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.acquire(ctx, kindEmbed, prioHigh); err == nil {
		t.Fatal("熔断期间不应放行请求")
	}
}

// 跨天重置：日期变化后每日计数清零、熔断解除
func TestRollover(t *testing.T) {
	h := newTestHub(t)
	h.today = "2020-01-01" // 伪造昨天
	h.dailyHigh[kindGenerate] = config.TextModelRPD
	h.dailyLow[kindGenerate] = lowDailyLimitFor(kindGenerate)
	h.highBlocked[kindGenerate] = time.Now().Add(time.Hour)
	h.lowBlocked[kindGenerate] = time.Now().Add(time.Hour)

	h.noteUsage(kindGenerate, prioHigh, 1)
	if h.dailyHigh[kindGenerate] != 1 {
		t.Fatalf("跨天后高优计数应重置为本次用量 1，得到 %d", h.dailyHigh[kindGenerate])
	}
	if h.dailyLow[kindGenerate] != 0 {
		t.Fatalf("跨天后低优计数应清零，得到 %d", h.dailyLow[kindGenerate])
	}
	if h.highBlocked[kindGenerate] != (time.Time{}) {
		t.Fatal("跨天后物理熔断应解除")
	}
	if h.lowBlocked[kindGenerate] != (time.Time{}) {
		t.Fatal("跨天后低优熔断应解除")
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
	h.noteUsage(kindGenerate, prioHigh, 7)
	h.noteUsage(kindEmbed, prioLow, 3)
	h.flushIfDirty()
	if h.dirty {
		t.Fatal("flush 成功后 dirty 标记应清除")
	}

	// 新实例从同一存储恢复计数（模拟容器重建后从 DB 读回）
	h2 := newTestHubWithStore(t, store)
	h2.loadUsageFromStore()
	if h2.dailyHigh[kindGenerate] != 7 {
		t.Fatalf("应从存储恢复文本高优计数 7，得到 %d", h2.dailyHigh[kindGenerate])
	}
	if h2.dailyLow[kindEmbed] != 3 {
		t.Fatalf("应从存储恢复向量低优计数 3，得到 %d", h2.dailyLow[kindEmbed])
	}
	if h2.totalUsed(kindGenerate) != 7 {
		t.Fatalf("文本物理总账应恢复为 7，得到 %d", h2.totalUsed(kindGenerate))
	}
}

// 异步批量刷盘：noteUsage 只累加内存并标记 dirty，不触发同步写
func TestNoteUsageNoSyncWrite(t *testing.T) {
	store := &memUsageStore{data: make(map[string]int)}
	h := newTestHubWithStore(t, store)
	h.noteUsage(kindGenerate, prioLow, 3)
	if !h.dirty {
		t.Fatal("noteUsage 后应标记 dirty 等待批量刷盘")
	}
	if store.data[h.today+"|generate|low"] != 0 {
		t.Fatalf("noteUsage 不应同步写存储，得到 %d", store.data[h.today+"|generate|low"])
	}
}

// flush 失败保留 dirty 标记，下个周期重试
func TestFlushRetryOnError(t *testing.T) {
	failing := &failStore{}
	h := newTestHubWithStore(t, failing)
	h.noteUsage(kindGenerate, prioLow, 5)
	h.flushIfDirty()
	if !h.dirty {
		t.Fatal("flush 失败后应保留 dirty 标记以便重试")
	}
}

// 文本/向量每日配额相互独立；低优软上限 = 总量 − 在线预留
func TestPerKindDailyLimit(t *testing.T) {
	if dailyLimitFor(kindGenerate) != config.TextModelRPD {
		t.Fatalf("文本每日配额应为 %d", config.TextModelRPD)
	}
	if dailyLimitFor(kindEmbed) != config.EmbedModelRPD {
		t.Fatalf("向量每日配额应为 %d", config.EmbedModelRPD)
	}
	if lowDailyLimitFor(kindGenerate) != config.TextModelRPD-config.TextModelRPDReserved {
		t.Fatalf("文本低优软上限应为 %d", config.TextModelRPD-config.TextModelRPDReserved)
	}
	if lowDailyLimitFor(kindEmbed) != config.EmbedModelRPD-config.EmbedModelRPDReserved {
		t.Fatalf("向量低优软上限应为 %d", config.EmbedModelRPD-config.EmbedModelRPDReserved)
	}
}

// 每分钟预留：低优先级被软桶限制在（总量 − 预留），
// 软桶耗尽后高优先级仍能立即用掉物理桶剩余的预留余量
func TestLowPrioritySoftCap(t *testing.T) {
	h := newTestHub(t)

	lowCap := config.TextModelRPM - config.TextModelRPMReserved
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < lowCap; i++ {
		if err := h.acquire(ctx, kindGenerate, prioLow); err != nil {
			t.Fatalf("低优第 %d 个应立即通过软桶: %v", i, err)
		}
	}

	// 低优超出软桶：应阻塞（不占用物理桶留给高优的余量）
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if err := h.acquire(ctx2, kindGenerate, prioLow); err == nil {
		t.Fatal("低优超出软桶 burst 不应立即放行")
	}

	// 高优此时应立即放行（物理桶剩余 = 预留 5）
	ctx3, cancel3 := context.WithTimeout(context.Background(), time.Second)
	defer cancel3()
	if err := h.acquire(ctx3, kindGenerate, prioHigh); err != nil {
		t.Fatalf("低优耗尽软桶后高优应立即放行（预留余量）: %v", err)
	}
}

// 每日低优软熔断：低优账本打满软上限只熔断低优，高优不受影响（可借用剩余额度）
func TestLowDailyCircuitBreak(t *testing.T) {
	h := newTestHub(t)

	// 低优打满软账本（不触发物理熔断，因为没到 RPD）
	h.noteUsage(kindEmbed, prioLow, lowDailyLimitFor(kindEmbed))
	if h.lowBlocked[kindEmbed] == (time.Time{}) {
		t.Fatal("低优软账本打满后应熔断低优")
	}
	if h.highBlocked[kindEmbed] != (time.Time{}) {
		t.Fatal("低优熔断不应触发高优（物理）熔断")
	}

	// 高优 acquire 仍应立即放行
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.acquire(ctx, kindEmbed, prioHigh); err != nil {
		t.Fatalf("低优熔断不应阻塞高优请求: %v", err)
	}

	// 低优 acquire 应被软熔断拦住（立即熔断到午夜，不探测）
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if err := h.acquire(ctx2, kindEmbed, prioLow); err == nil {
		t.Fatal("低优软熔断期间不应放行低优请求")
	}
}

// 优先级队列：令牌发放高优绝对优先，低优只在高优无等待时才捡漏
func TestPrioLimiterHighFirst(t *testing.T) {
	// burst=1、rate=1/60：令牌耗尽后 refill 极慢，可在测试窗口内手动注入令牌
	lim := newPrioLimiter(1)
	if err := lim.acquire(context.Background(), prioLow); err != nil {
		t.Fatalf("初始令牌应立即发放: %v", err)
	}

	// 队列中先排低优、后排高优（到达顺序模拟后台先占坑、用户后来）
	gotLow := make(chan struct{})
	go func() {
		lim.acquire(context.Background(), prioLow)
		close(gotLow)
	}()
	gotHigh := make(chan struct{})
	go func() {
		lim.acquire(context.Background(), prioHigh)
		close(gotHigh)
	}()

	// 等两个请求都进入等待队列
	deadline := time.Now().Add(2 * time.Second)
	for {
		lim.mu.Lock()
		queued := len(lim.highQ) > 0 && len(lim.lowQ) > 0
		lim.mu.Unlock()
		if queued || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	lim.mu.Lock()
	queued := len(lim.highQ) > 0 && len(lim.lowQ) > 0
	lim.mu.Unlock()
	if !queued {
		t.Fatal("两个请求应都进入等待队列")
	}

	// 手动注入 1 个令牌并发放：必须发给高优
	lim.mu.Lock()
	lim.tokens = 1
	lim.grant()
	lim.mu.Unlock()

	select {
	case <-gotHigh:
	case <-time.After(time.Second):
		t.Fatal("高优先级应先拿到令牌（插队）")
	}
	select {
	case <-gotLow:
		t.Fatal("低优先级不应在高优先级之前拿到令牌")
	default:
	}
}

type failStore struct{}

func (f *failStore) Load(_ context.Context, _, _, _ string) (int, error) { return 0, nil }
func (f *failStore) Save(_ context.Context, _, _, _ string, _ int) error {
	return context.DeadlineExceeded
}
