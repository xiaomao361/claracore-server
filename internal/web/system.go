package web

import (
	"net/http"

	"claracore-server/internal/domain/knowledge"
)

type systemPageData struct {
	pageData
	Environment       string
	DatabaseStatus    string
	RedisStatus       string
	EmbeddingProvider string
	Embedding         knowledge.EmbeddingHealth
	CacheEnabled      bool
	CacheHits         uint64
	CacheMisses       uint64
}

func (h *Host) handleSystem(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := systemPageData{
		pageData:          h.page(r, "系统状态", "system"),
		Environment:       h.opts.Env,
		DatabaseStatus:    "未提供检查器",
		RedisStatus:       "未配置；当前使用数据库会话与进程内能力",
		EmbeddingProvider: h.opts.EmbeddingProvider,
	}
	if data.Environment == "" {
		data.Environment = "unknown"
	}
	if h.opts.DatabaseHealth != nil {
		if err := h.opts.DatabaseHealth(r.Context()); err != nil {
			data.DatabaseStatus = "连接失败"
			data.Error = "数据库当前不可达，请先检查服务日志与数据库连接。"
			h.logger.Error("系统页数据库健康检查失败", "error", err)
		} else {
			data.DatabaseStatus = "连接正常"
		}
	}
	if h.opts.RedisConfigured {
		data.RedisStatus = "已配置；当前版本尚未装配 Redis 运行时，仍使用数据库会话与进程内能力"
	}
	if health, err := h.know.EmbeddingHealth(r.Context(), p.TenantID, 5); err != nil {
		data.Notice = "Embedding 索引状态暂时不可用。"
		h.logger.Warn("系统页读取 Embedding 状态失败", "error", err)
	} else {
		data.Embedding = health
	}
	if h.opts.EmbeddingCacheStats != nil {
		data.CacheHits, data.CacheMisses, data.CacheEnabled = h.opts.EmbeddingCacheStats()
	}
	h.render(w, "system.html", data)
}
