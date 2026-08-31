package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/ports"
)

const tenantExportConfirmation = "export-complete-tenant"

type exportPageData struct {
	pageData
	MaxBytes string
}

func (h *Host) handleExportPage(w http.ResponseWriter, r *http.Request) {
	h.renderExportPage(w, r, "")
}

func (h *Host) handleExportDownload(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if r.FormValue("confirm") != tenantExportConfirmation {
		markWebAuditFailed(r)
		h.renderExportPage(w, r, "请先确认导出范围和保管责任")
		return
	}
	if h.opts.TenantExporter == nil || h.activity == nil {
		markWebAuditFailed(r)
		h.renderExportPage(w, r, "团队导出暂时不可用")
		return
	}
	select {
	case h.exportGate <- struct{}{}:
		defer func() { <-h.exportGate }()
	default:
		markWebAuditFailed(r)
		h.renderExportPage(w, r, "当前实例已有团队导出正在生成，请稍后重试")
		return
	}

	// Fail closed before reading tenant content. Like complete-audit access, a
	// portable export is a sensitive read and must leave a durable meta-audit
	// before the first record is queried.
	metadata, _ := json.Marshal(map[string]any{
		"scope": "tenant", "confirmation": "explicit", "format": "claracore-team-export",
		"credentials_excluded": true,
	})
	if _, err := h.activity.Record(r.Context(), activity.RecordInput{
		TenantID: p.TenantID, UserID: p.UserID, Source: activity.SourceWeb,
		Kind: activity.KindMetaAudit, Access: activity.AccessRead,
		Outcome: activity.OutcomeSuccess, Action: "admin.export.download", Metadata: metadata,
	}); err != nil {
		markWebAuditFailed(r)
		h.logger.Error("记录团队导出审计失败", "error", err)
		h.renderExportPage(w, r, "无法记录本次导出，已拒绝读取团队数据")
		return
	}

	tmp, err := os.CreateTemp("", "claracore-team-export-*.zip")
	if err != nil {
		markWebAuditFailed(r)
		h.logger.Error("创建团队导出临时文件失败", "error", err)
		h.renderExportPage(w, r, "无法创建安全的临时导出文件")
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	defer tmp.Close()
	if err := tmp.Chmod(0o600); err != nil {
		markWebAuditFailed(r)
		h.logger.Error("限制团队导出临时文件权限失败", "error", err)
		h.renderExportPage(w, r, "无法保护临时导出文件")
		return
	}

	generatedAt := h.opts.Now().UTC()
	result, err := h.opts.TenantExporter.Export(r.Context(), ports.TenantExportInput{
		TenantID: p.TenantID, GeneratedAt: generatedAt, MaxBytes: h.opts.ExportMaxBytes,
	}, tmp)
	if err != nil {
		markWebAuditFailed(r)
		if errors.Is(err, ports.ErrTenantExportTooLarge) {
			h.renderExportPage(w, r, "导出超过当前大小上限；请提高 CCS_EXPORT_MAX_BYTES 后重试，或使用数据库灾备流程")
			return
		}
		h.logger.Error("生成团队导出失败", "error", err)
		h.renderExportPage(w, r, "生成团队导出失败；没有发送不完整文件")
		return
	}
	if err := tmp.Sync(); err != nil {
		markWebAuditFailed(r)
		h.logger.Error("落盘团队导出失败", "error", err)
		h.renderExportPage(w, r, "导出文件未能安全落盘")
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		markWebAuditFailed(r)
		h.logger.Error("读取团队导出临时文件失败", "error", err)
		h.renderExportPage(w, r, "导出文件暂时不可读")
		return
	}

	filename := "claracore-team-export-" + result.GeneratedAt.Format("20060102T150405Z") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", result.BytesWritten))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, tmp); err != nil {
		markWebAuditFailed(r)
		h.logger.Warn("发送团队导出中断", "error", err)
	}
}

func (h *Host) renderExportPage(w http.ResponseWriter, r *http.Request, message string) {
	data := exportPageData{
		pageData: h.page(r, "团队导出", "export"),
		MaxBytes: humanExportBytes(h.opts.ExportMaxBytes),
	}
	data.Error = message
	h.render(w, "export.html", data)
}

func humanExportBytes(value int64) string {
	const (
		mib = int64(1 << 20)
		gib = int64(1 << 30)
	)
	if value >= gib && value%gib == 0 {
		return fmt.Sprintf("%d GiB", value/gib)
	}
	return fmt.Sprintf("%d MiB", value/mib)
}
