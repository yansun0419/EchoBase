package llm

import (
	"context"
	"errors"
	"log"
	"math"
	"sync"
	"time"

	"github.com/google/generative-ai-go/genai"
	"golang.org/x/time/rate"
	"google.golang.org/api/googleapi"
	"gorm.io/gorm"

	"echobase/internal/config"
)

// ---------------------------------------------------------------------------
// 全局请求网关（Request Hub）
//
// 免费档配额（共享同一个 API Key，所有 worker/task 共同消耗）：
//   - 文本生成（gemini-3.1-flash-lite）：config.TextModelRPM req/min，RPD req/day
//   - 向量化（gemini-embedding-2）：config.EmbedModelRPM req/min，RPD req/day
//
// 设计：令牌桶 + 共享客户端。所有公开函数发请求前统一 acquire 令牌，
// 未超配额保持并发，超配额自动排队匀速发放，绝不突破每分钟上限。
//
// QoS（服务质量）资源隔离：请求分两种优先级
//   - prioHigh（在线）：用户提问 / 检索，一秒钟都不能被后台饿死
//   - prioLow（后台）：数据清洗、切片、向量化、合并，晚一点无所谓
//
// 每分钟：物理令牌桶发放时永远先服务高优先级队列，低优先级只能捡漏；
// 且低优先级还需额外通过自己的软桶（总量 − 预留），从数学上保证每分钟
// 至少给高优先级留下 config.*RPMReserved 的余量，高优到达无需等待。
//
// 每日：高低优先级共享一个物理总账（RPD 上限，高优可借用低优未用的额度），
// 低优先级另有独立软账本（上限 = RPD − 预留），到点立即熔断到次日 0 点，
// 当日剩余额度全部让给高优。物理总账耗尽则高低一起熔断，按 ProbeInterval
// 定时探测恢复。
//
// 每日计数：内存中累加，每 FlushInterval 定时批量写入数据库（而非每次请求
// 同步落盘），避免高频同步 I/O 阻塞；计数持久化到数据库而非本地文件，
// 保证云平台容器重建后限流计数不丢失。
// 本层完全通用，不含任何具体模型名或业务逻辑，换模型后端无需改动。
// ---------------------------------------------------------------------------

// kindKey 返回请求种类的持久化标识（api_quotas.kind 列）
func kindKey(kind requestKind) string {
	if kind == kindGenerate {
		return "generate"
	}
	return "embed"
}

// priorityKey 返回请求优先级的持久化标识（api_quotas.priority 列）
func priorityKey(p priority) string {
	if p == prioHigh {
		return "high"
	}
	return "low"
}

// dailyLimitFor 返回该请求种类的每日物理配额上限（高低优先级共享）
func dailyLimitFor(kind requestKind) int {
	if kind == kindGenerate {
		return config.TextModelRPD
	}
	return config.EmbedModelRPD
}

// lowDailyLimitFor 低优先级每日软上限 = 物理总量 − 在线预留（程序自动计算）
func lowDailyLimitFor(kind requestKind) int {
	if kind == kindGenerate {
		return config.TextModelRPD - config.TextModelRPDReserved
	}
	return config.EmbedModelRPD - config.EmbedModelRPDReserved
}

// lowRatePerSec 低优先级软桶速率（每秒令牌数）= (每分钟总量 − 预留) / 60
func lowRatePerSec(kind requestKind) float64 {
	if kind == kindGenerate {
		return float64(config.TextModelRPM-config.TextModelRPMReserved) / 60.0
	}
	return float64(config.EmbedModelRPM-config.EmbedModelRPMReserved) / 60.0
}

// lowBurst 低优先级软桶突发量 = 每分钟总量 − 预留
func lowBurst(kind requestKind) float64 {
	if kind == kindGenerate {
		return float64(config.TextModelRPM - config.TextModelRPMReserved)
	}
	return float64(config.EmbedModelRPM - config.EmbedModelRPMReserved)
}

// newSoftLimiter 构造低优先级软桶（速率与突发量 = 总量 − 在线预留）
func newSoftLimiter(kind requestKind) *rate.Limiter {
	return rate.NewLimiter(rate.Limit(lowRatePerSec(kind)), int(lowBurst(kind)))
}

