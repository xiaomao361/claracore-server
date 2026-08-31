package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/external"
	"claracore-server/internal/domain/governance"
)

type governanceTask struct {
	Title     string
	Count     int
	Summary   string
	Href      string
	LinkLabel string
	Attention bool
}

type governancePageData struct {
	pageData
	Tasks []governanceTask
}

// handleGovernance turns existing domain signals into a bounded human worklist.
// It deliberately owns no write action: every row links to the established page
// that already enforces the corresponding permission and audit boundary.
func (h *Host) handleGovernance(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := governancePageData{pageData: h.page(r, "治理待办", "governance")}

	scope, err := h.gov.Scope(r.Context(), governance.Identity{TenantID: p.TenantID, UserID: p.UserID})
	if err != nil {
		data.Error = "无法读取空间权限，请稍后重试"
		h.logger.Error("治理待办解析空间权限失败", "error", err)
		h.render(w, "governance.html", data)
		return
	}

	stats, err := h.know.SpaceStats(r.Context(), p.TenantID, scope.Readable())
	if err != nil {
		data.Error = "无法读取知识质量，请稍后重试"
		h.logger.Error("治理待办读取知识质量失败", "error", err)
		h.render(w, "governance.html", data)
		return
	}
	writable := make(map[string]bool)
	for _, access := range scope.All() {
		writable[string(access.Space.ID)] = access.CanWrite
	}
	var drafts, unqualified int
	for _, stat := range stats {
		if writable[string(stat.SpaceID)] {
			drafts += stat.DraftCards
			unqualified += stat.ActiveUnqualifiedCards
		}
	}
	data.Tasks = append(data.Tasks,
		governanceTask{
			Title: "审核待发布卡片", Count: drafts, Attention: drafts > 0,
			Summary: "只统计你有写权限的空间；逐张补齐理解与召回条件后再发布。",
			Href:    "/review", LinkLabel: "打开审核队列",
		},
		governanceTask{
			Title: "补齐已发布卡片", Count: unqualified, Attention: unqualified > 0,
			Summary: "这些 active 卡片缺少“理解”；Web 保持只读，请让 Agent 通过正常更新流程修正。",
			Href:    "/browse?kind=card&status=active", LinkLabel: "检查已发布卡片",
		},
	)

	if lines, err := h.continuity.Stats(r.Context(), p.TenantID); err != nil {
		data.Notice = appendNotice(data.Notice, "共同线信号暂时不可用。")
	} else {
		count := lines.NeedsReview + lines.Stale
		data.Tasks = append(data.Tasks, governanceTask{
			Title: "复核共同线", Count: count, Attention: count > 0,
			Summary: fmt.Sprintf("待复核 %d 条，已陈旧 %d 条；确认当前位置后再继续或归档。", lines.NeedsReview, lines.Stale),
			Href:    "/continuity", LinkLabel: "查看共同线",
		})
	}

	if h.activity == nil {
		data.Notice = appendNotice(data.Notice, "自动上下文质量暂时不可用。")
	} else if quality, err := h.activity.ContextFeedbackQuality(r.Context(), p.TenantID, 30*24*time.Hour); err != nil {
		data.Notice = appendNotice(data.Notice, "自动上下文质量暂时不可用。")
	} else {
		count := quality.Wrong + quality.Corrected + quality.AbstainReasons.Ambiguous
		data.Tasks = append(data.Tasks, governanceTask{
			Title: "检查自动上下文", Count: count, Attention: count > 0,
			Summary: fmt.Sprintf("错误 %d、已纠正 %d、因歧义放弃 %d；这些是治理信号，不是准确率。", quality.Wrong, quality.Corrected, quality.AbstainReasons.Ambiguous),
			Href:    "/context-quality", LinkLabel: "查看上下文质量",
		})
	}

	if p.IsAdmin() {
		if health, err := h.know.EmbeddingHealth(r.Context(), p.TenantID, 0); err != nil {
			data.Notice = appendNotice(data.Notice, "语义索引信号暂时不可用。")
		} else {
			data.Tasks = append(data.Tasks, governanceTask{
				Title: "处理语义索引失败", Count: health.Stats.Failed, Attention: health.Stats.Failed > 0,
				Summary: fmt.Sprintf("等待 %d，已耗尽自动重试 %d；确认模型服务恢复后再重试。", health.Stats.Pending, health.Stats.Exhausted),
				Href:    "/admin/indexing", LinkLabel: "打开语义索引",
			})
		}
		if h.opts.ExternalConfigured && h.opts.ExternalSources != nil {
			sources, err := h.opts.ExternalSources(r.Context(), p.TenantID)
			if err != nil {
				data.Notice = appendNotice(data.Notice, "外部来源信号暂时不可用。")
			} else {
				attention := 0
				for _, source := range sources {
					if source.LastSyncStatus == external.SyncFailed || source.LastSyncStatus == external.SyncNever || source.LastSucceededAt == nil {
						attention++
					}
				}
				data.Tasks = append(data.Tasks, governanceTask{
					Title: "恢复外部来源", Count: attention, Attention: attention > 0,
					Summary: "只统计最近同步失败、从未同步或尚无成功回执的来源。",
					Href:    "/admin/sources", LinkLabel: "打开外部来源",
				})
			}
		}
	}

	h.render(w, "governance.html", data)
}

func appendNotice(current, next string) string {
	return strings.TrimSpace(current + " " + next)
}

type contextQualityPageData struct {
	pageData
	Quality     activity.ContextFeedbackQuality
	Range       string
	WindowLabel string
	Coverage    int
	OutcomeSum  int
}

func (h *Host) handleContextQuality(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := contextQualityPageData{pageData: h.page(r, "自动上下文质量", "context-quality")}
	window := 30 * 24 * time.Hour
	data.Range, data.WindowLabel = "30d", "近 30 天"
	if r.URL.Query().Get("range") == "7d" {
		window, data.Range, data.WindowLabel = 7*24*time.Hour, "7d", "近 7 天"
	}
	if h.activity == nil {
		data.Error = "自动上下文质量暂时不可用"
		h.render(w, "context_quality.html", data)
		return
	}
	quality, err := h.activity.ContextFeedbackQuality(r.Context(), p.TenantID, window)
	if err != nil {
		data.Error = "无法读取自动上下文质量，请稍后重试"
		h.logger.Error("读取自动上下文质量失败", "error", err)
		h.render(w, "context_quality.html", data)
		return
	}
	data.Quality = quality
	data.OutcomeSum = quality.Total()
	if quality.Selected > 0 {
		data.Coverage = quality.FeedbackCoveredSelections * 100 / quality.Selected
	}
	h.render(w, "context_quality.html", data)
}
