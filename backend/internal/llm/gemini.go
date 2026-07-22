package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/generative-ai-go/genai"
	"github.com/pgvector/pgvector-go"
	"google.golang.org/api/option"
)

// getAPIKey 从环境变量获取 Gemini API Key，缺失时直接 fatal 退出
func getAPIKey() string {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		panic("❌ 致命错误: 未设置 GEMINI_API_KEY 环境变量")
	}
	return key
}

// GenerateSummaryAndTags 调用 Gemini 生成摘要和标签
func GenerateSummaryAndTags(contextData string) (summary string, tags string, err error) {
	ctx := context.Background()
	// 从环境变量中读取刚才配置的 Key
	client, err := genai.NewClient(ctx, option.WithAPIKey(getAPIKey()))
	if err != nil {
		return "", "", fmt.Errorf("创建 AI 客户端失败: %v", err)
	}
	defer client.Close()

	// 使用目前速度最快、免费额度最高的主力模型
	model := client.GenerativeModel("gemini-3.1-flash-lite")
	// 构造极其严格的 Prompt（提示词工程）
	prompt := fmt.Sprintf(`
请你作为一个专业的信息提炼专家，阅读以下内容，并按要求输出：
1. 用一段简明扼要的话概括核心内容（摘要）。
2. 提取 3-5 个核心关键词，用逗号分隔。

格式严格如下：
摘要：[你的摘要内容]
标签：[标签1,标签2,标签3]

需要处理的内容如下：
%s
`, contextData)

	resp, err := model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		return "", "", fmt.Errorf("生成内容失败: %v", err)
	}

	// 解析大模型返回的文本
	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		aiText := fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0])

		// 简单粗暴的字符串处理，分离摘要和标签
		lines := strings.Split(aiText, "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "摘要：") {
				summary = strings.TrimPrefix(line, "摘要：")
			} else if strings.HasPrefix(line, "标签：") {
				tags = strings.TrimPrefix(line, "标签：")
			}
		}
		return summary, tags, nil
	}

	return "", "", fmt.Errorf("AI 返回格式异常")
}

// GenerateEmbedding 调用 Gemini 专门的向量模型，生成 3072 维向量
func GenerateEmbedding(content string) (*pgvector.Vector, error) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(getAPIKey()))
	if err != nil {
		return nil, fmt.Errorf("创建 AI 客户端失败: %v", err)
	}
	defer client.Close()

	// 选用最新的文本向量模型
	em := client.EmbeddingModel("gemini-embedding-2")
	res, err := em.EmbedContent(ctx, genai.Text(content))
	if err != nil {
		return nil, fmt.Errorf("生成向量失败: %v", err)
	}

	if len(res.Embedding.Values) > 0 {
		// 将返回的 []float32 转换成 pgvector 认识的格式
		vec := pgvector.NewVector(res.Embedding.Values)
		return &vec, nil
	}

	return nil, fmt.Errorf("向量生成为空")
}

// 找到文件末尾，加上这个 RAG 回答函数
func GenerateRAGAnswer(query string, contextData string) (string, error) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(getAPIKey()))
	if err != nil {
		return "", fmt.Errorf("创建客户端失败: %v", err)
	}
	defer client.Close()

	model := client.GenerativeModel("gemini-3.1-flash-lite")

	// 这是 RAG 系统最核心的 Prompt 工程，强制 AI 必须且只能基于给定的知识库资料作答
	prompt := fmt.Sprintf(`你是一个极其专业的知识库智能助手。
请仔细阅读以下【参考资料】，并用温柔、清晰且结构化的中文来回答用户的【问题】。
要求：
1. 你的回答必须完全基于参考资料中的内容，绝对不允许胡编乱造。
2. 如果参考资料中没有提及相关答案，请如实回答“知识库中没有相关信息”。

【参考资料】:
%s

【用户问题】:
%s`, contextData, query)

	resp, err := model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		return "", fmt.Errorf("生成回答失败: %v", err)
	}

	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		return fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0]), nil
	}
	return "", fmt.Errorf("AI 返回结果为空")
}

// 定义大模型返回的严格 JSON 结构
type MergeDecision struct {
	Decision      string `json:"decision"`       // "MERGE" 或 "KEEP_SEPARATE" 或 "SPLIT"
	MergedContent string `json:"merged_content"` // 如果是 MERGE，这里存放重写后的完整内容
	Reason        string `json:"reason"`         // AI 给出这个决定的简短理由（方便我们调试 Debug）
}

// EvaluateAndMerge 核心记忆决策引擎
// newContent: 用户刚刚提交的新碎片
// existingContent: 数据库中检索到的最相似的老文章
func EvaluateAndMerge(newContent string, existingContent string) (*MergeDecision, error) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(os.Getenv("GEMINI_API_KEY")))
	if err != nil {
		return nil, fmt.Errorf("创建客户端失败: %v", err)
	}
	defer client.Close()

	model := client.GenerativeModel("gemini-3.1-flash-lite")

	// 强制要求大模型返回 JSON
	model.ResponseMIMEType = "application/json"

	// 核心 Prompt 工程：给 AI 设定极度严苛的合并规则
	prompt := fmt.Sprintf(`你是一个知识库底层的数据整理引擎（Agentic Memory Router）。
你的任务是评估用户提交的【新碎片】是否应该与数据库中检索到的【高相似度旧文档】进行合并。

【旧文档内容】:
%s

【新碎片内容】:
%s

评估规则：
1. 如果新旧内容讲述的是同一个具体事物的延续、补充、状态更新，或者对旧文档中观点的深度探讨，请做出 "MERGE" 的决定，并将两者天衣无缝地重写为一篇逻辑严密的完整 Markdown 文档（无损保留所有信息）。
2. 如果新旧内容只是恰好提到了相似的名词（例如一个是“竹笛的材质”，一个是“洞箫的材质”），但本质上是两个独立的知识实体，请做出 "KEEP_SEPARATE" 的决定。

你必须严格遵守以下 JSON 格式输出：
{
  "decision": "MERGE" 或 "KEEP_SEPARATE",
  "merged_content": "如果 decision 为 MERGE，请在这里输出融合后的完整 Markdown 文本。如果为 KEEP_SEPARATE，留空字符串即可。",
  "reason": "简短解释为什么做出这个决定"
}`, existingContent, newContent)

	resp, err := model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		return nil, fmt.Errorf("生成决策失败: %v", err)
	}

	if len(resp.Candidates) > 0 && len(resp.Candidates[0].Content.Parts) > 0 {
		aiResponse := fmt.Sprintf("%v", resp.Candidates[0].Content.Parts[0])

		var decision MergeDecision
		if err := json.Unmarshal([]byte(aiResponse), &decision); err != nil {
			return nil, fmt.Errorf("解析 AI JSON 返回结果失败: %v\n原始返回: %s", err, aiResponse)
		}

		return &decision, nil
	}

	return nil, fmt.Errorf("AI 返回结果为空")
}