// usageStore 每日配额持久化后端。
// 生产环境用 pgUsageStore（PostgreSQL），测试可用内存实现，换后端零侵入。
type usageStore interface {
	Load(ctx context.Context, date, kind, priority string) (int, error)
	Save(ctx context.Context, date, kind, priority string, count int) error
}

// pgUsageStore 基于 PostgreSQL 的每日配额存储
//（api_quotas 表，(date, kind, priority) 三维联合主键）
type pgUsageStore struct {
	db *gorm.DB
}

func (s *pgUsageStore) Load(ctx context.Context, date, kind, priority string) (int, error) {
	var rec struct{ UsageCount int }
	err := s.db.WithContext(ctx).
		Table("api_quotas").
		Select("usage_count").
		Where("date = ? AND kind = ? AND priority = ?", date, kind, priority).
		Scan(&rec).Error
	if err != nil {
		return 0, err
	}
	return rec.UsageCount, nil
}

func (s *pgUsageStore) Save(ctx context.Context, date, kind, priority string, count int) error {
	// upsert：当天该维度记录存在则覆盖，不存在则插入
	return s.db.WithContext(ctx).Exec(`
		INSERT INTO api_quotas (date, kind, priority, usage_count) VALUES (?, ?, ?, ?)
		ON CONFLICT (date, kind, priority) DO UPDATE SET usage_count = EXCLUDED.usage_count
	`, date, kind, priority, count).Error
}

// memUsageStore 内存版存储（测试用，不持久化）
type memUsageStore struct {
	mu   sync.Mutex
	data map[string]int
}

func (s *memUsageStore) Load(_ context.Context, date, kind, priority string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[date+"|"+kind+"|"+priority], nil
}

func (s *memUsageStore) Save(_ context.Context, date, kind, priority string, count int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[date+"|"+kind+"|"+priority] = count
	return nil
}

// ---------------------------------------------------------------------------
// 优先级令牌桶（每分钟速率）
// ---------------------------------------------------------------------------

// waitEntry 令牌桶中一个排队等待的请求
type waitEntry struct {
	prio priority
	done chan struct{}
}

// prioLimiter 带优先级的物理令牌桶：令牌发放永远先服务高优先级队列，
// 低优先级只在高优队列为空时才捡漏。突发量 = 每分钟总量，速率 = 总量/60，
// 保证任何一分钟窗口内总发放不超过物理上限。
type prioLimiter struct {
	mu     sync.Mutex
	rate   float64 // tokens/sec
	burst  float64
	tokens float64
	last   time.Time
	highQ  []*waitEntry
	lowQ   []*waitEntry
}

func newPrioLimiter(rpm float64) *prioLimiter {
	l := &prioLimiter{
		rate:   rpm / 60.0,
		burst:  rpm,
		tokens: rpm,
		last:   time.Now(),
	}
	go l.run()
	return l
}

// run 后台调度循环：每 50ms 补充令牌并发放给等待队列
func (l *prioLimiter) run() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		l.mu.Lock()
		l.refill(time.Now())
		l.grant()
		l.mu.Unlock()
	}
}

func (l *prioLimiter) refill(now time.Time) {
	if elapsed := now.Sub(l.last).Seconds(); elapsed > 0 {
		l.tokens = math.Min(l.burst, l.tokens+elapsed*l.rate)
		l.last = now
	}
}

// grant 发放令牌：高优先级队列绝对优先
func (l *prioLimiter) grant() {
	for l.tokens >= 1 && (len(l.highQ) > 0 || len(l.lowQ) > 0) {
		var e *waitEntry
		if len(l.highQ) > 0 {
			e = l.highQ[0]
			l.highQ = l.highQ[1:]
		} else {
			e = l.lowQ[0]
			l.lowQ = l.lowQ[1:]
		}
		l.tokens--
		close(e.done)
	}
}

// acquire 等待一个物理令牌。队列为空且桶内有令牌时立即放行；
// 否则按优先级入队，令牌由 run() 调度发放。ctx 取消时从队列移除。
func (l *prioLimiter) acquire(ctx context.Context, p priority) error {
	l.mu.Lock()
	l.refill(time.Now())
	if l.tokens >= 1 && len(l.highQ) == 0 && len(l.lowQ) == 0 {
		l.tokens--
		l.mu.Unlock()
		return nil
	}
	e := &waitEntry{prio: p, done: make(chan struct{})}
	if p == prioHigh {
		l.highQ = append(l.highQ, e)
	} else {
		l.lowQ = append(l.lowQ, e)
	}
	l.mu.Unlock()

	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		l.remove(e)
		return ctx.Err()
	}
}

