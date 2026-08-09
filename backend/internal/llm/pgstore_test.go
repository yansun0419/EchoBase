package llm

import (
	"context"
	"log"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 真实数据库集成测试：验证 pgUsageStore 的 upsert 与重启恢复。
// 用本地开发库（echobase-pgvector 容器）；数据库不可用时跳过。
func TestPgUsageStoreIntegration(t *testing.T) {
	dsn := "postgres://postgres:lingxi2026@localhost:5432/echobase_db?sslmode=disable&connect_timeout=3"

	g, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Skipf("数据库不可用，跳过集成测试: %v", err)
	}

	// 确保 api_quotas 表存在（本地开发库可能尚未跑过迁移）
	if err := g.Exec(`CREATE TABLE IF NOT EXISTS api_quotas (
		date TEXT NOT NULL,
		kind TEXT NOT NULL,
		usage_count INT NOT NULL DEFAULT 0,
		PRIMARY KEY (date, kind)
	)`).Error; err != nil {
		t.Skipf("无法创建测试表，跳过: %v", err)
	}

	// 用隔离日期避免污染真实当日计数
	date := "test-" + time.Now().Format("2006-01-02-150405")
	store := &pgUsageStore{db: g}

	// 1. 首次 Save（插入）
	if err := store.Save(context.Background(), date, "generate", 42); err != nil {
		t.Fatalf("首次 Save 失败: %v", err)
	}

	// 2. 覆盖 Save（upsert 更新）
	if err := store.Save(context.Background(), date, "generate", 50); err != nil {
		t.Fatalf("二次 Save 失败: %v", err)
	}

	// 3. Load 应返回最新值
	got, err := store.Load(context.Background(), date, "generate")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got != 50 {
		t.Fatalf("Load 应返回 50，得到 %d", got)
	}

	// 4. 不同 kind 互不影响（隔离验证）
	if err := store.Save(context.Background(), date, "embed", 7); err != nil {
		t.Fatalf("embed Save 失败: %v", err)
	}
	gotEmbed, _ := store.Load(context.Background(), date, "embed")
	if gotEmbed != 7 {
		t.Fatalf("embed Load 应返回 7，得到 %d", gotEmbed)
	}
	gotGen, _ := store.Load(context.Background(), date, "generate")
	if gotGen != 50 {
		t.Fatalf("generate Load 不应受 embed 影响，应返回 50，得到 %d", gotGen)
	}

	// 5. 模拟进程重启：新实例从同一表恢复
	store2 := &pgUsageStore{db: g}
	got2, _ := store2.Load(context.Background(), date, "generate")
	if got2 != 50 {
		t.Fatalf("重启恢复应得到 50，得到 %d", got2)
	}

	log.Printf("✅ pgUsageStore 集成测试通过（date=%s, generate=%d, embed=%d）", date, got2, gotEmbed)

	// 清理测试数据
	g.Exec("DELETE FROM api_quotas WHERE date = ?", date)
}
