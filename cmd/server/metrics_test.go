package main

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/ports"
	"claracore-server/internal/web"
)

type metricsBackupReader struct {
	status ports.BackupStatus
	err    error
}

func (r metricsBackupReader) Read() (ports.BackupStatus, error) { return r.status, r.err }

type metricsRestoreReader struct {
	status ports.RestoreStatus
	err    error
}

func (r metricsRestoreReader) Read() (ports.RestoreStatus, error) { return r.status, r.err }

type metricsIntegrityReader struct {
	status ports.IntegrityStatus
	err    error
}

func (r metricsIntegrityReader) Read() (ports.IntegrityStatus, error) { return r.status, r.err }

func TestMetricsExposeOnlyBoundedOperationalState(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	handler := newMetricsHandler(metricsOptions{
		Version: "v0.2.0", SourceDigest: strings.Repeat("a", 64), BinaryDigest: strings.Repeat("b", 64), Environment: "production", SchemaVersion: "0010", StartedAt: now.Add(-2 * time.Hour),
		MCPAgentRateLimit: 300, MCPTenantRateLimit: 3000, MCPRateWindow: time.Minute,
		Now:       func() time.Time { return now },
		Accepting: func() bool { return false },
		HTTPStats: func() web.HTTPRequestStats {
			return web.HTTPRequestStats{
				Available: true, Window: 5 * time.Minute, Requests: 42, ClientErrors: 3,
				ServerErrors: 1, Panics: 1, InFlight: 2, AverageLatency: 25 * time.Millisecond,
				P95Latency: 250 * time.Millisecond,
			}
		},
		DatabasePool: func() sql.DBStats {
			return sql.DBStats{
				MaxOpenConnections: 25, OpenConnections: 7, InUse: 3, Idle: 4,
				WaitCount: 9, WaitDuration: 1500 * time.Millisecond,
				MaxIdleClosed: 2, MaxIdleTimeClosed: 3, MaxLifetimeClosed: 4,
			}
		},
		BackupStatus: metricsBackupReader{status: ports.BackupStatus{
			Database: "secret_database", Archive: "secret_archive.sql.gz",
			CreatedAt: now.Add(-time.Hour), VerifiedAt: now.Add(-59 * time.Minute), MaxAge: 26 * time.Hour,
			IntegrityVerified: true, Recoverability: "not_proven",
		}},
		RestoreStatus: metricsRestoreReader{status: ports.RestoreStatus{
			Database: "secret_database", Archive: "secret_restore_archive.sql.gz", Report: "secret.restore-report.tsv",
			RestoredAt: now.Add(-7 * 24 * time.Hour), MaxAge: 30 * 24 * time.Hour,
			TableCount: 25, MigrationCount: 10, LatestMigration: "0010",
			TechnicalRestoreVerified: true, SemanticInspection: "not_proven",
		}},
		IntegrityStatus: metricsIntegrityReader{status: ports.IntegrityStatus{
			Verified: true, SchemaVersion: "0010", CheckedAt: now.Add(-30 * time.Minute), MaxAge: 25 * time.Hour, RulesChecked: 29,
		}},
		ExternalHealth: func() externalHealthMetrics {
			return externalHealthMetrics{
				Configured: true, Available: true, SampledAt: now.Add(-15 * time.Second), FreshnessBudget: 40 * time.Minute,
				Sources: 2, Documents: 37, Failed: 1, Stale: 1, PendingRequests: 1, ActiveClaims: 1,
			}
		},
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != prometheusContentType {
		t.Fatalf("metrics response = %d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("metrics security headers = %v", rec.Header())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`claracore_build_info{version="v0.2.0",source_digest="` + strings.Repeat("a", 64) + `",binary_digest="` + strings.Repeat("b", 64) + `",environment="production"} 1`,
		"claracore_process_uptime_seconds 7200",
		"claracore_instance_accepting_requests 0",
		"claracore_mcp_agent_rate_limit 300",
		"claracore_mcp_tenant_rate_limit 3000",
		"claracore_mcp_rate_window_seconds 60",
		"claracore_http_requests_window 42",
		"claracore_http_server_errors_window 1",
		"claracore_http_request_duration_p95_upper_bound_seconds 0.25",
		"claracore_mysql_connections_in_use 3",
		"claracore_mysql_connection_wait_total 9",
		"claracore_backup_receipt_available 1",
		"claracore_backup_fresh 1",
		"claracore_backup_age_seconds 3600",
		"claracore_restore_receipt_available 1",
		"claracore_restore_fresh 1",
		"claracore_restore_age_seconds 604800",
		"claracore_restore_table_count 25",
		"claracore_restore_migration_count 10",
		"claracore_integrity_receipt_available 1",
		"claracore_integrity_fresh 1",
		"claracore_integrity_verified 1",
		"claracore_integrity_age_seconds 1800",
		"claracore_integrity_rules_checked 29",
		"claracore_integrity_violations 0",
		"claracore_external_monitoring_configured 1",
		"claracore_external_health_snapshot_available 1",
		"claracore_external_health_snapshot_age_seconds 15",
		"claracore_external_freshness_budget_seconds 2400",
		"claracore_external_sources_total 2",
		"claracore_external_documents_active 37",
		"claracore_external_sources_failed 1",
		"claracore_external_sources_stale 1",
		"claracore_external_sync_requests_pending 1",
		"claracore_external_sync_claims_active 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q", want)
		}
	}
	for _, forbidden := range []string{"secret_database", "secret_archive", "secret_restore", "Authorization", "request_id", "route="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("metrics body leaks %q", forbidden)
		}
	}
	if !strings.HasSuffix(body, "\n") {
		t.Fatal("Prometheus text format must end with a line feed")
	}
}

