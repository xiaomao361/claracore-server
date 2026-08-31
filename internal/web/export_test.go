package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/ports"
)

type fakeTenantExporter struct {
	body   []byte
	err    error
	input  ports.TenantExportInput
	calls  int
	before func()
}

func (f *fakeTenantExporter) Export(_ context.Context, in ports.TenantExportInput, w io.Writer) (ports.TenantExportResult, error) {
	f.calls++
	f.input = in
	if f.before != nil {
		f.before()
	}
	if f.err != nil {
		return ports.TenantExportResult{}, f.err
	}
	if _, err := w.Write(f.body); err != nil {
		return ports.TenantExportResult{}, err
	}
	return ports.TenantExportResult{FormatVersion: 1, GeneratedAt: in.GeneratedAt, RecordCount: 7, BytesWritten: int64(len(f.body))}, nil
}

func TestAdminTenantExportIsConfirmedAuditedAndCompleteBeforeDownload(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	act := &fakeActivity{}
	exporter := &fakeTenantExporter{body: []byte("complete-zip")}
	exporter.before = func() {
		if len(act.records) != 1 || act.records[0].Action != "admin.export.download" || act.records[0].Access != "read" || act.records[0].Kind != "meta_audit" {
			t.Fatalf("export must persist a sensitive-read audit before reading records: %+v", act.records)
		}
	}
	now := time.Date(2026, 8, 29, 12, 34, 56, 0, time.UTC)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), act, logger, Options{
		Version: "test", Now: func() time.Time { return now }, TenantExporter: exporter, ExportMaxBytes: 8 << 20,
	}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/admin/export", true))
	for _, want := range []string{"导出完整团队知识", "Memory", "Card", "Shared Line", "不包含密码哈希", "8 MiB", tenantExportConfirmation} {
		if !strings.Contains(page, want) {
			t.Errorf("export page missing %q", want)
		}
	}

	resp := post(t, srv, "/admin/export", url.Values{"confirm": {tenantExportConfirmation}}, true, true)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || string(got) != "complete-zip" {
		t.Fatalf("download mismatch: status=%d body=%q", resp.StatusCode, got)
	}
	if resp.Header.Get("Content-Type") != "application/zip" || !strings.Contains(resp.Header.Get("Content-Disposition"), "claracore-team-export-20260829T123456Z.zip") {
		t.Fatalf("unsafe or missing download headers: %+v", resp.Header)
	}
	if resp.Header.Get("Cache-Control") != "no-store, private" || resp.Header.Get("Content-Length") != "12" {
		t.Fatalf("export must be no-store with exact completed length: %+v", resp.Header)
	}
	if exporter.calls != 1 || exporter.input.TenantID != tenantA || exporter.input.MaxBytes != 8<<20 || !exporter.input.GeneratedAt.Equal(now) {
		t.Fatalf("exporter received wrong bounded tenant input: calls=%d input=%+v", exporter.calls, exporter.input)
	}
}

func TestAdminTenantExportFailsClosedBeforeContentRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		form      url.Values
		recordErr error
		exportErr error
		want      string
		wantCalls int
	}{
		{name: "missing explicit confirmation", form: url.Values{}, want: "请先确认", wantCalls: 0},
		{name: "audit unavailable", form: url.Values{"confirm": {tenantExportConfirmation}}, recordErr: errors.New("audit unavailable"), want: "已拒绝读取团队数据", wantCalls: 0},
		{name: "size limit", form: url.Values{"confirm": {tenantExportConfirmation}}, exportErr: ports.ErrTenantExportTooLarge, want: "超过当前大小上限", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gov := newFakeGov(governance.RoleAdmin, false)
			act := &fakeActivity{recordErr: tc.recordErr}
			exporter := &fakeTenantExporter{err: tc.exportErr}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), act, logger, Options{
				Version: "test", TenantExporter: exporter,
			}).Handler())
			t.Cleanup(srv.Close)

			page := body(t, post(t, srv, "/admin/export", tc.form, true, true))
			if !strings.Contains(page, tc.want) || exporter.calls != tc.wantCalls {
				t.Fatalf("fail-closed mismatch: page=%q calls=%d", page, exporter.calls)
			}
		})
	}
}
