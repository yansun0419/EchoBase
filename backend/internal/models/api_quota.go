package models

// APIQuota 记录 Gemini API 每日用量，供限流网关熔断判断。
// 持久化到数据库而非本地文件：Render 等云平台容器文件系统是易失的，
// 每次休眠唤醒或重新部署都会重建容器，本地 JSON 计数会丢失导致限流失效。
//
// 文本生成与向量化按各自模型独立计费/限流，因此用 (date, kind) 联合主键
// 分别记录每日用量，kind 取值见 internal/llm/rate_limiter.go 的 kindKey()。
type APIQuota struct {
	Date       string `gorm:"primaryKey;size:10"` // 当天日期 YYYY-MM-DD
	Kind       string `gorm:"primaryKey;size:16"` // 请求种类：generate / embed
	UsageCount int    `gorm:"not null;default:0"` // 该种类当天已消耗的请求数
}

func (APIQuota) TableName() string { return "api_quotas" }
