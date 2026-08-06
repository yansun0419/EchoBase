package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"
)

// DataCommit 数据摄入与洗稿总控表 (v0.2)
// 记录每次输入的原始数据流，负责拦截重复请求，并存储 AI 清洗后的标准化文本。
type DataCommit struct {
	ID uuid.UUID `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`

	// RawContent 原始输入内容，原封不动保留，作为溯源底稿
	RawContent string `gorm:"type:text;not null"`

	// InputHash 对 raw_content 的哈希值，UNIQUE 约束，在入口拦截重复提交
	InputHash string `gorm:"type:varchar(64);uniqueIndex;not null"`

	// ProcessedContent 经 AI 统一风格洗稿、排版后的纯净数据
	ProcessedContent string `gorm:"type:text"`

	// Embedding processed_content 的向量，洗稿完成后直接算好存起来
	Embedding *pgvector.Vector `gorm:"type:vector(3072)"`

	// Status 处理状态标记: pending -> washing -> completed / failed
	Status string `gorm:"type:varchar(20);default:'pending'"`

	CreatedAt time.Time
}

func (c *DataCommit) BeforeCreate(tx *gorm.DB) (err error) {
	c.ID = uuid.New()
	return
}
