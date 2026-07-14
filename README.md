# EchoBase (灵犀) 🌌

> 一个原生基于向量空间（Vector-Native）、具备自生长能力的个人专属 AI 外脑智能体。

EchoBase 不是一个传统的笔记软件，也不是简单的 LangChain 套壳应用。它是一个**完全手写底层逻辑**的 RAG（检索增强生成）系统。抛弃了传统树状分类与繁重的抽象框架，EchoBase 将算法解析、项目架构笔记、甚至传统乐器与武术的心得沉淀，全部映射到 3072 维的纯粹向量星海中。

目前的 **v0.1** 版本已经打通了全栈高并发检索与大模型生成的闭环。

---

## 🛠 技术栈 (Tech Stack)

### Backend (核心引擎)

- **Language:** Go (原生高性能并发)
- **Framework:** Gin (RESTful API & 网关)
- **Database:** PostgreSQL + `pgvector` 扩展
- **Architecture:** 单表状态机异步 Worker (Goroutines & Channels 完美抵抗 API 阻塞)

### Frontend (视觉呈现)

- **Framework:** Next.js 16 (App Router) + React
- **Styling:** Tailwind CSS (深色渐变 + Glassmorphism 毛玻璃质感)
- **Language:** TypeScript

### AI / LLM (数字大脑)

- **Reasoning:** Google Gemini 3.1 Flash Lite (高速语义总结与 RAG 判决)
- **Embedding:** Google Gemini Embedding 2 (3072维多模态高精度向量化)

---

## ✨ 当前特性 (Features - v0.1 MVP)

- **[x] 异步知识摄入 (Async Ingestion):** 通过极简的数据中枢（Upload Console），一键摄入超长文本、会议录音转写或随想碎片。Go Worker 在后台静默完成摘要提炼与 3072 维坐标映射。
- **[x] 高维语义检索 (Semantic Search):** 突破关键词匹配的局限。运用 `pgvector` 库的余弦相似度（Cosine Similarity）算法，通过人类自然语言精准打捞深层共振的知识碎片。
- **[x] RAG 智能合成 (RAG Synthesis):** Gemini 结合检索到的上下文，自动排除噪声，生成结构化、极具条理的最终 Markdown 答复，实现跨文档的交叉印证。

---

## 🧠 核心架构哲学 (Architecture Philosophy)

本系统在架构设计上彻底放弃了传统关系型数据库的“树状目录”思维，拥抱纯粹的 **Agentic Memory (智能体记忆)** 理念：

1. **扁平的向量星海 (Flat Vector Sea):** 数据生而平等。没有任何人工设置的分类标签，知识块在 3072 维空间中的“距离（Distance）”即是它们天然的分类聚落。
2. **去 LangChain 化 (LangChain-Free):** 为追求极致的性能与对 Prompt 控制流的 100% 掌握，后端完全基于 Go 原生 SDK 手撕大模型调度与向量检索引擎，拒绝过度封装与黑盒。

---

## 🚀 下一步计划 (Roadmap)

我们正处于从静态知识库向“动态有机生命体”演进的边缘，接下来的开发将聚焦于以下前沿方向：

### Phase 1: 自适应动态记忆引擎 (Agentic Memory Consolidation)

- **细胞融合 (Semantic Merging):** 当摄入极短碎片时，后台触发向量共振，唤醒 LLM 判断是否与现有短笔记属于同一主题，实现知识的无缝自动合并与重写。
- **细胞分裂 (Chunk Splitting):** 当单一文档随时间推移长度逼近上下文窗口极限时，触发自动拆分机制，将长文重构为多篇逻辑独立但空间相邻的子文档。
- **无损压缩 (Lossless Compression):** 拒绝遗忘机制，通过 LLM 提炼将过时认知压缩为思想演进的时间线基石。

### Phase 2: 全栈可视化降维打击 (Visualizing the Void)

- **RAG 链路追踪 (Step-by-step Pipeline):** 搜索过程状态细分前端展示，直观呈现意图提取、星海检索、语义缝合的全流程逻辑。
- **3D 向量空间投影 (3D Projection):** 引入 PCA/UMAP 降维算法配合 React Three Fiber / ECharts 3D，在前端渲染可交互的 3D 粒子星空。每次检索，视觉焦点自动汇聚至被唤醒的知识高亮节点及连接链路。

### Phase 3: 工程化与安全加固 (Production Ready)

- **防注入网关 (Prompt Injection Guard):** 建立安全隔离层，抵御恶意绕过与数据库拖库攻击。
- **全栈流式响应 (SSE Streaming):** 实现打字机效果的低延迟回复体感。
- **云端部署 (Cloud Native):** 实现 Vercel (Front) + Render (Back) + Neon/Supabase (pgvector) 的三端分离部署。
- **多租户隔离 (Multi-tenancy):** 为家庭成员或朋友分配独立的 Namespace 向量空间。

---

>_“让所有的知识如星光般散落，又在被呼唤时如灵犀般汇聚。”_
