package llm

import (
	"context"
	"errors"
	"fmt"
	"log"
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
// 文本/向量各自独立的每日配额（按 kind 分别计数、分别熔断）。
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

// dailyLimitFor 返回该请求种类的每日配额上限
func dailyLimitFor(kind requestKind) int {
	if kind == kindGenerate {
		return config.TextModelRPD
	}
	return config.EmbedModelRPD
}

// usageStore 每日配额持久化后端。
// 生产环境用 pgUsageStore（PostgreSQL），测试可用内存实现，换后端零侵入。
type usageStore interface {
	Load(ctx context.Context, date, kind string) (int, error)
	Save(ctx context.Context, date, kind string, count int) error
}

// pgUsageStore 基于 PostgreSQL 的每日配额存储（api_quotas 表，(date,kind) 联合主键）
type pgUsageStore struct {
	db *gorm.DB
}

func (s *pgUsageStore) Load(ctx context.Context, date, kind string) (int, error) {
	var rec struct{ UsageCount int }
	err := s.db.WithContext(ctx).
		Table("api_quotas").
		Select("usage_count").
		Where("date = ? AND kind = ?", date, kind).
		Scan(&rec).Error
	if err != nil {
		return 0, err
	}
	return rec.UsageCount, nil
}

func (s *pgUsageStore) Save(ctx context.Context, date, kind string, count int) error {
	// upsert：当天该种类记录存在则覆盖，不存在则插入
	return s.db.WithContext(ctx).Exec(`
		INSERT INTO api_quotas (date, kind, usage_count) VALUES (?, ?, ?)
		ON CONFLICT (date, kind) DO UPDATE SET usage_count = EXCLUDED.usage_count
	`, date, kind, count).Error
}

// memUsageStore 内存版存储（测试用，不持久化）
type memUsageStore struct {
	mu   sync.Mutex
	data map[string]int
}

func (s *memUsageStore) Load(_ context.Context, date, kind string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[date+"|"+kind], nil
}

func (s *memUsageStore) Save(_ context.Context, date, kind string, count int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[date+"|"+kind] = count
	return nil
}

// rateHub 全局限流器：双令牌桶（文本/向量各按分钟配额）+ 每日计数熔断。
type rateHub struct {
	client *genai.Client

	genLimiter   *rate.Limiter
	embedLimiter *rate.Limiter

	mu            sync.Mutex
	today         string
	dailyUsed     map[requestKind]int // 各请求种类今日已用次数
	dirty         bool                // 内存计数已变化，待刷入存储
	store         usageStore          // 每日配额持久化后端（nil = 仅内存计数）
	dailyBlocked  map[requestKind]time.Time // 各请求种类每日配额熔断截止时间
	cooldownUntil time.Time           // 429 全局冷却截止时间（熔断广播）
	lastProbe     time.Time           // 上次探测时间
}

// initLimiters 初始化令牌桶与每日计数（从存储恢复）。
func (h *rateHub) initLimiters() {
	h.genLimiter = rate.NewLimiter(rate.Limit(float64(config.TextModelRPM)/60.0), config.TextModelRPM)
	h.embedLimiter = rate.NewLimiter(rate.Limit(float64(config.EmbedModelRPM)/60.0), config.EmbedModelRPM)
	h.dailyUsed = map[requestKind]int{kindGenerate: 0, kindEmbed: 0}
	h.dailyBlocked = map[requestKind]time.Time{}
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

// flushIfDirty 若内存计数有变化，则批量写入持久化存储（每种请求各写一行）。
func (h *rateHub) flushIfDirty() {
	h.mu.Lock()
	if !h.dirty || h.store == nil {
		h.mu.Unlock()
		return
	}
	date := h.today
	used := make(map[requestKind]int, len(h.dailyUsed))
	for k, v := range h.dailyUsed {
		used[k] = v
	}
	h.dirty = false
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for k, count := range used {
		if err := h.store.Save(ctx, date, kindKey(k), count); err != nil {
			// 写入失败则保留 dirty 标记，下个周期重试，避免计数丢失
			h.mu.Lock()
			h.dirty = true
			h.mu.Unlock()
			log.Printf("⚠️ Gemini 每日配额写入数据库失败（%s）: %v", kindKey(k), err)
			return
		}
	}
}

// acquire 阻塞直到获准发送一次请求：先等全局 429 冷却，再检查对应请求种类的
// 每日配额，熔断期间按 ProbeInterval 定时发最小探测请求试探恢复，最后拿令牌桶。
func (h *rateHub) acquire(ctx context.Context, kind requestKind) error {
	lim := h.embedLimiter
	if kind == kindGenerate {
		lim = h.genLimiter
	}

	for {
		h.mu.Lock()
		h.rolloverLocked()

		now := time.Now()
		limit := dailyLimitFor(kind)
		// 该种类每日配额耗尽 → 熔断到次日 0 点，期间每 ProbeInterval 探测一次
		if h.dailyUsed[kind] >= limit {
			blockedUntil, ok := h.dailyBlocked[kind]
			if !ok {
				blockedUntil = nextMidnight(now)
				h.dailyBlocked[kind] = blockedUntil
				log.Printf("🚫 今日 %s 配额已耗尽（%d/%d），熔断至 %v，每 %v 探测恢复\n",
					kindKey(kind), h.dailyUsed[kind], limit, blockedUntil, config.ProbeInterval)
			}
			h.lastProbe = now // 记录本轮探测计划时刻
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
			delete(h.dailyBlocked, kind)
			h.dailyUsed[kind] = 0 // 探测成功说明 API 侧已恢复，重置计数避免误熔断
			h.dirty = true
			h.mu.Unlock()
			log.Printf("✅ %s 每日配额探测成功，队列恢复", kindKey(kind))
			continue
		}

		// 全局 429 冷却（熔断广播）
		if now.Before(h.cooldownUntil) {
			cd := h.cooldownUntil.Sub(now)
			h.mu.Unlock()
			if err := sleepCtx(ctx, cd); err != nil {
				return err
			}
			continue
		}
		h.mu.Unlock()

		if err := lim.Wait(ctx); err != nil {
			return err
		}
		return nil
	}
}

// noteUsage 在每次成功请求后登记配额消耗（仅内存累加 + 标记待刷盘，
// 由 startFlusher 后台批量写库，避免高频同步 I/O 阻塞请求链路）。
func (h *rateHub) noteUsage(kind requestKind, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rolloverLocked()
	h.dailyUsed[kind] += n
	h.dirty = true
	if limit := dailyLimitFor(kind); h.dailyUsed[kind] >= limit {
		until := nextMidnight(time.Now())
		h.dailyBlocked[kind] = until
		log.Printf("🚫 今日 %s 用量已达 %d/%d，熔断至 %v\n",
			kindKey(kind), h.dailyUsed[kind], limit, until)
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
		for k := range h.dailyUsed {
			h.dailyUsed[k] = 0
		}
		h.dailyBlocked = map[requestKind]time.Time{}
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
		count, err := h.store.Load(context.Background(), today, kindKey(k))
		if err != nil {
			log.Printf("⚠️ 加载 %s 每日配额失败: %v", kindKey(k), err)
			continue
		}
		h.dailyUsed[k] = count
		if count > 0 {
			log.Printf("📊 已从数据库恢复今日 %s 用量: %d/%d", kindKey(k), count, dailyLimitFor(k))
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

var _ = fmt.Sprintf // keep fmt import if unused paths change
