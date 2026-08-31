package web

import (
	"fmt"
	"net/http"
	"time"

	"claracore-server/internal/domain/external"
)

const externalReceiptLimit = 20

type sourceHealthView struct {
	Name, System, Status, Detail, LastStarted, LastSuccess, Lease, Request string
	Attention                                                              bool
}

type syncReceiptView struct {
	ID, Source, System, Status, Started, Finished, Error string
	Seen, Indexed, Unchanged, Missing                    int
	Attention                                            bool
}

type sourcesPageData struct {
	pageData
	Configured bool
	CanQueue   bool
	Summary    string
	Sources    []sourceHealthView
	Runs       []syncReceiptView
}

func (h *Host) handleSources(w http.ResponseWriter, r *http.Request) {
	h.renderSources(w, r, sourcesPageData{pageData: h.page(r, "外部来源", "sources")})
}

func (h *Host) handleSourceSync(w http.ResponseWriter, r *http.Request) {
	data := sourcesPageData{pageData: h.page(r, "外部来源", "sources")}
	p, _ := principalFrom(r.Context())
	if !h.opts.ExternalConfigured || h.opts.QueueExternalSync == nil {
		markWebAuditFailed(r)
		data.Error = "当前没有可用的 Atlas 同步配置。"
	} else if accepted, err := h.opts.QueueExternalSync(r.Context(), p.TenantID); err != nil {
		markWebAuditFailed(r)
		h.logger.Error("管理员请求 Atlas 同步入队失败", "error", err)
		data.Error = "同步请求未能记录，请检查服务日志后重试。"
	} else if !accepted {
		data.Notice = "已有一轮手动同步在等待，无需重复提交。"
	} else {
		data.Notice = "同步请求已记录；后台完成后，本页会显示新的运行回执。"
	}
	h.renderSources(w, r, data)
}

func (h *Host) renderSources(w http.ResponseWriter, r *http.Request, data sourcesPageData) {
	p, _ := principalFrom(r.Context())
	data.Configured = h.opts.ExternalConfigured
	data.CanQueue = h.opts.ExternalConfigured && h.opts.QueueExternalSync != nil
	data.Summary = "未启用 Atlas；Memory、Card 与共同线不受影响。"
	if data.Configured {
		data.Summary = "Atlas 已启用；只保存来源元数据与派生向量，不保存外部正文。"
	}
	if h.opts.ExternalSources != nil {
		sources, err := h.opts.ExternalSources(r.Context(), p.TenantID)
		if err != nil {
			h.logger.Warn("外部来源页读取来源状态失败", "error", err)
			if data.Error == "" {
				data.Error = "来源状态暂时不可用。"
			}
		} else {
			for _, source := range sources {
				data.Sources = append(data.Sources, makeSourceHealthView(source, data.Configured, h.opts.Now()))
			}
		}
	}
	if h.opts.ExternalSyncRuns != nil {
		runs, err := h.opts.ExternalSyncRuns(r.Context(), p.TenantID, externalReceiptLimit)
		if err != nil {
			h.logger.Warn("外部来源页读取同步回执失败", "error", err)
			if data.Error == "" {
				data.Error = "同步回执暂时不可用。"
			}
		} else {
			for _, run := range runs {
				data.Runs = append(data.Runs, makeSyncReceiptView(run))
			}
		}
	}
	h.render(w, "sources.html", data)
}

func makeSourceHealthView(source external.SourceHealth, configured bool, now time.Time) sourceHealthView {
	view := sourceHealthView{Name: source.Name, System: source.System, LastStarted: "从未", LastSuccess: "从未", Lease: "—", Request: "没有等待请求"}
	if source.LastStartedAt != nil {
		view.LastStarted = source.LastStartedAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	if source.LastSucceededAt != nil {
		view.LastSuccess = source.LastSucceededAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	if source.SyncLeaseUntil != nil {
		view.Lease = source.SyncLeaseUntil.UTC().Format("2006-01-02 15:04 UTC")
	}
	if source.SyncRequestedAt != nil {
		view.Request = "等待处理"
	}
	if source.SyncClaimed {
		if source.SyncClaimUntil != nil && source.SyncClaimUntil.After(now) {
			view.Request = "正在处理"
		} else {
			view.Request = "等待其他实例接管"
			view.Attention = true
		}
	}
	if !configured {
		view.Status, view.Detail, view.Attention = "仅镜像", fmt.Sprintf("%d 份有效镜像；连接关闭，不再刷新", source.DocumentCount), true
		return view
	}
	switch source.LastSyncStatus {
	case external.SyncSucceeded:
		view.Status, view.Detail = "同步正常", fmt.Sprintf("%d 份有效镜像", source.DocumentCount)
	case external.SyncRunning:
		view.Status, view.Detail = "同步中", "后台正在枚举来源并生成 body-free 向量"
		if source.SyncLeaseUntil == nil || !source.SyncLeaseUntil.After(now) {
			view.Status, view.Detail, view.Attention = "租约已过期", "本轮可能异常中断；下一轮可安全接管", true
		}
	case external.SyncFailed:
		view.Status, view.Detail, view.Attention = "最近失败", source.LastError, true
		if view.Detail == "" {
			view.Detail = "失败原因未记录，请检查服务日志"
		}
	default:
		view.Status, view.Detail, view.Attention = "尚未同步", "等待第一轮完整快照", true
	}
	return view
}

func makeSyncReceiptView(run external.SyncRunReceipt) syncReceiptView {
	view := syncReceiptView{
		ID: string(run.ID), Source: run.SourceName, System: run.SourceSystem,
		Started: run.StartedAt.UTC().Format("2006-01-02 15:04 UTC"), Finished: "进行中",
		Seen: run.Seen, Indexed: run.Indexed, Unchanged: run.Unchanged, Missing: run.Missing,
		Error: run.ErrorMessage,
	}
	if run.FinishedAt != nil {
		view.Finished = run.FinishedAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	switch run.Status {
	case external.SyncSucceeded:
		view.Status = "成功"
	case external.SyncRunning:
		view.Status = "进行中"
	case external.SyncFailed:
		view.Status, view.Attention = "失败", true
	default:
		view.Status, view.Attention = run.Status, true
	}
	return view
}
