package tasks

import (
	"errors"
	"log"
	"strings"
	"time"

	"echobase/internal/db"
	"echobase/internal/llm" // 引入咱们刚刚写的 AI 核心模块
	"echobase/internal/models"

	"github.com/lib/pq"
	"gorm.io/gorm"
)

// StartWorker 启动后台轮询守护进程
func StartWorker() {
	log.Println("⚙️ AI 后台处理引擎已启动，开始轮询缓冲池...")

	// 保持 5 秒轮询一次的平滑节奏，完美卡在 Google 免费额度（15 RPM）安全线内
	ticker := time.NewTicker(5 * time.Second)

	go func() {
		for range ticker.C {
			processNextDocument()
		}
	}()
}

func processNextDocument() {
	var doc models.Document

	// 1. 捞取数据：查找第一条待处理的记录
	result := db.DB.Where("process_status = ?", "pending").First(&doc)
	if result.Error != nil {
		// 如果没有待处理记录，直接返回
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return
		}
		log.Printf("❌ 数据库查询异常: %v\n", result.Error)
		return
	}

	log.Printf("📥 发现待处理文档: [%s] %s\n", doc.ID, doc.Title)

	// 2. 状态锁定：防止并发冲突
	db.DB.Model(&doc).Update("process_status", "processing")
	log.Println("🔒 状态已锁定为 processing，开始唤醒 Gemini 脑核...")

	// 3. 核心：调用真实大模型生成摘要和标签
	summary, tags, err := llm.GenerateSummaryAndTags(doc.Content)
	if err != nil {
		log.Printf("❌ Gemini 摘要提炼失败 [%s]: %v\n", doc.ID, err)
		db.DB.Model(&doc).Update("process_status", "failed")
		return
	}

	// 4. 核心：调用向量模型生成 3072 维 Embedding 向量
	embedding, err := llm.GenerateEmbedding(doc.Content)
	if err != nil {
		log.Printf("❌ Gemini 向量构建失败 [%s]: %v\n", doc.ID, err)
		db.DB.Model(&doc).Update("process_status", "failed")
		return
	}

	// 【工程清洗】处理大模型返回的标签字符串，将其转化为 Postgres 认识的字符串数组
	tagsStr := strings.TrimSpace(tags)
	tagsStr = strings.TrimPrefix(tagsStr, "[")
	tagsStr = strings.TrimSuffix(tagsStr, "]")
	var tagsArray pq.StringArray
	if tagsStr != "" {
		for _, t := range strings.Split(tagsStr, ",") {
			tagsArray = append(tagsArray, strings.TrimSpace(t))
		}
	}

	// 5. 数据归档：将摘要、标签、高维向量一次性写入 PostgreSQL，状态切为 completed
	err = db.DB.Model(&doc).Updates(map[string]interface{}{
		"process_status": "completed",
		"ai_summary":     strings.TrimSpace(summary),
		"tags":           tagsArray,
		"embedding":      embedding,
	}).Error

	if err != nil {
		log.Printf("❌ 归档数据库失败 [%s]: %v\n", doc.ID, err)
		db.DB.Model(&doc).Update("process_status", "failed")
		return
	}

	log.Printf("✅ 文档 [%s] 处理完成，AI 摘要及 3072 维向量完美落盘！\n", doc.Title)
}

// StartCommitWorker 启动 data_commits 洗稿流水线的后台守护进程 (v0.2)
func StartCommitWorker() {
	log.Println("🧼 洗稿流水线引擎已启动，开始轮询 data_commits 缓冲池...")

	// 保持与文档 worker 相同的 5 秒轮询节奏
	ticker := time.NewTicker(5 * time.Second)

	go func() {
		for range ticker.C {
			processNextCommit()
		}
	}()
}

func processNextCommit() {
	var commit models.DataCommit

	// 1. 捞取数据：查找第一条待洗稿的记录
	result := db.DB.Where("status = ?", "pending").First(&commit)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return
		}
		log.Printf("❌ 数据库查询异常: %v\n", result.Error)
		return
	}

	log.Printf("📥 发现待洗稿数据: %s\n", commit.ID)

	// 2. 状态锁定：pending -> washing，防止并发重复处理
	db.DB.Model(&commit).Update("status", "washing")
	log.Println("🔒 状态已锁定为 washing，开始唤醒 Gemini 脑核洗稿...")

	// 3. 核心：调用大模型清洗 raw_content -> processed_content
	processedContent, err := llm.WashContent(commit.RawContent)
	if err != nil {
		log.Printf("❌ AI 洗稿失败 [%s]: %v\n", commit.ID, err)
		db.DB.Model(&commit).Update("status", "failed")
		return
	}

	// 4. 核心：对洗稿后的纯净文本生成 3072 维 Embedding 向量
	embedding, err := llm.GenerateEmbedding(processedContent)
	if err != nil {
		log.Printf("❌ Gemini 向量构建失败 [%s]: %v\n", commit.ID, err)
		db.DB.Model(&commit).Update("status", "failed")
		return
	}

	// 5. 数据归档：洗稿文本 + 向量一次性落盘，状态切为 completed
	err = db.DB.Model(&commit).Updates(map[string]interface{}{
		"status":            "completed",
		"processed_content": processedContent,
		"embedding":         embedding,
	}).Error
	if err != nil {
		log.Printf("❌ 归档数据库失败 [%s]: %v\n", commit.ID, err)
		db.DB.Model(&commit).Update("status", "failed")
		return
	}

	log.Printf("✅ 数据 [%s] 洗稿完成，纯净文本及 3072 维向量已落盘！\n", commit.ID)
}
