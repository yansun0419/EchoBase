package models

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"
)

// UUIDArray 自定义 uuid[] 类型，实现 PostgreSQL 数组与 Go 切片之间的转换
type UUIDArray []uuid.UUID

func (a UUIDArray) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	elems := make([]string, len(a))
	for i, u := range a {
		elems[i] = u.String()
	}
	return "{" + strings.Join(elems, ",") + "}", nil
}

func (a *UUIDArray) Scan(value interface{}) error {
	if value == nil {
		*a = nil
		return nil
	}
	var s string
	switch v := value.(type) {
	case []byte:
		s = string(v)
	case string:
		s = v
	default:
		return fmt.Errorf("UUIDArray: 不支持的类型 %T", value)
	}
	s = strings.Trim(s, "{}")
	if s == "" {
		*a = UUIDArray{}
		return nil
	}
	parts := strings.Split(s, ",")
	arr := make(UUIDArray, len(parts))
	for i, p := range parts {
		u, err := uuid.Parse(p)
		if err != nil {
			return err
		}
		arr[i] = u
	}
	*a = arr
	return nil
}

// SemanticChunk 海星扁平网络：核心碎片向量表 (v0.2)
// 真正的知识图谱。通过哈希锁死冗余，通过外键数组实现多路复用，通过 Token 计数触发细胞分裂。
type SemanticChunk struct {
	ID uuid.UUID `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`

	// Content 最终切分好的、绝对不重复的知识切片
	Content string `gorm:"type:text;not null"`

	// Embedding 核心高维向量引擎
	Embedding *pgvector.Vector `gorm:"type:vector(3072)"`

	// SourceCommitIDs 外键数组：指向 data_commits 的 id。
	// 遇到重复语义触发合并时绝不新建行，只把新的来源 ID 追加进这个数组。
	SourceCommitIDs UUIDArray `gorm:"type:uuid[]"`

	// TokenCount 记录该切片的长度，后台轮询监控，一旦超载立刻触发 AI "细胞分裂"
	TokenCount int `gorm:"type:integer;default:0"`

	// Status 状态戳: active / deprecated / archived，实现软删除
	Status string `gorm:"type:varchar(20);default:'active'"`

	CreatedAt time.Time
	// UpdatedAt 每次触发合并或分裂时更新
	UpdatedAt time.Time
}

func (c *SemanticChunk) BeforeCreate(tx *gorm.DB) (err error) {
	c.ID = uuid.New()
	return
}