func (l *prioLimiter) remove(e *waitEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.prio == prioHigh {
		for i, w := range l.highQ {
			if w == e {
				l.highQ = append(l.highQ[:i], l.highQ[i+1:]...)
				return
			}
		}
		return
	}
	for i, w := range l.lowQ {
		if w == e {
			l.lowQ = append(l.lowQ[:i], l.lowQ[i+1:]...)
			return
		}
	}
}

// ---------------------------------------------------------------------------
// 全局限流器
// ---------------------------------------------------------------------------

// rateHub 全局限流器：每 kind 一个物理优先级桶 + 一个低优先级软桶，
// 每日高/低独立账本（物理总量共享，低优另设软上限）。
type rateHub struct {
	client *genai.Client

	genLimiter   *prioLimiter  // 文本物理桶（高低共享，高优优先）
	embedLimiter *prioLimiter  // 向量物理桶
	genSoft      *rate.Limiter // 文本低优先级软桶（总量 − 预留）
	embedSoft    *rate.Limiter // 向量低优先级软桶

	mu            sync.Mutex
	today         string
	dailyHigh     map[requestKind]int // 各种类高优先级今日已用（在线）
	dailyLow      map[requestKind]int // 各种类低优先级今日已用（后台）
	dirty         bool                // 内存计数已变化，待刷入存储
	store         usageStore          // 每日配额持久化后端（nil = 仅内存计数）
	highBlocked   map[requestKind]time.Time // 物理总量熔断截止（高低都堵，probe 恢复）
	lowBlocked    map[requestKind]time.Time // 低优软熔断截止（只堵低优，到午夜）
	cooldownUntil time.Time           // 429 全局冷却截止时间（熔断广播）
	lastProbe     time.Time           // 上次探测时间
}

// initLimiters 初始化令牌桶与每日计数（从存储恢复）。
func (h *rateHub) initLimiters() {
	h.genLimiter = newPrioLimiter(float64(config.TextModelRPM))
	h.embedLimiter = newPrioLimiter(float64(config.EmbedModelRPM))
	h.genSoft = newSoftLimiter(kindGenerate)
	h.embedSoft = newSoftLimiter(kindEmbed)
	h.dailyHigh = map[requestKind]int{kindGenerate: 0, kindEmbed: 0}
	h.dailyLow = map[requestKind]int{kindGenerate: 0, kindEmbed: 0}
	h.highBlocked = map[requestKind]time.Time{}
	h.lowBlocked = map[requestKind]time.Time{}
	h.loadUsageFromStore()
}

// startFlusher 启动后台定时刷盘协程：每隔 FlushInterval 将内存中的每日计数
// 批量写入持久化存储，避免每次请求同步写数据库造成阻塞。
func (h *rateHub) startFlusher() {
	go func() {
		ticker := time.NewTicker(config.FlushInterval)
		defer ticker.Stop()
		for range ticker.C {
			h.flushIfDirty()
		}
	}()
}

// flushIfDirty 若内存计数有变化，则批量写入持久化存储
//（每个种类按高/低优先级各写一行，共 4 行）。
func (h *rateHub) flushIfDirty() {
	h.mu.Lock()
	if !h.dirty || h.store == nil {
		h.mu.Unlock()
		return
	}
	date := h.today
	high := make(map[requestKind]int, len(h.dailyHigh))
	low := make(map[requestKind]int, len(h.dailyLow))
	for k, v := range h.dailyHigh {
		high[k] = v
	}
	for k, v := range h.dailyLow {
		low[k] = v
	}
	h.dirty = false
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	write := func(kind requestKind, p priority, count int) error {
		return h.store.Save(ctx, date, kindKey(kind), priorityKey(p), count)
	}
	for k, count := range high {
		if err := write(k, prioHigh, count); err != nil {
			h.markDirtyRetry(k)
			return
		}
	}
	for k, count := range low {
		if err := write(k, prioLow, count); err != nil {
			h.markDirtyRetry(k)
			return
		}
	}
}

// markDirtyRetry 写库失败时保留 dirty 标记，下个周期重试，避免计数丢失
func (h *rateHub) markDirtyRetry(kind requestKind) {
	h.mu.Lock()
	h.dirty = true
	h.mu.Unlock()
	log.Printf("⚠️ Gemini 每日配额写入数据库失败（%s）: 保留计数待重试\n", kindKey(kind))
}

