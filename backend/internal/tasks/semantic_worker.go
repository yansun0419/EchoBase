package tasks

import (
	"log"
	"strings"
	"time"

	"echobase/internal/chunker"
	"echobase/internal/db"
	"echobase/internal/llm"
	"echobase/internal/models"

	"github.com/google/uuid"
)

// 语义合并相似度阈值：达到该值认为语义重复，直接复用已有知识块而非新建
const mergeSimilarityThreshold = 0.80

// retryBackoffs 记录每个 commit 因限流/临时错误失败后的下次可重试时间（进程内存），
// 避免 5 秒轮询立即重打 Gemini API 造成 429 忙循环。
var retryBackoffs = make(map[string]time.Time)

// scheduleRetry 把 commit 标记为失败并设置退避：不立即置回 pending，等待 backoff 后再重试。
func scheduleRetry(commit models.DataCommit) {
	backoff := 60 * time.Second
	retryBackoffs[commit.ID.String()] = time.Now().Add(backoff)
	db.DB.Model(&commit).Update("chunk_status", "failed")
	log.Printf("🔧 数据 [%s] 暂缓重试，%s 后重新排队\n", commit.ID, backoff)
}

// retryDue 检查 commit 是否已过退避期；未到期的直接跳过本轮轮询。
func retryDue(commit models.DataCommit) bool {
	due, ok := retryBackoffs[commit.ID.String()]
	if !ok {
		return true
	}
	return time.Now().After(due)
}

// StartSemanticWorker 启动语义切片引擎 (v0.2)
// 负责把洗稿完成的 processed_content 切成知识切片，写入 semantic_chunks。
// 核心逻辑：高相似度触发"多路复用"（只追加来源 ID，绝不新建行）。
func StartSemanticWorker() {
	log.Println("🧬 语义切片引擎已启动，开始轮询已完成洗稿的数据...")
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for range ticker.C {
			processNextCommitForChunking()
		}
	}()
}

func processNextCommitForChunking() {
	// 遍历候选 commit，跳过仍在退避期的，找到第一个到期的处理
	var candidates []models.DataCommit
	db.DB.Where("status = ? AND chunk_status IN ?", "completed", []string{"pending", "failed"}).Order("created_at asc").Find(&candidates)
	for _, commit := range candidates {
		if !retryDue(commit) {
			continue
		}
		processCommitChunking(commit)
		// 处理完一个后冷却，避免连续提交瞬间耗尽 Gemini 免费档配额
		time.Sleep(20 * time.Second)
		return
	}
}

func processCommitChunking(commit models.DataCommit) {
	log.Printf("🧬 开始为数据 [%s] 做语义切片...\n", commit.ID)

	// 1. 物理切分：逐句向量断崖法，纯数学控制块体积（短块整过，长块切句找断崖）
	chunks, err := chunker.Split(commit.ProcessedContent, llm.GenerateEmbeddingsBatch)
	if err != nil {
		log.Printf("❌ 语义切分失败 [%s]: %v\n", commit.ID, err)
		db.DB.Model(&commit).Update("chunk_status", "failed")
		return
	}
	if len(chunks) == 0 {
		db.DB.Model(&commit).Update("chunk_status", "done")
		return
	}

	// 2. AI 补齐上下文（Contextual Retrieval）：切分靠数学，背景靠 AI
	for i := range chunks {
		preview := string([]rune(chunks[i].Content))
		if len([]rune(preview)) > 40 {
			preview = string([]rune(preview)[:40]) + "..."
		}
		log.Printf("📦 物理块[%d] token=%d | %s\n", i, chunks[i].TokenCount, preview)
		// AI 只产出前置背景句，绝不改写区块本体：背景拼在开头，正文原样保留。
		background, err := llm.Contextualize(chunks[i].Content, commit.ProcessedContent)
		if err != nil {
			// 背景句失败不影响主流程：直接使用原始切片继续（补齐只是锦上添花）
			log.Printf("⚠️ 上下文补齐失败 [%s]: %v，使用原始切片\n", commit.ID, err)
			continue
		}
		background = strings.TrimSpace(background)
		if background == "" || background == "无" {
			continue
		}
		chunks[i].Content = background + "\n" + chunks[i].Content
		chunks[i].TokenCount = chunker.TokenCount(chunks[i].Content)
	}

	for _, ch := range chunks {
		// 3. 计算补齐后区块的高维向量
		vec, err := llm.GenerateEmbedding(ch.Content)
		if err != nil {
			// 向量生成失败（重试后仍失败）→ 不能静默丢块：进入退避队列，
			// 等待限流恢复后重新处理，保证内容绝不丢失。
			log.Printf("❌ 切片向量生成失败 [%s]: %v\n", commit.ID, err)
			scheduleRetry(commit)
			return
		}

		// 3. 在现有 active 知识块中检索最相似的
		var hit struct {
			ID         uuid.UUID
			Similarity float64
		}
		db.DB.Raw(`
			SELECT id, 1 - (embedding <=> ?) AS similarity
			FROM semantic_chunks
			WHERE status = 'active'
			ORDER BY embedding <=> ?
			LIMIT 1`, vec, vec).Scan(&hit)

		// 4. 语义重复 → 多路复用：只把新的来源 ID 追加进 source_commit_ids，绝不新建行
		if hit.Similarity >= mergeSimilarityThreshold {
			// 命中块若已收录该来源，说明是同文档的另一个切片被 AI 补齐成近似文本
			// （如"数据采集"与"建模"两段被润色成相似句式）。此时合并会把本切片内容
			// 整个丢弃，造成知识丢失 → 禁止合并，改走下方新建。
			var alreadySource bool
			db.DB.Raw(`SELECT ? = ANY(source_commit_ids) FROM semantic_chunks WHERE id = ?`, commit.ID, hit.ID).Scan(&alreadySource)
			if alreadySource {
				log.Printf("⚠️ 切片与同源块 [%s] 相似度 %.2f，禁止合并，改为新建\n", hit.ID, hit.Similarity)
			} else {
				err := db.DB.Exec(`
					UPDATE semantic_chunks
					SET source_commit_ids = array_append(source_commit_ids, ?),
						updated_at = now()
					WHERE id = ?`, commit.ID, hit.ID).Error
				if err != nil {
					log.Printf("❌ 追加来源失败: %v\n", err)
					continue
				}
				log.Printf("🔁 切片与知识块 [%s] 相似度 %.2f，已复用并追加来源\n", hit.ID, hit.Similarity)
				continue
			}
		}

		// 5. 全新知识 → 新建知识块
		chunk := models.SemanticChunk{
			Content:         ch.Content,
			Embedding:       vec,
			SourceCommitIDs: models.UUIDArray{commit.ID},
			TokenCount:      ch.TokenCount,
			Status:          "active",
		}
		if err := db.DB.Create(&chunk).Error; err != nil {
			log.Printf("❌ 新建知识块失败: %v\n", err)
			continue
		}
		log.Printf("🆕 已新建知识块 [%s]，token=%d\n", chunk.ID, chunk.TokenCount)
	}

	// 6. 全部切片处理完毕，标记完成
	db.DB.Model(&commit).Update("chunk_status", "done")
	log.Printf("✅ 数据 [%s] 语义切片完成！\n", commit.ID)
}
