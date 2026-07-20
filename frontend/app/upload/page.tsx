"use client";

import { useState } from "react";
import Link from "next/link";

export default function UploadPage() {
  const [title, setTitle] = useState("");
  const [content, setContent] = useState("");
  const [loading, setLoading] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!title.trim() || !content.trim()) return;

    setLoading(true);
    setMessage("");
    setError("");

    try {
      const res = await fetch(`${process.env.BACKEND_URL}/api/documents`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title, content }),
      });

      if (!res.ok) {
        throw new Error("数据落盘失败，请检查后端状态");
      }

      setMessage("✅ 文档已成功发送至引擎！后台正在进行摘要与 3072 维向量提炼...");
      setTitle("");
      setContent("");
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
      <div className="max-w-4xl mx-auto pt-10">
        {/* 导航区域 */}
        <div className="mb-8 flex justify-between items-center">
          <h1 className="text-4xl font-extrabold bg-clip-text text-transparent bg-linear-to-r from-cyan-400 to-fuchsia-400">
            EchoBase 数据中枢
          </h1>
          <Link href="/" className="text-slate-400 hover:text-white transition flex items-center gap-2 border border-slate-700 bg-slate-800/50 px-4 py-2 rounded-lg hover:bg-slate-700/50">
            ← 返回检索台
          </Link>
        </div>

        {/* 表单区域 */}
        <form onSubmit={handleSubmit} className="bg-slate-800/60 backdrop-blur-xl border border-white/10 rounded-3xl p-8 shadow-2xl">
          <div className="space-y-6">
            <div>
              <label className="block text-sm font-medium text-slate-300 mb-2">知识切片标题</label>
              <input
                type="text"
                value={title}
                onChange={(e) => setTitle(e.target.value)}
                placeholder="例如：滑动窗口算法边界条件实战复盘"
                className="w-full bg-slate-900/50 border border-white/10 rounded-xl px-4 py-3 text-white focus:outline-none focus:ring-2 focus:ring-fuchsia-500 transition"
                disabled={loading}
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-slate-300 mb-2">原始内容 (支持超长文本)</label>
              <textarea
                value={content}
                onChange={(e) => setContent(e.target.value)}
                placeholder="将你的视频转写文本、代码笔记或思路随笔粘贴在这里..."
                rows={12}
                className="w-full bg-slate-900/50 border border-white/10 rounded-xl px-4 py-3 text-white focus:outline-none focus:ring-2 focus:ring-fuchsia-500 transition resize-y"
                disabled={loading}
              ></textarea>
            </div>

            {/* 状态提示 */}
            {message && (
              <div className="text-green-400 bg-green-400/10 p-4 rounded-xl border border-green-400/20 animate-in fade-in">
                {message}
              </div>
            )}
            {error && (
              <div className="text-red-400 bg-red-400/10 p-4 rounded-xl border border-red-400/20 animate-in fade-in">
                {error}
              </div>
            )}

            <button
              type="submit"
              disabled={loading}
              className="w-full bg-linear-to-r from-fuchsia-600 to-cyan-600 text-white font-bold text-lg py-4 rounded-xl hover:scale-[1.02] transition-transform disabled:opacity-50 disabled:hover:scale-100 flex justify-center items-center gap-2 shadow-lg shadow-fuchsia-500/20"
            >
              {loading ? (
                <>
                  <svg className="animate-spin h-5 w-5 text-white" viewBox="0 0 24 24">
                    <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" fill="none"></circle>
                    <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
                  </svg>
                  <span>量子纠缠中...</span>
                </>
              ) : (
                "注入知识库"
              )}
            </button>
          </div>
        </form>
      </div>
    </main>
  );
}