// totalUsed 该种类今日物理总用量（高 + 低）
func (h *rateHub) totalUsed(kind requestKind) int {
	return h.dailyHigh[kind] + h.dailyLow[kind]
}

// acquire 阻塞直到获准发送一次请求：
//  1. 物理总账耗尽 → 熔断所有（每 ProbeInterval 探测恢复）
//  2. 低优先级软账本耗尽 → 立即熔断低优到次日 0 点（不探测，高优不受影响）
//  3. 全局 429 冷却
//  4. 低优先过软桶，再按优先级进物理桶
func (h *rateHub) acquire(ctx context.Context, kind requestKind, p priority) error {
	lim := h.embedLimiter
	if kind == kindGenerate {
		lim = h.genLimiter
	}

	for {
		h.mu.Lock()
		h.rolloverLocked()
		now := time.Now()
		limit := dailyLimitFor(kind)

		// 1. 物理总量耗尽 → 高优熔断（也堵低优），定时探测恢复
		if h.totalUsed(kind) >= limit {
			blockedUntil, ok := h.highBlocked[kind]
			if !ok {
				blockedUntil = nextMidnight(now)
				h.highBlocked[kind] = blockedUntil
				log.Printf("🚫 今日 %s 物理配额已耗尽（%d/%d），高低优先级全部熔断至 %v，每 %v 探测恢复\n",
					kindKey(kind), h.totalUsed(kind), limit, blockedUntil, config.ProbeInterval)
			}
			h.lastProbe = now
			h.mu.Unlock()

			waitUntil := blockedUntil
			if next := now.Add(config.ProbeInterval); next.Before(waitUntil) {
				waitUntil = next
			}
			if waitUntil.After(now) {
				if err := sleepCtx(ctx, waitUntil.Sub(now)); err != nil {
					return err
				}
			}
			if err := h.probe(ctx, kind); err != nil {
				continue // 探测失败（仍未恢复），等下一轮
			}
			h.mu.Lock()
			delete(h.highBlocked, kind)
			h.dailyHigh[kind] = 0
			h.dailyLow[kind] = 0 // 探测成功说明 API 侧已恢复，重置计数避免误熔断
			h.dirty = true
			h.mu.Unlock()
			log.Printf("✅ %s 每日配额探测成功，队列恢复", kindKey(kind))
			continue
		}

		// 2. 低优先级软账本耗尽 → 立即熔断低优到次日 0 点（不探测，后台可延迟）
		if p == prioLow && h.dailyLow[kind] >= lowDailyLimitFor(kind) {
			until, ok := h.lowBlocked[kind]
			if !ok {
				until = nextMidnight(now)
				h.lowBlocked[kind] = until
				log.Printf("⏸ %s 低优先级今日软配额已耗尽（%d/%d），后台任务熔断至 %v，额度让给在线请求\n",
					kindKey(kind), h.dailyLow[kind], lowDailyLimitFor(kind), until)
			}
			h.mu.Unlock()
			if err := sleepCtx(ctx, until.Sub(now)); err != nil {
				return err
			}
			continue
		}

		// 3. 全局 429 冷却（熔断广播）
		if now.Before(h.cooldownUntil) {
			cd := h.cooldownUntil.Sub(now)
			h.mu.Unlock()
			if err := sleepCtx(ctx, cd); err != nil {
				return err
			}
			continue
		}
		h.mu.Unlock()

		// 4. 低优先过软桶（拿"后台额度"资格），再按优先级进物理桶
		if p == prioLow {
			var soft *rate.Limiter
			if kind == kindGenerate {
				soft = h.genSoft
			} else {
				soft = h.embedSoft
			}
			if err := soft.Wait(ctx); err != nil {
				return err
			}
		}
		if err := lim.acquire(ctx, p); err != nil {
			return err
		}
		return nil
	}
}

