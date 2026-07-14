"use client";

import { useState } from "react";

// 定义后端返回的数据结构
interface SearchResult {
  id: string;
  title: string;
  ai_summary: string;
  similarity: number;
}

interface SearchResponse {
  query: string;
  answer: string;
  results: SearchResult[];
}

export default function Home() {
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(false);
  const [data, setData] = useState<SearchResponse | null>(null);
  const [error, setError] = useState("");

  const handleSearch = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!query.trim()) return;

    setLoading(true);
    setError("");
    setData(null);

    try {
      const res = await fetch("http://localhost:8080/api/search", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ query }),
      });

      if (!res.ok) {
        throw new Error("搜索请求失败，请检查后端服务是否启动");
      }

      const json = await res.json();
      setData(json);
    } catch (err: unknown) {
      if (err instanceof Error) {
        setError(err.message);
      } else {
        setError(String(err));
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <main className="min-h-screen bg-linear-to-br from-slate-900 via-purple-900 to-slate-900 text-white p-8 font-sans selection:bg-fuchsia-500 selection:text-white">
      <div className="max-w-4xl mx-auto pt-20">
        {/* 头部标题 */}
        <div className="text-center mb-12">
          <h1 className="text-5xl font-extrabold tracking-tight mb-4 bg-clip-text text-transparent bg-linear-to-r from-fuchsia-400 to-cyan-400">
            EchoBase (灵犀)
          </h1>
          <p className="text-slate-400 text-lg">基于 Gemini 3.1 & 3072维向量的专属 AI 知识库</p>
        </div>

        {/* 搜索框 (毛玻璃效果) */}
        <form onSubmit={handleSearch} className="relative mb-12 group">
          <div className="absolute -inset-1 bg-linear-to-r from-fuchsia-600 to-cyan-600 rounded-2xl blur opacity-25 group-hover:opacity-50 transition duration-1000 group-hover:duration-200"></div>
          <div className="relative flex items-center bg-slate-800/80 backdrop-blur-xl ring-1 ring-white/10 rounded-2xl p-2">
            <input
              type="text"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="向你的知识库提问，例如：刘晓艳老师推荐怎么防止走神？"
              className="flex-1 bg-transparent border-none outline-none px-6 py-4 text-lg text-white placeholder-slate-500"
              disabled={loading}
            />
            <button
              type="submit"
              disabled={loading}
              className="bg-linear-to-r from-fuchsia-500 to-cyan-500 text-white px-8 py-4 rounded-xl font-bold transition hover:scale-105 disabled:opacity-50 disabled:hover:scale-100 flex items-center gap-2"
            >
              {loading ? (
                <>
                  <svg className="animate-spin h-5 w-5 text-white" viewBox="0 0 24 24">
                    <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" fill="none"></circle>
                    <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
                  </svg>
                  <span>潜入星海...</span>
                </>
              ) : (
                "智能检索"
              )}
            </button>
          </div>
        </form>

        {/* 错误提示 */}
        {error && (
          <div className="bg-red-500/20 border border-red-500/50 text-red-200 px-6 py-4 rounded-xl mb-8">
            {error}
          </div>
        )}

        {/* 结果展示区 */}
        {data && (
          <div className="space-y-8 animate-in fade-in slide-in-from-bottom-4 duration-700 ease-out">
            {/* AI 终极回答卡片 */}
            <div className="bg-slate-800/60 backdrop-blur-lg border border-white/10 rounded-3xl p-8 shadow-2xl">
              <h2 className="text-2xl font-bold mb-6 flex items-center gap-3">
                <span className="text-2xl">✨</span> 灵犀管家的回答：
              </h2>
              <div className="prose prose-invert max-w-none text-slate-300 leading-relaxed space-y-4 whitespace-pre-wrap">
                {data.answer}
              </div>
            </div>

            {/* 参考资料来源卡片列表 */}
            {data.results.length > 0 && (
              <div>
                <h3 className="text-xl font-semibold text-slate-400 mb-6 mt-12 flex items-center gap-2">
                  <span className="text-cyan-400">⚡</span> 检索到的 3072 维高频共振碎片：
                </h3>
                <div className="grid grid-cols-1 gap-4">
                  {data.results.map((res, index) => (
                    <div key={res.id} className="bg-slate-800/40 backdrop-blur-md border border-white/5 rounded-2xl p-6 hover:bg-slate-800/60 transition">
                      <div className="flex justify-between items-start mb-3">
                        <h4 className="text-lg font-medium text-fuchsia-300">
                          {index + 1}. {res.title}
                        </h4>
                        <span className="text-xs font-mono bg-cyan-900/50 text-cyan-300 px-3 py-1 rounded-full border border-cyan-500/30">
                          相关度: {(res.similarity * 100).toFixed(1)}%
                        </span>
                      </div>
                      <p className="text-sm text-slate-400 line-clamp-3">
                        {res.ai_summary}
                      </p>
                    </div>
                  ))}
                </div>
              </div>
            )}
          </div>
        )}
      </div>
    </main>
  );
}