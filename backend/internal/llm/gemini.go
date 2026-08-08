package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/generative-ai-go/genai"
	"github.com/pgvector/pgvector-go"
	"golang.org/x/time/rate"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// maxRetries 免费档 Gemini API 每分钟限 100 次请求，突发时高频触发 429，
// 这里统一做指数退避重试，避免切块流水线因限流静默丢失数据。
const maxRetries = 5

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
	const minWait = 2 * time.Second
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

// ---------------------------------------------------------------------------
// 全局请求网关（Request Hub）
//
// 免费档配额（共享同一个 API Key，所有 worker/task 共同消耗）：
//   - 文本生成（gemini-3.1-flash-lite）：15 req/min
//   - 向量化（gemini-embedding-2）：100 req/min
//   - 每日总请求：500 req/day
//
// 设计：令牌桶 + 共享客户端。所有公开函数发请求前统一 acquire 令牌，
// 未超配额保持并发，超配额自动排队匀速发放，绝不突破每分钟上限；
// 每日计数持久化到本地 JSON，跨天重置，到限熔断后每 5 分钟探测恢复。
// ---------------------------------------------------------------------------

const (
	genRatePerMin   = 15
	embedRatePerMin = 100
	dailyLimit      = 500
	probeInterval   = 5 * time.Minute
)

// requestKind 区分两种独立配额的模型
type requestKind int

const (
	kindGenerate requestKind = iota // gemini-3.1-flash-lite
	kindEmbed                       // gemini-embedding-2
)

type usageRecord struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type rateHub struct {
	client *genai.Client

	genLimiter   *rate.Limiter
	embedLimiter *rate.Limiter

	mu                sync.Mutex
	today             string
	dailyUsed         int
	dailyBlockedUntil time.Time // 每日配额耗尽时的熔断截止时间
	cooldownUntil     time.Time // 429 全局冷却截止时间（熔断广播）
	lastProbe         time.Time // 上次探测时间
	usageFilePath     string
}

var (
	hubOnce sync.Once
	hub     *rateHub
)

func getHub() *rateHub {
	hubOnce.Do(func() {
		ctx := context.Background()
		client, err := genai.NewClient(ctx, option.WithAPIKey(getAPIKey()))
		if err != nil {
			panic(fmt.Sprintf("❌ 创建 Gemini 客户端失败: %v", err))
		}
		h := &rateHub{
			client:        client,
			genLimiter:    rate.NewLimiter(rate.Limit(float64(genRatePerMin)/60.0), genRatePerMin),
			embedLimiter:  rate.NewLimiter(rate.Limit(float64(embedRatePerMin)/60.0), embedRatePerMin),
			usageFilePath: filepath.Join("data", "gemini_usage.json"),
		}
		h.loadUsage()
		hub = h
	})
	return hub
}

