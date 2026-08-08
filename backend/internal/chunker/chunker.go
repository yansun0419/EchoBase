package chunker

import (
	"math"
	"strings"
)

// 触发 AI 切分的 token 阈值（触发结界）：
// 洗稿后体积小于该值，直接作为完整一块向量化，不折腾
const SplitThreshold = 800

// 组块的目标体积区间（token）
const minChunkTokens = 300
const maxChunkTokens = 600

// 断崖判定阈值：相邻两句余弦相似度低于该值，视为话题切换点
const similarityBreakpoint = 0.60

// Chunk 一个知识切片
type Chunk struct {
	Content    string
	TokenCount int
}

// TokenCount 估算 token 数（中文 1 字 ≈ 1 token）
func TokenCount(s string) int {
	return len([]rune(s))
}

// Embedder 批量计算文本向量的函数签名
type Embedder func(texts []string) ([][]float32, error)

// Split 纯数学向量切分入口：
// 1. 触发结界：短文本（<= 阈值）直接作为完整一块。
// 2. 逐句向量断崖法：切句 -> 批量向量 -> 找话题断崖 -> 按 300-600 token 组块。
// 任何一步失败都降级为整块返回，保证数据绝不丢失。
func Split(content string, embed Embedder) ([]Chunk, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, nil
	}

	tokens := TokenCount(content)

	// ① 触发结界：没超标，直接整块过
	if tokens <= SplitThreshold {
		return []Chunk{{Content: content, TokenCount: tokens}}, nil
	}

	// ② 切句
	sentences := SplitBySentences(content)
	if len(sentences) <= 1 {
		return []Chunk{{Content: content, TokenCount: tokens}}, nil
	}

	// ③ 逐句批量向量
	vectors, err := embed(sentences)
	if err != nil || len(vectors) != len(sentences) {
		// 向量化失败（限流/网络）时不得把整块交给后续 AI 补齐——那会被 AI 压缩丢内容。
		// 降级为纯 token 机械切分，保证每块体积不超上限、内容零丢失。
		return splitByTokens(content), nil
	}

	// ④ 断崖检测 + 组块
	breaks := findBreakpoints(vectors)
	return assembleChunks(sentences, breaks), nil
}

// splitByTokens 纯 token 机械切分：向量化失败时的兜底，按 maxChunkTokens 上限硬切，
// 保证每块体积有界、内容零丢失（不用句子标点，避免丢句）。
func splitByTokens(content string) []Chunk {
	runes := []rune(content)
	var chunks []Chunk
	for start := 0; start < len(runes); start += maxChunkTokens {
		end := start + maxChunkTokens
		if end > len(runes) {
			end = len(runes)
		}
		text := strings.TrimSpace(string(runes[start:end]))
		if text != "" {
			chunks = append(chunks, Chunk{Content: text, TokenCount: len([]rune(text))})
		}
	}
	return chunks
}

// SplitBySentences 把文本按句子标点切成句子
func SplitBySentences(text string) []string {
	var sentences []string
	var buf strings.Builder
	for _, r := range text {
		buf.WriteRune(r)
		if strings.ContainsRune("。！？；.!?;", r) {
			if s := strings.TrimSpace(buf.String()); s != "" {
				sentences = append(sentences, s)
			}
			buf.Reset()
		}
	}
	if rest := strings.TrimSpace(buf.String()); rest != "" {
		sentences = append(sentences, rest)
	}
	return sentences
}

// findBreakpoints 计算相邻句子余弦相似度，标记"话题断崖"位置
// 返回一个布尔数组，breaks[i]=true 表示句子 i 与 i+1 之间话题切换
func findBreakpoints(vectors [][]float32) []bool {
	breaks := make([]bool, len(vectors)-1)
	for i := 0; i < len(vectors)-1; i++ {
		sim := cosineSimilarity(vectors[i], vectors[i+1])
		if sim < similarityBreakpoint {
			breaks[i] = true
		}
	}
	return breaks
}

// assembleChunks 按断崖 + 目标体积组块
func assembleChunks(sentences []string, breaks []bool) []Chunk {
	var chunks []Chunk
	var buf strings.Builder
	bufTokens := 0

	flush := func() {
		text := strings.TrimSpace(buf.String())
		if text != "" {
			chunks = append(chunks, Chunk{Content: text, TokenCount: TokenCount(text)})
		}
		buf.Reset()
		bufTokens = 0
	}

	for i, s := range sentences {
		st := TokenCount(s)

		// 当前块已达标，且这句会超上限 -> 必须切
		if bufTokens >= minChunkTokens && bufTokens+st > maxChunkTokens {
			flush()
		}
		// 当前块已达标，且下一句话题切换 -> 顺势切
		if bufTokens >= minChunkTokens && i < len(breaks) && breaks[i] {
			flush()
		}

		buf.WriteString(s)
		buf.WriteString("\n")
		bufTokens += st
	}
	flush()

	return chunks
}

// cosineSimilarity 计算两个向量的余弦相似度
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
