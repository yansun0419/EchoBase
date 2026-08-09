package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"echobase/internal/config"
	"echobase/internal/db"
	"echobase/internal/llm"
	"echobase/internal/models"
	"echobase/internal/tasks"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"gorm.io/gorm"
)

// DocumentRequest 定义了前端传过来的 JSON 数据结构
type DocumentRequest struct {
	Title      string `json:"title" binding:"required"` // binding:"required" 表示这个字段必填
	Content    string `json:"content" binding:"required"`
	SourceURL  string `json:"source_url"`
	SourceType string `json:"source_type"`
}

func main() {
	log.Println("--- EchoBase (灵犀) 后端引擎启动 ---")

	// 0. 加载环境变量
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️ 警告: 无法加载 .env 文件，确保环境变量已正确设置。")
	}

	// 1. 从环境变量读取数据库连接字符串 (Supabase/Render 部署时自动注入 DATABASE_URL)
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("❌ 致命错误: 未设置 DATABASE_URL 环境变量")
	}
	db.InitDB(dsn)

	// 将 Gemini 每日配额持久化到数据库（而非本地文件）。
	// Render 等云平台容器文件系统易失，重启后本地 JSON 计数会丢失导致限流失效。
	llm.InitUsageStore(db.DB)

	// add worker
	tasks.StartWorker()

	// v0.2 洗稿流水线 worker
	tasks.StartCommitWorker()

	// v0.2 语义切片 worker + 细胞分裂 worker
	tasks.StartSemanticWorker()
	tasks.StartSplitWorker()

	// 2. 初始化 Gin 引擎
	r := gin.Default()

	// 3. 配置 CORS 中间件：通过环境变量控制允许的前端域名
	allowedOrigin := os.Getenv("CORS_ORIGIN")
	if allowedOrigin == "" {
		allowedOrigin = "*" // 本地开发时默认允许所有
	}
	r.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// 4. 路由定义
	// 存活测试接口
	r.GET("/api/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "EchoBase 引擎通过 Gin 框架全速运行中！"})
	})

	// 核心业务：接收前端发来的文档数据
	r.POST("/api/documents", func(c *gin.Context) {
		var req DocumentRequest

		// 解析并校验 JSON 数据
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "数据格式错误，请检查 title 和 content 是否为空: " + err.Error()})
			return
		}

		// 将接收到的数据装载到数据库模型中
		doc := models.Document{
			Title:      req.Title,
			Content:    req.Content,
			SourceURL:  req.SourceURL,
			SourceType: req.SourceType,
			// ProcessStatus 默认就是 'pending'，无需特别指定
		}

		// 使用 GORM 将数据插入 PostgreSQL
		if result := db.DB.Create(&doc); result.Error != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库保存失败: " + result.Error.Error()})
			return
		}

		// 返回成功响应
		c.JSON(http.StatusOK, gin.H{
			"message":     "数据已成功接收并进入缓冲池等待 AI 处理",
			"document_id": doc.ID,
		})
	})

	// v0.2 去重闸门：数据摄入与洗稿总控表的物理大门
	// 接收前端传来的原始文本 -> SHA-256 哈希 -> 写入 data_commits，UNIQUE 约束拦截重复
	r.POST("/api/commits", func(c *gin.Context) {
		var req struct {
			RawContent string `json:"raw_content" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "数据格式错误，raw_content 不能为空: " + err.Error()})
			return
		}

		// 1. 计算 SHA-256 哈希，作为内容去重指纹
		sum := sha256.Sum256([]byte(req.RawContent))
		inputHash := hex.EncodeToString(sum[:])

		// 2. 写入 data_commits，Status 默认 'pending' 进入洗稿队列
		commit := models.DataCommit{
			RawContent: req.RawContent,
			InputHash:  inputHash,
		}
		result := db.DB.Create(&commit)

		// 3. 利用 UNIQUE 约束拦截重复提交
		if errors.Is(result.Error, gorm.ErrDuplicatedKey) {
			c.JSON(http.StatusConflict, gin.H{
				"message":    "检测到重复内容，已在入口拦截",
				"duplicated": true,
				"input_hash": inputHash,
			})
			return
		}
		if result.Error != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "数据库写入失败: " + result.Error.Error()})
			return
		}

		// 4. 通过闸门，进入洗稿流水线
		c.JSON(http.StatusOK, gin.H{
			"message":    "数据已通过去重闸门，进入洗稿流水线",
			"commit_id":  commit.ID,
			"input_hash": inputHash,
			"status":     commit.Status,
		})
	})

	// 定义搜索请求的结构体
	type SearchRequest struct {
		Query string `json:"query" binding:"required"`
	}

	// 核心业务：RAG 向量检索与智能问答 API
	r.POST("/api/search", func(c *gin.Context) {
		var req SearchRequest
		// 1. 接收前端传来的搜索提问
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请提供查询关键词(query)"})
			return
		}

		// 2. 唤醒 Embedding 2 模型，将提问转化为 3072 维向量
		//    （在线请求按高优先级排队，不被后台批量向量化堵住）
		queryVector, err := llm.GenerateQueryEmbedding(req.Query)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "搜索词向量化失败: " + err.Error()})
			return
		}

		// 3. 核心：在 pgvector 中执行高维余弦相似度检索
		var results []struct {
			ID         string  `json:"id"`
			Title      string  `json:"title"`
			AISummary  string  `json:"ai_summary"`
			Content    string  `json:"content"` // 把原文也捞出来给 AI 看
			Similarity float64 `json:"similarity"`
		}

		// 执行 SQL：按余弦相似度倒序，取最相关的前 3 篇
		db.DB.Raw(`
			SELECT 
				id, 
				title, 
				ai_summary, 
				content,
				1 - (embedding <=> ?) AS similarity 
			FROM documents 
			WHERE process_status = 'completed'
			ORDER BY embedding <=> ? 
			LIMIT 3
		`, queryVector, queryVector).Scan(&results)

		// 4. 【新增】RAG 增强生成阶段！
		// 如果搜不到任何内容，直接婉拒
		if len(results) == 0 || results[0].Similarity < config.MinSearchSimilarity {
			c.JSON(http.StatusOK, gin.H{
				"query":   req.Query,
				"answer":  "抱歉，在您的知识库中没有找到与此问题强相关的内容哦。",
				"results": []string{},
			})
			return
		}

		// 把搜到的相关文章内容拼接到一起，作为 AI 的“参考资料”
		var contextContext strings.Builder
		for i, res := range results {
			if res.Similarity > config.MinSearchSimilarity { // 过滤掉相关性极低的噪声
				contextContext.WriteString(fmt.Sprintf("参考资料 %d: [%s]\n%s\n\n", i+1, res.Title, res.Content))
			}
		}

		// 召唤 Gemini 3.1 Lite，让它根据参考资料回答用户提问
		ragAnswer, err := llm.GenerateRAGAnswer(req.Query, contextContext.String())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "RAG 回答生成失败: " + err.Error()})
			return
		}

		// 5. 将 AI 的完美回答以及参考源返回给前端
		c.JSON(http.StatusOK, gin.H{
			"query":   req.Query,
			"answer":  ragAnswer,
			"results": results, // 附带检索出来的文章摘要供前端展示
		})
	})

	// 5. 启动服务器（Render 等云平台通过 PORT 环境变量注入端口）
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // 本地开发默认端口
	}
	log.Printf("⚡ API 网关已就绪，正在监听 :%s 端口...\n", port)
	r.Run(":" + port)
}