// acquire 阻塞直到获准发送一次请求：先等全局 429 冷却，再检查每日配额，
// 熔断期间按 probeInterval 定时发最小探测请求试探恢复，最后拿令牌桶。
func (h *rateHub) acquire(ctx context.Context, kind requestKind) error {
	lim := h.embedLimiter
	if kind == kindGenerate {
		lim = h.genLimiter
	}

	for {
		h.mu.Lock()
		h.rolloverLocked()

		now := time.Now()
		// 每日配额耗尽 → 熔断到次日 0 点，期间每 probeInterval 探测一次
		if h.dailyUsed >= dailyLimit {
			if h.dailyBlockedUntil.IsZero() {
				h.dailyBlockedUntil = nextMidnight(now)
				log.Printf("🚫 今日 Gemini 配额已耗尽（%d/%d），熔断至 %v，每 %v 探测恢复\n",
					h.dailyUsed, dailyLimit, h.dailyBlockedUntil, probeInterval)
			}
			blockedUntil := h.dailyBlockedUntil
			h.lastProbe = now // 记录本轮探测计划时刻
			h.mu.Unlock()

			waitUntil := blockedUntil
			if next := now.Add(probeInterval); next.Before(waitUntil) {
				waitUntil = next
			}
			if waitUntil.After(now) {
				if err := sleepCtx(ctx, waitUntil.Sub(now)); err != nil {
					return err
				}
			}
			if err := h.probe(ctx); err != nil {
				continue // 探测失败（仍未恢复），等下一轮
			}
			h.mu.Lock()
			h.dailyBlockedUntil = time.Time{}
			h.dailyUsed = 0 // 探测成功说明 API 侧已恢复，重置计数避免误熔断
			h.saveUsageLocked()
			h.mu.Unlock()
			log.Printf("✅ Gemini 每日配额探测成功，队列恢复")
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

// noteUsage 在每次成功请求后登记配额消耗（embedding 批量请求按批次计 1 次）。
func (h *rateHub) noteUsage(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rolloverLocked()
	h.dailyUsed += n
	h.saveUsageLocked()
	if h.dailyUsed >= dailyLimit {
		h.dailyBlockedUntil = nextMidnight(time.Now())
		log.Printf("🚫 今日 Gemini 用量已达 %d/%d，熔断至 %v\n",
			h.dailyUsed, dailyLimit, h.dailyBlockedUntil)
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

// probe 发一个最小嵌入请求试探配额是否恢复；若仍 429 则顺带广播冷却。
func (h *rateHub) probe(ctx context.Context) error {
	em := h.client.EmbeddingModel("gemini-embedding-2")
	_, err := em.EmbedContent(ctx, genai.Text("probe"))
	if err != nil {
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && gerr.Code == 429 {
			h.noteThrottle(retryAfter(gerr, time.Second))
			log.Printf("⏳ 每日配额探测仍被限流")
		}
		return err
	}
	return nil
}

// rolloverLocked 跨天时重置每日计数
func (h *rateHub) rolloverLocked() {
	today := time.Now().Format("2006-01-02")
	if h.today != today {
		h.today = today
		h.dailyUsed = 0
		h.dailyBlockedUntil = time.Time{}
		h.saveUsageLocked()
	}
}

func (h *rateHub) loadUsage() {
	data, err := os.ReadFile(h.usageFilePath)
	if err != nil {
		return
	}
	var rec usageRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return
	}
	h.today = time.Now().Format("2006-01-02")
	if rec.Date == h.today {
		h.dailyUsed = rec.Count
	}
}

func (h *rateHub) saveUsageLocked() {
	if h.today == "" {
		h.today = time.Now().Format("2006-01-02")
	}
	if err := os.MkdirAll(filepath.Dir(h.usageFilePath), 0o755); err != nil {
		return
	}
	rec := usageRecord{Date: h.today, Count: h.dailyUsed}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	tmp := h.usageFilePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err == nil {
		os.Rename(tmp, h.usageFilePath)
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

// Execute 是 Gemini API 的统一调用入口（Request Hub 的门面）。
//
// 所有新增的模型方法只需两步即可接入限流体系：
//  1. 声明请求类型 kind（kindGenerate 文本 / kindEmbed 向量，决定走哪个令牌桶）；
//  2. 写一个闭包，用传入的共享 client 发请求、解析结果。
//
// 内部自动完成：令牌桶排队（自动选择发送时机）→ 真实请求 →
// 每日配额计数 → 429 退避重试 + 熔断广播。调用方零样板。
//
// 注意：一个 Execute 调用对应一次 API 请求（消耗 1 令牌 + 1 每日计数）；
// 批量场景（如多个 batch 的向量化）请在循环里逐个调用 Execute。
func Execute[T any](kind requestKind, fn func(client *genai.Client, ctx context.Context) (T, error)) (T, error) {
	h := getHub()
	return withRetry(func() (T, error) {
		var zero T
		if err := h.acquire(context.Background(), kind); err != nil {
			return zero, err
		}
		res, err := fn(h.client, context.Background())
		if err != nil {
			return zero, err
		}
		h.noteUsage(1)
		return res, nil
	})
}

// GenerateSummaryAndTags 调用 Gemini 生成摘要和标签
func GenerateSummaryAndTags(contextData string) (summary string, tags string, err error) {
	type result struct {
		summary string
		tags    string
	}
	res, err := Execute(kindGenerate, func(client *genai.Client, ctx context.Context) (result, error) {
		var r result
		model := client.GenerativeModel("gemini-3.1-flash-lite")
		// 构造极其严格的 Prompt（提示词工程）
		prompt := fmt.Sprintf(`
请你作为一个专业的信息提炼专家，阅读以下内容，并按要求输出：
1. 用一段简明扼要的话概括核心内容（摘要）。
2. 提取 3-5 个核心关键词，用逗号分隔。

格式严格如下：
摘要：[你的摘要内容]
标签：[标签1,标签2,标签3]

需要处理的内容如下：
%s
`, contextData)

		resp, err := model.GenerateContent(ctx, genai.Text(prompt))
		if err != nil {
			return r, fmt.Errorf("生成内容失败: %w", err)
		}

		// 解析大模型返回的文本
		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			aiText := fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0])

			// 简单粗暴的字符串处理，分离摘要和标签
			lines := strings.Split(aiText, "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "摘要：") {
					r.summary = strings.TrimPrefix(line, "摘要：")
				} else if strings.HasPrefix(line, "标签：") {
					r.tags = strings.TrimPrefix(line, "标签：")
				}
			}
			return r, nil
		}

		return r, fmt.Errorf("AI 返回格式异常")
	})
	return res.summary, res.tags, err
}

// SemanticSplit 用 AI 把长文本切成语义自洽的知识区块（Agentic Chunking）
// 每个区块都会自然融入整篇文章的核心主旨，拿出来就能独立存活
func SemanticSplit(content string) ([]string, error) {
	return Execute(kindGenerate, func(client *genai.Client, ctx context.Context) ([]string, error) {
		model := client.GenerativeModel("gemini-3.1-flash-lite")

		// 强制返回结构化 JSON，避免 AI 吐出乱七八糟的格式导致解析崩溃
		model.ResponseMIMEType = "application/json"

		// 核心 Prompt：语义约束 + 段落规模约束（不用字数——大模型不会数数）
		// 宁长勿碎 / 逻辑自洽 / 上下文继承 / 规模预期
		prompt := fmt.Sprintf(`你是一个专业的知识库架构师。请将下面这篇长文切分为多个高质量的知识碎片。

【切分原则】
1. 宁长勿碎：绝不能切成只有一两句话的短句。多个关联紧密的短句必须合并成一个完整区块。
2. 逻辑自洽：每个碎片必须包含一个完整的子主题（如：一个完整的概念推导、一个完整的前因后果、一个完整的方法论），信息量要丰满。
3. 上下文继承：如果切片是上文的延续，请在切片开头用一句话概括前置背景，避免脱离语境。
4. 规模预期：每个切片最好包含 2 到 3 个完整的段落。
5. 严禁遗漏任何信息，所有要点必须完整分布到各个切片中；切片之间不允许重复内容。
6. 只输出切好的纯文本片段，不要输出任何解释、编号、标题或前后缀。

请以 JSON 数组的格式输出纯文本切片，例如：["切片1内容", "切片2内容", "切片3内容"]

【待切分内容】
%s`, content)

		resp, err := model.GenerateContent(ctx, genai.Text(prompt))
		if err != nil {
			return nil, fmt.Errorf("AI 切分失败: %w", err)
		}

		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			aiResponse := fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0])

			var parts []string
			if err := json.Unmarshal([]byte(aiResponse), &parts); err != nil {
				return nil, fmt.Errorf("解析 AI 切分结果失败: %v\n原始返回: %s", err, aiResponse)
			}
			return parts, nil
		}

		return nil, fmt.Errorf("AI 返回结果为空")
	})
}