// noteUsage 在每次成功请求后登记配额消耗（仅内存累加 + 标记待刷盘，
// 由 startFlusher 后台批量写库，避免高频同步 I/O 阻塞请求链路）。
func (h *rateHub) noteUsage(kind requestKind, p priority, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rolloverLocked()
	if p == prioHigh {
		h.dailyHigh[kind] += n
	} else {
		h.dailyLow[kind] += n
	}
	h.dirty = true

	now := time.Now()
	// 物理总账耗尽 → 高低一起熔断
	if h.totalUsed(kind) >= dailyLimitFor(kind) {
		h.highBlocked[kind] = nextMidnight(now)
		log.Printf("🚫 今日 %s 用量已达 %d/%d，高低优先级熔断至 %v\n",
			kindKey(kind), h.totalUsed(kind), dailyLimitFor(kind), h.highBlocked[kind])
	}
	// 低优软账本耗尽 → 只熔断低优
	if p == prioLow && h.dailyLow[kind] >= lowDailyLimitFor(kind) {
		h.lowBlocked[kind] = nextMidnight(now)
		log.Printf("⏸ %s 低优先级今日用量已达 %d/%d，后台任务熔断至 %v\n",
			kindKey(kind), h.dailyLow[kind], lowDailyLimitFor(kind), h.lowBlocked[kind])
	}
}

// noteThrottle 把某次请求收到的 429 冷却时间广播给全队列。
func (h *rateHub) noteThrottle(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(h.cooldownUntil) {
		h.cooldownUntil = until
		log.Printf("🔇 全局限流广播：Gemini 429，全队列暂停至 %v\n", until)
	}
}

// probe 发一个最小请求试探配额是否恢复；若仍 429 则顺带广播冷却。
// 按请求种类选择对应的最小请求（向量用 embed，文本用一句极短生成）。
func (h *rateHub) probe(ctx context.Context, kind requestKind) error {
	if kind == kindGenerate {
		model := h.client.GenerativeModel(TextModelName)
		_, err := model.GenerateContent(ctx, genai.Text("回复：ok"))
		if err == nil {
			return nil
		}
		if is429(err) {
			h.noteThrottle(retryAfter(err429(err), time.Second))
			log.Printf("⏳ %s 每日配额探测仍被限流", kindKey(kind))
		}
		return err
	}
	em := h.client.EmbeddingModel(EmbedModelName)
	_, err := em.EmbedContent(ctx, genai.Text("probe"))
	if err == nil {
		return nil
	}
	if is429(err) {
		h.noteThrottle(retryAfter(err429(err), time.Second))
		log.Printf("⏳ %s 每日配额探测仍被限流", kindKey(kind))
	}
	return err
}

// is429 判断错误是否为 429 限流错误
func is429(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == 429
}

// err429 从错误中提取 *googleapi.Error（调用前需先 is429 确认）
func err429(err error) *googleapi.Error {
	var gerr *googleapi.Error
	errors.As(err, &gerr)
	return gerr
}

// rolloverLocked 跨天时重置每日计数（dirty 标记让后台把新日期刷入存储）
func (h *rateHub) rolloverLocked() {
	today := time.Now().Format("2006-01-02")
	if h.today != today {
		h.today = today
		for k := range h.dailyHigh {
			h.dailyHigh[k] = 0
		}
		for k := range h.dailyLow {
			h.dailyLow[k] = 0
		}
		h.highBlocked = map[requestKind]time.Time{}
		h.lowBlocked = map[requestKind]time.Time{}
		h.dirty = true
	}
}

// loadUsageFromStore 从持久化存储恢复今天的计数（进程重启后限流计数不丢）
func (h *rateHub) loadUsageFromStore() {
	today := time.Now().Format("2006-01-02")
	h.today = today
	if h.store == nil {
		return
	}
	for _, k := range []requestKind{kindGenerate, kindEmbed} {
		high, err := h.store.Load(context.Background(), today, kindKey(k), priorityKey(prioHigh))
		if err != nil {
			log.Printf("⚠️ 加载 %s 高优先级配额失败: %v", kindKey(k), err)
			continue
		}
		low, err := h.store.Load(context.Background(), today, kindKey(k), priorityKey(prioLow))
		if err != nil {
			log.Printf("⚠️ 加载 %s 低优先级配额失败: %v", kindKey(k), err)
			continue
		}
		h.dailyHigh[k] = high
		h.dailyLow[k] = low
		if total := high + low; total > 0 {
			log.Printf("📊 已从数据库恢复今日 %s 用量: 高优 %d + 后台 %d = %d/%d",
				kindKey(k), high, low, total, dailyLimitFor(k))
		}
	}
}

func nextMidnight(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
