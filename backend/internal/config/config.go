// Package config 集中管理全项目的魔法数字（配额、切块阈值、网关内部参数）。
// 所有业务代码禁止散落硬编码数字，一律引用本包，改配置只动这一处。
package config

import "time"

// ---------------------------------------------------------------------------
// 语义切块参数（单位：估算 Token，中文 1 字 ≈ 1 token）
// ⚠️ 已定义于此，但尚未接入 chunker（用户指定"先写入，之后用的时候再启用"）。
// ---------------------------------------------------------------------------

const (
	// ChunkSplitTrigger 切分触发阈值：洗稿后体积低于该值直接整块过，不切分
	ChunkSplitTrigger = 800

	// ChunkTargetMin 组块目标体积下限
	ChunkTargetMin = 300

	// ChunkTargetMax 组块目标体积上限
	ChunkTargetMax = 500
)

// ---------------------------------------------------------------------------
// 文本生成模型配额（gemini-3.1-flash-lite，免费档）
// ---------------------------------------------------------------------------

const (
	// TextModelRPM 文本生成每分钟请求上限（物理总量，高低优先级共享）
	TextModelRPM = 15

	// TextModelRPD 文本生成每日请求上限（物理总量，高低优先级共享）
	TextModelRPD = 500

	// TextModelRPMReserved 文本生成每分钟为在线高优先级请求预留的余量。
	// 低优先级（后台清洗/切片/合并）可用量由程序自动算出：总量 − 预留。
	// 保证用户提问到达时，无论后台多么繁忙，每分钟至少能抢到该余量。
	TextModelRPMReserved = 5

	// TextModelRPDReserved 文本生成每天为在线高优先级请求预留的余量。
	// 低优先级每日软上限 = TextModelRPD − 预留，到点立即熔断，当日剩余全部让给高优。
	TextModelRPDReserved = 100
)

// ---------------------------------------------------------------------------
// 向量模型配额（gemini-embedding-2，免费档）
// ---------------------------------------------------------------------------

const (
	// EmbedModelRPM 向量化每分钟请求上限（物理总量，高低优先级共享）
	EmbedModelRPM = 100

	// EmbedModelRPD 向量化每日请求上限（物理总量，高低优先级共享）
	EmbedModelRPD = 1000

	// EmbedModelRPMReserved 向量化每分钟为在线高优先级请求预留的余量
	EmbedModelRPMReserved = 5

	// EmbedModelRPDReserved 向量化每天为在线高优先级请求预留的余量
	EmbedModelRPDReserved = 100
)

// ---------------------------------------------------------------------------
// 请求网关内部参数（限流/重试/持久化）
// ---------------------------------------------------------------------------

const (
	// MaxRetries 429 限流时的最大重试次数
	MaxRetries = 5

	// RetryMinWait 429 重试的最小等待时间
	RetryMinWait = 2 * time.Second

	// ProbeInterval 每日配额熔断后的探测恢复间隔
	ProbeInterval = 5 * time.Minute

	// FlushInterval 每日计数批量刷库间隔
	FlushInterval = 30 * time.Second

	// EmbeddingBatchSize 单次向量化批量请求的上限条数
	EmbeddingBatchSize = 100
)

// ---------------------------------------------------------------------------
// 检索与业务参数
// ---------------------------------------------------------------------------

const (
	// MinSearchSimilarity RAG 检索的最低相关度，低于该值视为未命中
	MinSearchSimilarity = 0.45
)