// GenerateEmbeddingsBatch 批量生成向量，一次请求完成多句向量化（逐句向量断崖法的基石）
func GenerateEmbeddingsBatch(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// 分批打包（batch 有请求上限，超过 100 条分批请求）。
	// 每个 batch 是一次独立 HTTP 请求，循环内逐个走 Execute，
	// 各自占一个令牌 + 一次每日配额，自动排队发送。
	const batchSize = 100
	var all [][]float32
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batchTexts := texts[start:end]

		batchVecs, err := Execute(kindEmbed, func(client *genai.Client, ctx context.Context) ([][]float32, error) {
			em := client.EmbeddingModel("gemini-embedding-2")
			batch := em.NewBatch()
			for _, t := range batchTexts {
				batch.AddContent(genai.Text(t))
			}
			res, err := em.BatchEmbedContents(ctx, batch)
			if err != nil {
				return nil, fmt.Errorf("批量向量生成失败: %w", err)
			}
			var vecs [][]float32
			for _, e := range res.Embeddings {
				vecs = append(vecs, e.Values)
			}
			return vecs, nil
		})
		if err != nil {
			return nil, err
		}
		all = append(all, batchVecs...)
	}
	return all, nil
}

// Contextualize 用 AI 给物理切好的区块补齐上下文（Contextual Retrieval）。
// 安全约束：AI 只被允许生成"前置背景句"，绝不能改写/重写区块本体——
// 返回的文本会由调用方拼接到区块开头，原始区块内容 100% 保留，杜绝 AI 压缩丢内容。
func Contextualize(chunk string, fullContent string) (string, error) {
	return Execute(kindGenerate, func(client *genai.Client, ctx context.Context) (string, error) {
		model := client.GenerativeModel("gemini-3.1-flash-lite")

		prompt := fmt.Sprintf(`你是一个上下文补齐助手。给定一篇完整文章，以及从中截取的一个区块。
请根据文章前部背景，为这个区块生成 1~3 句"前置背景句"，用于：
- 替换区块中代词（如"它"、"这个方法"）所指代的具体名词。
- 补齐区块依赖的前置背景信息（如运行环境、前提假设、所属主题）。
约束（必须严格遵守）：
1. 只输出背景句本身，绝对不要输出或改写区块的任何原文内容。
2. 若区块本身足够自洽、无需背景，只输出"无"。
3. 不要输出任何解释、编号或前后缀。

【完整文章】
%s

【待补齐区块】
%s`, fullContent, chunk)

		resp, err := model.GenerateContent(ctx, genai.Text(prompt))
		if err != nil {
			return "", fmt.Errorf("上下文补齐失败: %w", err)
		}

		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			return strings.TrimSpace(fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0])), nil
		}
		return "", fmt.Errorf("AI 返回结果为空")
	})
}