func TestMetricsTreatMissingStaleAndFutureRestoreReceiptsAsUnhealthy(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		reader ports.RestoreStatusReader
		want   string
	}{
		{name: "missing", reader: nil, want: "claracore_restore_receipt_available 0"},
		{name: "invalid", reader: metricsRestoreReader{err: errors.New("invalid")}, want: "claracore_restore_receipt_available 0"},
		{name: "stale", reader: metricsRestoreReader{status: ports.RestoreStatus{
			RestoredAt: now.Add(-31 * 24 * time.Hour), MaxAge: 30 * 24 * time.Hour,
			TechnicalRestoreVerified: true, SemanticInspection: "not_proven",
		}}, want: "claracore_restore_fresh 0"},
		{name: "future clock", reader: metricsRestoreReader{status: ports.RestoreStatus{
			RestoredAt: now.Add(10 * time.Minute), MaxAge: 30 * 24 * time.Hour,
			TechnicalRestoreVerified: true, SemanticInspection: "not_proven",
		}}, want: "claracore_restore_fresh 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderMetrics(metricsOptions{Now: func() time.Time { return now }, RestoreStatus: tc.reader})
			if !strings.Contains(body, tc.want) {
				t.Fatalf("metrics missing %q:\n%s", tc.want, body)
			}
		})
	}
}

func TestMetricsTreatMissingStaleAndFutureBackupReceiptsAsUnhealthy(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		reader ports.BackupStatusReader
		want   string
	}{
		{name: "missing", reader: nil, want: "claracore_backup_receipt_available 0"},
		{name: "invalid", reader: metricsBackupReader{err: errors.New("invalid")}, want: "claracore_backup_receipt_available 0"},
		{name: "stale", reader: metricsBackupReader{status: ports.BackupStatus{
			CreatedAt: now.Add(-27 * time.Hour), VerifiedAt: now.Add(-27 * time.Hour), MaxAge: 26 * time.Hour,
			IntegrityVerified: true, Recoverability: "not_proven",
		}}, want: "claracore_backup_fresh 0"},
		{name: "future clock", reader: metricsBackupReader{status: ports.BackupStatus{
			CreatedAt: now.Add(10 * time.Minute), VerifiedAt: now.Add(10 * time.Minute), MaxAge: 26 * time.Hour,
			IntegrityVerified: true, Recoverability: "not_proven",
		}}, want: "claracore_backup_fresh 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderMetrics(metricsOptions{Now: func() time.Time { return now }, BackupStatus: tc.reader})
			if !strings.Contains(body, tc.want) {
				t.Fatalf("metrics missing %q:\n%s", tc.want, body)
			}
		})
	}
}

func TestMetricsExposeIntegrityMissingStaleSchemaAndViolations(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		reader ports.IntegrityStatusReader
		want   []string
	}{
		{name: "missing", want: []string{"claracore_integrity_receipt_available 0", "claracore_integrity_verified 0"}},
		{name: "stale", reader: metricsIntegrityReader{status: ports.IntegrityStatus{Verified: true, SchemaVersion: "0010", CheckedAt: now.Add(-26 * time.Hour), MaxAge: 25 * time.Hour, RulesChecked: 29}}, want: []string{"claracore_integrity_receipt_available 1", "claracore_integrity_fresh 0", "claracore_integrity_verified 1"}},
		{name: "schema mismatch", reader: metricsIntegrityReader{status: ports.IntegrityStatus{Verified: true, SchemaVersion: "0009", CheckedAt: now, MaxAge: 25 * time.Hour, RulesChecked: 27}}, want: []string{"claracore_integrity_receipt_available 0"}},
		{name: "violations", reader: metricsIntegrityReader{status: ports.IntegrityStatus{SchemaVersion: "0010", CheckedAt: now, MaxAge: 25 * time.Hour, RulesChecked: 29, Violations: 8, Issues: []ports.IntegrityIssue{{Domain: "knowledge", Rule: "memory_space", Violations: 8}}}}, want: []string{"claracore_integrity_receipt_available 1", "claracore_integrity_fresh 1", "claracore_integrity_verified 0", "claracore_integrity_violations 8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderMetrics(metricsOptions{Now: func() time.Time { return now }, SchemaVersion: "0010", IntegrityStatus: tc.reader})
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Fatalf("metrics missing %q:\n%s", want, body)
				}
			}
		})
	}
}

func TestMetricsEscapeStaticLabelsAndExposeNoOtherRoutes(t *testing.T) {
	if got := metricLabel("v\\\"\nnext"); got != `v\\\"\nnext` {
		t.Fatalf("escaped label = %q", got)
	}
	handler := newMetricsHandler(metricsOptions{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("metrics listener must expose only /metrics, got %d", rec.Code)
	}
}

var _ ports.BackupStatusReader = metricsBackupReader{}
var _ ports.RestoreStatusReader = metricsRestoreReader{}
var _ ports.IntegrityStatusReader = metricsIntegrityReader{}
