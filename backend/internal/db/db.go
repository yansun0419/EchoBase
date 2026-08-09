package db

import (
	"log"
	"os"
	"time"

	"echobase/internal/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

func InitDB(dsn string) {
	newLogger := logger.New(
		log.New(os.Stdout, "\r\n", log.LstdFlags), // io writer
		logger.Config{
			SlowThreshold:             time.Second,  // 慢 SQL 阈值
			LogLevel:                  logger.Error, // 仅打印 Error 级别的 SQL 日志
			IgnoreRecordNotFoundError: true,         // 🌟 核心：彻底忽略 "record not found" 刷屏！
			Colorful:                  true,         // 保持彩色打印
		},
	)

	var err error
	// TranslateError: true 让 GORM 把数据库错误翻译成 gorm.ErrDuplicatedKey 等标准错误，
	// 这样上层才能用 errors.Is 精确判断"唯一约束冲突"（去重闸门的核心依赖）
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:         newLogger,
		TranslateError: true,
	})
	if err != nil {
		log.Fatalf("无法连接到数据库: %v", err)
	}

	log.Println("数据库连接成功，正在校验 pgvector 扩展...")

	// 确保数据库层面开启了 vector 扩展
	DB.Exec("CREATE EXTENSION IF NOT EXISTS vector;")

	log.Println("正在执行自动迁移...")

	err = DB.AutoMigrate(
		&models.Document{},
		&models.DataCommit{},
		&models.SemanticChunk{},
		&models.APIQuota{},
	)
	if err != nil {
		log.Fatalf("数据库迁移失败: %v", err)
	}

	log.Println("EchoBase 数据库表结构及向量引擎同步完成！")
}
