package llm

import (
	"context"
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
