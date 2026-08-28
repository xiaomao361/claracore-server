package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

type ingestPageData struct {
	pageData
	Enabled     bool
	Root        string
	Spaces      []governance.Space
	SourceName  string
	RelativeDir string
	Selected    platform.ID
	Ran         bool
	ScanWarning string
	Report      knowledge.IngestReport
}

func (h *Host) handleIngestPage(w http.ResponseWriter, r *http.Request) {
	h.renderIngest(w, r, ingestPageData{pageData: h.page(r, "导入 Markdown", "ingest")})
}

func (h *Host) renderIngest(w http.ResponseWriter, r *http.Request, data ingestPageData) {
	p, _ := principalFrom(r.Context())
	data.Enabled = strings.TrimSpace(h.opts.IngestRoot) != "" && h.opts.ScanMarkdown != nil
	data.Root = h.opts.IngestRoot

	scope, err := h.gov.Scope(r.Context(), governance.Identity{TenantID: p.TenantID, UserID: p.UserID})
	if err != nil {
		h.logger.Error("导入页解析空间权限失败", "error", err)
		data.Error = "读取空间权限失败"
	} else {
		for _, access := range scope.All() {
			if access.CanWrite {
				data.Spaces = append(data.Spaces, access.Space)
			}
		}
	}
	if !data.Enabled && data.Error == "" {
		data.Notice = "Web 导入尚未启用；请在服务器配置 CCS_INGEST_ROOT。"
	}
	h.render(w, "ingest.html", data)
}

func (h *Host) handleIngestRun(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := ingestPageData{
		pageData:    h.page(r, "导入 Markdown", "ingest"),
		SourceName:  strings.TrimSpace(r.FormValue("source")),
		RelativeDir: strings.TrimSpace(r.FormValue("dir")),
		Selected:    platform.ID(r.FormValue("space")),
	}
	if strings.TrimSpace(h.opts.IngestRoot) == "" || h.opts.ScanMarkdown == nil {
		markWebAuditFailed(r)
		data.Error = "Web 导入尚未启用"
		h.renderIngest(w, r, data)
		return
	}
	if data.SourceName == "" || utf8.RuneCountInString(data.SourceName) > knowledge.MaxSourceNameLen || strings.ContainsAny(data.SourceName, "/\\\x00") {
		markWebAuditFailed(r)
		data.Error = "来源名称不能为空、不能包含斜杠，且最多 120 个字"
		h.renderIngest(w, r, data)
		return
	}

	scope, err := h.gov.Scope(r.Context(), governance.Identity{TenantID: p.TenantID, UserID: p.UserID})
	if err != nil {
		markWebAuditFailed(r)
		data.Error = "读取空间权限失败"
		h.renderIngest(w, r, data)
		return
	}
	space, err := scope.ResolveWriteTarget(data.Selected)
	if err != nil {
		markWebAuditFailed(r)
		data.Error = "目标空间不存在或不可写"
		h.renderIngest(w, r, data)
		return
	}

	dir, err := resolveIngestDir(h.opts.IngestRoot, data.RelativeDir)
	if err != nil {
		markWebAuditFailed(r)
		data.Error = err.Error()
		h.renderIngest(w, r, data)
		return
	}
	docs, scanErr := h.opts.ScanMarkdown(r.Context(), data.SourceName, dir)
	if len(docs) == 0 {
		markWebAuditFailed(r)
		data.Error = "没有扫到可导入的 Markdown 文档"
		if scanErr != nil {
			h.logger.Warn("Web 导入扫描失败", "error", scanErr)
		}
		h.renderIngest(w, r, data)
		return
	}
	if scanErr != nil {
		// 部分扫描不能证明“未出现的文件已经删除”，因此保留成功项，
		// 但明确关闭缺失检测。
		data.ScanWarning = "部分文件读取失败；已导入可读文件，本轮未执行缺失检测。"
		h.logger.Warn("Web 导入部分扫描失败", "error", scanErr)
	}
	report, err := h.know.Ingest(r.Context(), knowledge.IngestInput{
		TenantID: p.TenantID, SpaceID: space.ID, AuthorUserID: p.UserID,
		SourceName: data.SourceName, DetectMissing: scanErr == nil, Documents: docs,
	})
	if err != nil {
		markWebAuditFailed(r)
		h.logger.Error("Web 导入失败", "space_id", space.ID, "error", err)
		data.Error = "导入失败"
		h.renderIngest(w, r, data)
		return
	}
	data.Ran, data.Report = true, report
	setWebAuditTarget(r, "space", space.ID, space.ID)
	h.renderIngest(w, r, data)
}

// resolveIngestDir 同时拦相对路径穿越与符号链接逃逸。只做 Clean/Join
// 不够：root/allowed/link 可以指向 /etc，而字符串仍看起来在 root 下面。
func resolveIngestDir(root, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("导入目录必须是配置根目录下的相对路径")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("服务器导入根目录不可用")
	}
	if relative == "" {
		relative = "."
	}
	candidate, err := filepath.EvalSymlinks(filepath.Join(realRoot, filepath.Clean(relative)))
	if err != nil {
		return "", fmt.Errorf("导入目录不存在或不可读")
	}
	rel, err := filepath.Rel(realRoot, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("导入目录必须位于服务器配置的根目录内")
	}
	return candidate, nil
}
