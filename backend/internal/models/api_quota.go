package models

// APIQuota 记录 Gemini API 每日用量，供限流网关熔断判断。
// 持久化到数据库而非本地文件：Render 等云平台容器文件系统是易失的，
// 每次休眠唤醒或重新部署都会重建容器，本地 JSON 计数会丢失导致限流失效。
//
// 文本生成与向量化按各自模型独立计费/限流，因此用 (date, kind) 联合主键
// 分别记录每日用量；在线请求（high）与后台任务（low）配额隔离，
// 再叠加 priority 维度，最终为 (date, kind, priority) 三维联合主键。
// kind / priority 取值见 internal/llm 的 kindKey() / priorityKey()。
type APIQuota struct {
	Date       string `gorm:"primaryKey;size:10"` // 当天日期 YYYY-MM-DD
	Kind       string `gorm:"primaryKey;size:16"` // 请求种类：generate / embed
	Priority   string `gorm:"primaryKey;size:8"`  // 请求优先级：high（在线）/ low（后台）
	UsageCount int    `gorm:"not null;default:0"` // 该维度当天已消耗的请求数
}

func (APIQuota) TableName() string { return "api_quotas" }
