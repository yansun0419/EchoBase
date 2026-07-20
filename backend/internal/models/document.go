package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/pgvector/pgvector-go"
	"gorm.io/gorm"
)

type Document struct {
	ID            uuid.UUID      `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`
	Title         string         `gorm:"type:varchar(255);not null"`
	Content       string         `gorm:"type:text;not null"`
	SourceURL     string         `gorm:"type:varchar(512)"`
	SourceType    string         `gorm:"type:varchar(50)"`
	ProcessStatus string         `gorm:"type:varchar(20);default:'pending'"`
	AISummary     string         `gorm:"type:text"`
	Tags          pq.StringArray `gorm:"type:varchar(255)[]"`

	Embedding *pgvector.Vector `gorm:"type:vector(3072)"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

func (doc *Document) BeforeCreate(tx *gorm.DB) (err error) {
	doc.ID = uuid.New()
	return
}

type Raw struct {
	ID uuid.UUID `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`
	// Title         string         `gorm:"type:varchar(255);not null"`
	Content       string         `gorm:"type:text;not null"`
	SourceURL     string         `gorm:"type:varchar(512)"`
	SourceType    string         `gorm:"type:varchar(50)"`
	ProcessStatus string         `gorm:"type:varchar(20);default:'pending'"`
	AISummary     string         `gorm:"type:text"`
	Tags          pq.StringArray `gorm:"type:varchar(255)[]"`

	Embedding *pgvector.Vector `gorm:"type:vector(3072)"`

	CreatedAt time.Time
	UpdatedAt time.Time
}