// WashContent 调用 Gemini 对原始内容进行统一风格洗稿、排版，输出纯净数据
func WashContent(rawContent string) (string, error) {
	return Execute(kindGenerate, func(client *genai.Client, ctx context.Context) (string, error) {
		model := client.GenerativeModel("gemini-3.1-flash-lite")

		// 洗稿 Prompt：目标是把口水话原稿清洗成统一风格、逻辑清晰的纯净知识文本
		prompt := fmt.Sprintf(`你是一个知识库内容的专业洗稿排版引擎。
你的任务是将用户提交的【原始内容】清洗为统一风格、逻辑清晰、排版干净的纯文本数据。
清洗规则：
1. 去除口头禅、重复、无意义的口水话和噪声（例如"嗯"、"啊"、"就是"、"然后呢"之类的填充词）。
2. 保留所有核心信息，绝对不允许丢失或篡改任何事实、数据、观点。
3. 按照清晰合理的逻辑重新组织段落，使用 Markdown 标题、列表等排版元素让内容更有条理。
4. 只输出清洗后的正文，不要输出任何解释、前缀或后缀。

【原始内容】:
%s`, rawContent)

		resp, err := model.GenerateContent(ctx, genai.Text(prompt))
		if err != nil {
			return "", fmt.Errorf("洗稿失败: %w", err)
		}

		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			return strings.TrimSpace(fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0])), nil
		}
		return "", fmt.Errorf("AI 返回结果为空")
	})
}

// GenerateEmbedding 调用 Gemini 专门的向量模型，生成 3072 维向量
func GenerateEmbedding(content string) (*pgvector.Vector, error) {
	return Execute(kindEmbed, func(client *genai.Client, ctx context.Context) (*pgvector.Vector, error) {
		// 选用最新的文本向量模型
		em := client.EmbeddingModel("gemini-embedding-2")
		res, err := em.EmbedContent(ctx, genai.Text(content))
		if err != nil {
			return nil, fmt.Errorf("生成向量失败: %w", err)
		}

		if len(res.Embedding.Values) > 0 {
			// 将返回的 []float32 转换成 pgvector 认识的格式
			vec := pgvector.NewVector(res.Embedding.Values)
			return &vec, nil
		}

		return nil, fmt.Errorf("向量生成为空")
	})
}

