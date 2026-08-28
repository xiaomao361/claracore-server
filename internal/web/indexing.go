package web

import (
	"net/http"

	"claracore-server/internal/domain/knowledge"
)

const indexingOperationBatch = 256

type indexingPageData struct {
	pageData
	Health   knowledge.EmbeddingHealth
	Backfill *knowledge.BackfillReport
	Retried  int
}

func (h *Host) handleIndexingPage(w http.ResponseWriter, r *http.Request) {
	h.renderIndexing(w, r, indexingPageData{pageData: h.page(r, "语义索引", "indexing")})
}

func (h *Host) handleIndexingBackfill(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := indexingPageData{pageData: h.page(r, "语义索引", "indexing")}
	report, err := h.know.BackfillEmbeddings(r.Context(), p.TenantID, indexingOperationBatch)
	if err != nil {
		markWebAuditFailed(r)
		h.logger.Error("回填语义索引失败", "error", err)
		data.Error = "回填失败"
	} else {
		data.Backfill = &report
		data.Notice = "已把下一批存量内容加入索引队列；后台 worker 会异步计算。"
	}
	h.renderIndexing(w, r, data)
}

func (h *Host) handleIndexingRetry(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := indexingPageData{pageData: h.page(r, "语义索引", "indexing")}
	n, err := h.know.RetryEmbeddingFailures(r.Context(), p.TenantID)
	if err != nil {
		markWebAuditFailed(r)
		h.logger.Error("重试语义索引失败项失败", "error", err)
		data.Error = "重试失败"
	} else {
		data.Retried = n
		data.Notice = "失败项已重新加入队列。"
	}
	h.renderIndexing(w, r, data)
}

func (h *Host) renderIndexing(w http.ResponseWriter, r *http.Request, data indexingPageData) {
	p, _ := principalFrom(r.Context())
	health, err := h.know.EmbeddingHealth(r.Context(), p.TenantID, 20)
	if err != nil {
		h.logger.Error("读取语义索引状态失败", "error", err)
		if data.Error == "" {
			data.Error = "读取索引状态失败"
		}
	} else {
		data.Health = health
		if !health.Enabled && data.Notice == "" {
			data.Notice = "当前未配置 Embedding；检索仍使用中文全文匹配。"
		}
	}
	h.render(w, "indexing.html", data)
}
