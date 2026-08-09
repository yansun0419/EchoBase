package llm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"gorm.io/gorm"

	"echobase/internal/config"
)

// 各请求种类对应的模型名。换大模型时只需改这里（或 gemini.go 里的调用），
// 执行器与限流器代码完全不需要动。
const (
	TextModelName  = "gemini-3.1-flash-lite"
	EmbedModelName = "gemini-embedding-2"
)

// maxRetries 免费档 Gemini API 限流时统一退避重试，
// 避免切块流水线因限流静默丢失数据。次数见 config.MaxRetries。
// 这里作为 withRetry 循环上限。
const maxRetries = config.MaxRetries

// withRetry 对 Gemini 请求做 429 退避重试：优先按 API 返回的 "retry in Xs" 等待，
// 否则指数退避，最多重试 maxRetries 次。拿到 429 时会同步广播给全局限流网关
// （getHub().noteThrottle），让所有排队中的请求一起暂停。
func withRetry[T any](fn func() (T, error)) (T, error) {
	var zero T
	var lastErr error
	wait := time.Second
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(wait)
		}
		result, err := fn()
		if err == nil {
			return result, nil
		}
		lastErr = err
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && gerr.Code == 429 {
			wait = retryAfter(gerr, wait)
			getHub().noteThrottle(wait)
			log.Printf("⏳ Gemini 429 限流，等待 %v 后重试（第 %d 次）\n", wait, attempt+1)
			continue
		}
		return zero, err
	}
	return zero, lastErr
}

// retryAfter 从 429 错误信息中提取 API 建议的重试秒数（如 "Please retry in 50.5s"），
// 否则按指数退避翻倍，并保底返回至少翻倍后的值。
func retryAfter(gerr *googleapi.Error, currentWait time.Duration) time.Duration {
	const minWait = config.RetryMinWait
	// retry 提示可能在 Message 或 Body 里
	text := gerr.Message + " " + gerr.Body
	if i := strings.LastIndex(text, "retry in "); i >= 0 {
		rest := text[i+len("retry in "):]
		var seconds float64
		if _, err := fmt.Sscanf(rest, "%f", &seconds); err == nil && seconds > 0 {
			if d := time.Duration(seconds*float64(time.Second)); d > currentWait {
				return d + time.Second
			}
		}
	}
	next := currentWait * 2
	if next < minWait {
		return minWait
	}
	return next
}

// getAPIKey 从环境变量获取 Gemini API Key，缺失时直接 fatal 退出
func getAPIKey() string {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		panic("❌ 致命错误: 未设置 GEMINI_API_KEY 环境变量")
	}
	return key
}

// requestKind 区分两种独立配额的模型种类。
// 新增一种配额（如另一种模型）时在此扩展，然后在限流器里加对应令牌桶。
type requestKind int

const (
	kindGenerate requestKind = iota // 文本生成（TextModelName）
	kindEmbed                       // 向量化（EmbedModelName）
)

// priority 区分在线用户请求与后台离线任务，是 QoS 资源隔离的核心维度。
// 令牌发放高优绝对优先，低优被软配额限制，永远让出预留余量给高优。
type priority int

const (
	prioLow  priority = iota // 后台离线任务（清洗/切片/合并/后台向量化）
	prioHigh                 // 在线用户请求（提问/检索）
)

// ---------------------------------------------------------------------------
// 执行器门面（Gateway）
//
// 所有模型方法统一通过 Execute 发请求，内部自动串联：
//   令牌桶排队 → 真实请求 → 每日配额计数 → 429 退避重试 + 熔断广播
// 本层不包含任何具体业务逻辑，换模型后端时无需改动。
// ---------------------------------------------------------------------------

// Execute 是 LLM API 的统一调用入口（Request Hub 的门面）。
//
// 所有新增的模型方法只需三步即可接入限流体系：
//  1. 声明请求类型 kind（kindGenerate 文本 / kindEmbed 向量，决定走哪个令牌桶）；
//  2. 声明请求优先级 p（prioHigh 在线提问 / prioLow 后台任务，决定排队次序与配额）；
//  3. 写一个闭包，用传入的共享 client 发请求、解析结果。
//
// 内部自动完成：令牌桶排队（高优优先，低优受软配额约束）→ 真实请求 →
// 每日配额计数（高/低独立账本 + 共享物理熔断）→ 429 退避重试 + 熔断广播。
// 调用方零样板。
//
// 注意：一个 Execute 调用对应一次 API 请求（消耗 1 令牌 + 1 每日计数）；
// 批量场景（如多个 batch 的向量化）请在循环里逐个调用 Execute。
func Execute[T any](kind requestKind, p priority, fn func(client *genai.Client, ctx context.Context) (T, error)) (T, error) {
	h := getHub()
	return withRetry(func() (T, error) {
		var zero T
		if err := h.acquire(context.Background(), kind, p); err != nil {
			return zero, err
		}
		res, err := fn(h.client, context.Background())
		if err != nil {
			return zero, err
		}
		h.noteUsage(kind, p, 1)
		return res, nil
	})
}

// ---------------------------------------------------------------------------
// 共享客户端单例（Provider）
// ---------------------------------------------------------------------------

var (
	hubOnce sync.Once
	hub     *rateHub

	// quotaStore 每日配额持久化后端，默认内存版；服务启动时由 InitUsageStore
	// 注入数据库版（见 cmd/server/main.go），容器重建后计数从 DB 恢复。
	quotaStore usageStore = &memUsageStore{data: make(map[string]int)}
)

// InitUsageStore 注入每日配额持久化后端（应在服务启动时调用一次，传入 *gorm.DB）。
// 不调用时回退为纯内存计数（测试环境）。
func InitUsageStore(db *gorm.DB) {
	if db == nil {
		return
	}
	quotaStore = &pgUsageStore{db: db}
}

func getHub() *rateHub {
	hubOnce.Do(func() {
		ctx := context.Background()
		client, err := genai.NewClient(ctx, option.WithAPIKey(getAPIKey()))
		if err != nil {
			panic(fmt.Sprintf("❌ 创建 Gemini 客户端失败: %v", err))
		}
		h := &rateHub{
			client: client,
			store:  quotaStore,
		}
		h.initLimiters()
		h.startFlusher()
		hub = h
	})
	return hub
}
