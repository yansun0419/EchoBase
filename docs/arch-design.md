# EchoBase (灵犀) - 架构与数据库设计文档

## 1. 系统架构图 (简化版)

[浏览器扩展/采集端] ---> (HTTP API) ---> [Go 后端服务]
                                            |
                                            |--> 写入原始数据 ---> [PostgreSQL (持久化存取)]
                                            |
                                            |---> (定时/队列消费)
                                                    |
                                                    |---> 调用 [大模型 API] 进行信息蒸馏、打标签
                                                    |---> 生成 Embedding 向量 ---> 写入 [Pinecone 向量数据库]
                                                    |
                                                    |---> 更新 [PostgreSQL] 中的状态和 AI 摘要

[用户查询] ---> [Go 后端检索接口] ---> 并行查询 [Pinecone] & [PostgreSQL] ---> 组合上下文 ---> [大模型 API] ---> [最终回答]

## 2. 核心数据库表结构 (PostgreSQL)

**表名：`documents`**
*   `id` (UUID): 主键，唯一标识符。
*   `title` (VARCHAR): 文档或资源标题。
*   `content` (TEXT): 完整的原始文本数据。
*   `source_url` (VARCHAR): 来源网址，用于精确溯源。
*   `source_type` (VARCHAR): 来源类型（bilibili, article, document 等）。
*   `process_status` (VARCHAR): 状态机标记（pending, processing, completed, failed），用于异步处理的游标控制。
*   `ai_summary` (TEXT): 经过 AI 提炼后的核心摘要。
*   `tags` (VARCHAR[]): AI 自动提取并归类的标签数组。
*   `created_at` (TIMESTAMP): 创建时间。
*   `updated_at` (TIMESTAMP): 更新时间。