package tasks

import (
	"errors"
	"log"
	"time"

	"echobase/internal/chunker"
	"echobase/internal/db"
	"echobase/internal/llm"
	"echobase/internal/models"

	"gorm.io/gorm"
)

// 触发细胞分裂的 token 上限：知识块超过该值即视为"超载"
const chunkSplitTokenThreshold = 1500

// StartSplitWorker 启动细胞分裂守护进程 (v0.2)
// 后台轮询监控 token_count 超载的知识块，拆成多个更小、语义更聚焦的子块。
func StartSplitWorker() {
	log.Println("🔬 细胞分裂引擎已启动，开始轮询超载知识块...")
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for range ticker.C {
			processOverloadedChunks()
		}
	}()
}

func processOverloadedChunks() {
	var chunks []models.SemanticChunk
	result := db.DB.Where("status = ? AND token_count > ?", "active", chunkSplitTokenThreshold).Find(&chunks)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return
		}
		log.Printf("❌ 数据库查询异常: %v\n", result.Error)
		return
	}

	for _, chunk := range chunks {
		splitChunk(chunk)
	}
}

// splitChunk 把一个超载知识块分裂为若干子块（AI 语义分裂，保证子块独立存活）
func splitChunk(chunk models.SemanticChunk) {
	log.Printf("🔬 知识块 [%s] token=%d 超载，触发细胞分裂...\n", chunk.ID, chunk.TokenCount)

	// 物理切分：逐句向量断崖法，数学控制子块体积
	subChunks, err := chunker.Split(chunk.Content, llm.GenerateEmbeddingsBatch)
	if err != nil {
		log.Printf("❌ 细胞分裂失败 [%s]: %v\n", chunk.ID, err)
		return
	}
	if len(subChunks) <= 1 {
		// 拆不出多于一块就不拆，避免死循环
		log.Printf("⏭️ 知识块 [%s] 无法有效拆分，跳过\n", chunk.ID)
		return
	}

	// 1. 老块软删除：deprecated 标记，不再参与检索（细胞分裂的"母体死亡"）
	err = db.DB.Model(&chunk).Updates(map[string]interface{}{
		"status":     "deprecated",
		"updated_at": time.Now(),
	}).Error
	if err != nil {
		log.Printf("❌ 老块状态更新失败: %v\n", err)
		return
	}

	// 2. 子块继承全部来源 ID，新建为 active（"后代继承血缘"）
	for _, sub := range subChunks {
		vec, err := llm.GenerateEmbedding(sub.Content)
		if err != nil {
			log.Printf("❌ 子块向量生成失败: %v\n", err)
			continue
		}
		newChunk := models.SemanticChunk{
			Content:         sub.Content,
			Embedding:       vec,
			SourceCommitIDs: chunk.SourceCommitIDs,
			TokenCount:      sub.TokenCount,
			Status:          "active",
		}
		if err := db.DB.Create(&newChunk).Error; err != nil {
			log.Printf("❌ 新建子块失败: %v\n", err)
			continue
		}
		log.Printf("🧬 子块 [%s] 诞生，token=%d\n", newChunk.ID, newChunk.TokenCount)
	}

	log.Printf("✅ 知识块 [%s] 分裂完成！\n", chunk.ID)
}