// GenerateRAGAnswer 基于检索到的上下文生成 RAG 回答
func GenerateRAGAnswer(query string, contextData string) (string, error) {
	return Execute(kindGenerate, func(client *genai.Client, ctx context.Context) (string, error) {
		model := client.GenerativeModel("gemini-3.1-flash-lite")

		// 这是 RAG 系统最核心的 Prompt 工程，强制 AI 必须且只能基于给定的知识库资料作答
		prompt := fmt.Sprintf(`你是一个极其专业的知识库智能助手。
请仔细阅读以下【参考资料】，并用温柔、清晰且结构化的中文来回答用户的【问题】。
要求：
1. 你的回答必须完全基于参考资料中的内容，绝对不允许胡编乱造。
2. 如果参考资料中没有提及相关答案，请如实回答"知识库中没有相关信息"。

【参考资料】:
%s

【用户问题】:
%s`, contextData, query)

		resp, err := model.GenerateContent(ctx, genai.Text(prompt))
		if err != nil {
			return "", fmt.Errorf("生成回答失败: %w", err)
		}

		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			return fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0]), nil
		}
		return "", fmt.Errorf("AI 返回结果为空")
	})
}

// 定义大模型返回的严格 JSON 结构
type MergeDecision struct {
	Decision      string `json:"decision"`       // "MERGE" 或 "KEEP_SEPARATE" 或 "SPLIT"
	MergedContent string `json:"merged_content"` // 如果是 MERGE，这里存放重写后的完整内容
	Reason        string `json:"reason"`         // AI 给出这个决定的简短理由（方便我们调试 Debug）
}

// EvaluateAndMerge 核心记忆决策引擎
// newContent: 用户刚刚提交的新碎片
// existingContent: 数据库中检索到的最相似的老文章
func EvaluateAndMerge(newContent string, existingContent string) (*MergeDecision, error) {
	return Execute(kindGenerate, func(client *genai.Client, ctx context.Context) (*MergeDecision, error) {
		model := client.GenerativeModel("gemini-3.1-flash-lite")

		// 强制要求大模型返回 JSON
		model.ResponseMIMEType = "application/json"

		// 核心 Prompt 工程：给 AI 设定极度严苛的合并规则
		prompt := fmt.Sprintf(`你是一个知识库底层的数据整理引擎（Agentic Memory Router）。
你的任务是评估用户提交的【新碎片】是否应该与数据库中检索到的【高相似度旧文档】进行合并。

【旧文档内容】:
%s

【新碎片内容】:
%s

评估规则：
1. 如果新旧内容讲述的是同一个具体事物的延续、补充、状态更新，或者对旧文档中观点的深度探讨，请做出 "MERGE" 的决定，并将两者天衣无缝地重写为一篇逻辑严密的完整 Markdown 文档（无损保留所有信息）。
2. 如果新旧内容只是恰好提到了相似的名词（例如一个是"竹笛的材质"，一个是"洞箫的材质"），但本质上是两个独立的知识实体，请做出 "KEEP_SEPARATE" 的决定。

你必须严格遵守以下 JSON 格式输出：
{
  "decision": "MERGE" 或 "KEEP_SEPARATE",
  "merged_content": "如果 decision 为 MERGE，请在这里输出融合后的完整 Markdown 文本。如果为 KEEP_SEPARATE，留空字符串即可。",
  "reason": "简短解释为什么做出这个决定"
}`, existingContent, newContent)

		resp, err := model.GenerateContent(ctx, genai.Text(prompt))
		if err != nil {
			return nil, fmt.Errorf("生成决策失败: %w", err)
		}

		if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
			aiResponse := fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0])

			var decision MergeDecision
			if err := json.Unmarshal([]byte(aiResponse), &decision); err != nil {
				return nil, fmt.Errorf("解析 AI JSON 返回结果失败: %v\n原始返回: %s", err, aiResponse)
			}

			return &decision, nil
		}

		return nil, fmt.Errorf("AI 返回结果为空")
	})
}
