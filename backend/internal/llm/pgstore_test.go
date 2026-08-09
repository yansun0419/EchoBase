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
// 表结构升级为 (date, kind, priority) 三维联合主键，测试内重建，避免旧表冲突。
func TestPgUsageStoreIntegration(t *testing.T) {
	dsn := "postgres://postgres:lingxi2026@localhost:5432/echobase_db?sslmode=disable&connect_timeout=3"

	g, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Skipf("数据库不可用，跳过集成测试: %v", err)
	}

	// 重建三维主键表结构（本地开发库可能仍是旧的 (date,kind) 二维主键）
	if err := g.Exec(`DROP TABLE IF EXISTS api_quotas`).Error; err != nil {
		t.Skipf("无法重建测试表，跳过: %v", err)
	}
	if err := g.Exec(`CREATE TABLE api_quotas (
		date TEXT NOT NULL,
		kind TEXT NOT NULL,
		priority TEXT NOT NULL,
		usage_count INT NOT NULL DEFAULT 0,
		PRIMARY KEY (date, kind, priority)
	)`).Error; err != nil {
		t.Skipf("无法创建测试表，跳过: %v", err)
	}

	// 用隔离日期避免污染真实当日计数
	date := "test-" + time.Now().Format("2006-01-02-150405")
	store := &pgUsageStore{db: g}

	// 1. 首次 Save（插入）
	if err := store.Save(context.Background(), date, "generate", "high", 42); err != nil {
		t.Fatalf("首次 Save 失败: %v", err)
	}

	// 2. 覆盖 Save（upsert 更新）
	if err := store.Save(context.Background(), date, "generate", "high", 50); err != nil {
		t.Fatalf("二次 Save 失败: %v", err)
	}

	// 3. Load 应返回最新值
	got, err := store.Load(context.Background(), date, "generate", "high")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got != 50 {
		t.Fatalf("Load 应返回 50，得到 %d", got)
	}

	// 4. 不同维度互不影响（kind / priority 隔离验证）
	if err := store.Save(context.Background(), date, "generate", "low", 7); err != nil {
		t.Fatalf("generate|low Save 失败: %v", err)
	}
	if err := store.Save(context.Background(), date, "embed", "high", 5); err != nil {
		t.Fatalf("embed|high Save 失败: %v", err)
	}
	gotLow, _ := store.Load(context.Background(), date, "generate", "low")
	if gotLow != 7 {
		t.Fatalf("generate|low Load 应返回 7，得到 %d", gotLow)
	}
	gotHigh, _ := store.Load(context.Background(), date, "generate", "high")
	if gotHigh != 50 {
		t.Fatalf("generate|high Load 不应受 low 影响，应返回 50，得到 %d", gotHigh)
	}
	gotEmbed, _ := store.Load(context.Background(), date, "embed", "high")
	if gotEmbed != 5 {
		t.Fatalf("embed|high Load 应返回 5，得到 %d", gotEmbed)
	}

	// 5. 模拟进程重启：新实例从同一表恢复
	store2 := &pgUsageStore{db: g}
	got2, _ := store2.Load(context.Background(), date, "generate", "high")
	if got2 != 50 {
		t.Fatalf("重启恢复应得到 50，得到 %d", got2)
	}

	log.Printf("✅ pgUsageStore 集成测试通过（date=%s, gen|high=%d, gen|low=%d, embed|high=%d）",
		date, got2, gotLow, gotEmbed)

	// 清理测试数据
	g.Exec("DELETE FROM api_quotas WHERE date = ?", date)
}
