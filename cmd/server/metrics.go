package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"claracore-server/internal/ports"
	"claracore-server/internal/web"
)

const prometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

type metricsOptions struct {
	Version            string
	SourceDigest       string
	BinaryDigest       string
	Environment        string
	SchemaVersion      string
	MCPAgentRateLimit  int
	MCPTenantRateLimit int
	MCPRateWindow      time.Duration
	StartedAt          time.Time
	Now                func() time.Time
	Accepting          func() bool
	HTTPStats          func() web.HTTPRequestStats
	DatabasePool       func() sql.DBStats
	BackupStatus       ports.BackupStatusReader
	RestoreStatus      ports.RestoreStatusReader
	IntegrityStatus    ports.IntegrityStatusReader
	ExternalHealth     func() externalHealthMetrics
}

// newMetricsHandler exposes only constant-cardinality operational state. It
// never reads MySQL, Redis, request logs, identities, routes, or business data.
// The caller serves it on a separately validated loopback-only listener.
func newMetricsHandler(opts metricsOptions) http.Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", prometheusContentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(renderMetrics(opts)))
	})
	return mux
}

func renderMetrics(opts metricsOptions) string {
	now := opts.Now().UTC()
	var out strings.Builder
	writeMetric(&out, "claracore_build_info", "Static build and environment information.", "gauge",
		fmt.Sprintf("{version=\"%s\",source_digest=\"%s\",binary_digest=\"%s\",environment=\"%s\"}",
			metricLabel(opts.Version), metricLabel(opts.SourceDigest), metricLabel(opts.BinaryDigest), metricLabel(opts.Environment)), 1)
	uptime := now.Sub(opts.StartedAt)
	if opts.StartedAt.IsZero() || uptime < 0 {
		uptime = 0
	}
	writeMetric(&out, "claracore_process_uptime_seconds", "Seconds since this process started.", "gauge", "", uptime.Seconds())
	accepting := float64(1)
	if opts.Accepting != nil && !opts.Accepting() {
		accepting = 0
	}
	writeMetric(&out, "claracore_instance_accepting_requests", "Whether the instance is accepting new business requests.", "gauge", "", accepting)
	writeMetric(&out, "claracore_mcp_agent_rate_limit", "Authenticated MCP requests allowed per agent in one fixed window.", "gauge", "", float64(opts.MCPAgentRateLimit))
	writeMetric(&out, "claracore_mcp_tenant_rate_limit", "Authenticated MCP requests allowed across one tenant in one fixed window.", "gauge", "", float64(opts.MCPTenantRateLimit))
	writeMetric(&out, "claracore_mcp_rate_window_seconds", "Fixed window shared by authenticated MCP agent and tenant budgets.", "gauge", "", opts.MCPRateWindow.Seconds())

	stats := web.HTTPRequestStats{}
	if opts.HTTPStats != nil {
		stats = opts.HTTPStats()
	}
	writeBoolMetric(&out, "claracore_http_window_available", "Whether the bounded HTTP health window is available.", stats.Available)
	writeMetric(&out, "claracore_http_window_seconds", "Width of the rolling HTTP health window.", "gauge", "", stats.Window.Seconds())
	writeMetric(&out, "claracore_http_requests_window", "Completed non-probe requests in the rolling window.", "gauge", "", float64(stats.Requests))
	writeMetric(&out, "claracore_http_client_errors_window", "Completed 4xx requests in the rolling window.", "gauge", "", float64(stats.ClientErrors))
	writeMetric(&out, "claracore_http_server_errors_window", "Completed 5xx requests in the rolling window.", "gauge", "", float64(stats.ServerErrors))
	writeMetric(&out, "claracore_http_panics_window", "Recovered or connection-aborting panics in the rolling window.", "gauge", "", float64(stats.Panics))
	writeMetric(&out, "claracore_http_in_flight", "Current in-flight business and probe requests.", "gauge", "", float64(stats.InFlight))
	writeMetric(&out, "claracore_http_request_duration_average_seconds", "Average request duration in the rolling window.", "gauge", "", stats.AverageLatency.Seconds())
	writeMetric(&out, "claracore_http_request_duration_p95_upper_bound_seconds", "Upper bound of the approximate p95 duration bucket.", "gauge", "", stats.P95Latency.Seconds())
	writeBoolMetric(&out, "claracore_http_request_duration_p95_overflow", "Whether p95 exceeded the largest tracked bucket.", stats.P95Overflow)

	poolAvailable := opts.DatabasePool != nil
	writeBoolMetric(&out, "claracore_mysql_pool_available", "Whether database/sql pool statistics are wired.", poolAvailable)
	if poolAvailable {
		pool := opts.DatabasePool()
		writeMetric(&out, "claracore_mysql_connections_max", "Configured maximum open MySQL connections.", "gauge", "", float64(pool.MaxOpenConnections))
		writeMetric(&out, "claracore_mysql_connections_open", "Current open MySQL connections.", "gauge", "", float64(pool.OpenConnections))
		writeMetric(&out, "claracore_mysql_connections_in_use", "Current in-use MySQL connections.", "gauge", "", float64(pool.InUse))
		writeMetric(&out, "claracore_mysql_connections_idle", "Current idle MySQL connections.", "gauge", "", float64(pool.Idle))
		writeMetric(&out, "claracore_mysql_connection_wait_total", "Cumulative waits for a MySQL connection.", "counter", "", float64(pool.WaitCount))
		writeMetric(&out, "claracore_mysql_connection_wait_seconds_total", "Cumulative time waiting for a MySQL connection.", "counter", "", pool.WaitDuration.Seconds())
		writeMetric(&out, "claracore_mysql_connections_closed_idle_total", "Connections closed for exceeding MaxIdleConns.", "counter", "", float64(pool.MaxIdleClosed))
		writeMetric(&out, "claracore_mysql_connections_closed_idle_time_total", "Connections closed for exceeding ConnMaxIdleTime.", "counter", "", float64(pool.MaxIdleTimeClosed))
		writeMetric(&out, "claracore_mysql_connections_closed_lifetime_total", "Connections closed for exceeding ConnMaxLifetime.", "counter", "", float64(pool.MaxLifetimeClosed))
	}

	writeBackupMetrics(&out, opts.BackupStatus, now)
	writeRestoreMetrics(&out, opts.RestoreStatus, now)
	writeIntegrityMetrics(&out, opts.IntegrityStatus, opts.SchemaVersion, now)
	writeExternalHealthMetrics(&out, opts.ExternalHealth, now)
	return out.String()
}

func writeExternalHealthMetrics(out *strings.Builder, snapshot func() externalHealthMetrics, now time.Time) {
	metrics := externalHealthMetrics{}
	if snapshot != nil {
		metrics = snapshot()
	}
	writeBoolMetric(out, "claracore_external_monitoring_configured", "Whether Atlas external-source monitoring is configured for this deployment.", metrics.Configured)
	writeBoolMetric(out, "claracore_external_health_snapshot_available", "Whether the latest bounded external-source health sample succeeded.", metrics.Available)
	age := time.Duration(0)
	if !metrics.SampledAt.IsZero() {
		age = now.Sub(metrics.SampledAt)
		if age < 0 {
			age = 0
		}
	}
	writeMetric(out, "claracore_external_health_snapshot_age_seconds", "Age of the latest external-source health sample.", "gauge", "", age.Seconds())
	writeMetric(out, "claracore_external_freshness_budget_seconds", "Maximum successful-sync age before a source is classified as stale.", "gauge", "", metrics.FreshnessBudget.Seconds())
	writeMetric(out, "claracore_external_sources_total", "Configured active external sources in the sampled tenant.", "gauge", "", float64(metrics.Sources))
	writeMetric(out, "claracore_external_documents_active", "Active body-free external document mirrors across configured sources.", "gauge", "", float64(metrics.Documents))
	writeMetric(out, "claracore_external_sources_running", "External sources whose latest sync is running.", "gauge", "", float64(metrics.Running))
	writeMetric(out, "claracore_external_sources_failed", "External sources whose latest sync failed.", "gauge", "", float64(metrics.Failed))
	writeMetric(out, "claracore_external_sources_never_succeeded", "External sources that have never completed a successful sync.", "gauge", "", float64(metrics.NeverSucceeded))
	writeMetric(out, "claracore_external_sources_stale", "External sources outside the configured successful-sync freshness budget.", "gauge", "", float64(metrics.Stale))
	writeMetric(out, "claracore_external_sync_requests_pending", "Coalesced external sync requests waiting for a worker.", "gauge", "", float64(metrics.PendingRequests))
	writeMetric(out, "claracore_external_sync_claims_active", "External sync requests currently claimed by a worker.", "gauge", "", float64(metrics.ActiveClaims))
	writeMetric(out, "claracore_external_sync_claims_expired", "External sync request claims waiting to be reclaimed.", "gauge", "", float64(metrics.ExpiredClaims))
}

func writeBackupMetrics(out *strings.Builder, reader ports.BackupStatusReader, now time.Time) {
	if reader == nil {
		writeBoolMetric(out, "claracore_backup_receipt_available", "Whether a valid bounded backup verification receipt is available.", false)
		writeBoolMetric(out, "claracore_backup_fresh", "Whether the latest verified backup is within its age budget.", false)
		return
	}
	status, err := reader.Read()
	available := err == nil && status.IntegrityVerified && status.Recoverability == "not_proven" && status.MaxAge > 0
	writeBoolMetric(out, "claracore_backup_receipt_available", "Whether a valid bounded backup verification receipt is available.", available)
	if !available {
		writeBoolMetric(out, "claracore_backup_fresh", "Whether the latest verified backup is within its age budget.", false)
		return
	}
	age := now.Sub(status.CreatedAt)
	verifiedAge := now.Sub(status.VerifiedAt)
	clockValid := age >= -5*time.Minute && verifiedAge >= -5*time.Minute
	if age < 0 {
		age = 0
	}
	if verifiedAge < 0 {
		verifiedAge = 0
	}
	writeBoolMetric(out, "claracore_backup_fresh", "Whether the latest verified backup is within its age budget.", clockValid && age <= status.MaxAge)
	writeMetric(out, "claracore_backup_age_seconds", "Age of the latest verified backup archive.", "gauge", "", age.Seconds())
	writeMetric(out, "claracore_backup_verification_age_seconds", "Age of the latest backup verification receipt.", "gauge", "", verifiedAge.Seconds())
	writeMetric(out, "claracore_backup_max_age_seconds", "Configured maximum acceptable backup age.", "gauge", "", status.MaxAge.Seconds())
}

func writeRestoreMetrics(out *strings.Builder, reader ports.RestoreStatusReader, now time.Time) {
	if reader == nil {
		writeBoolMetric(out, "claracore_restore_receipt_available", "Whether a valid bounded isolated-restore receipt is available.", false)
		writeBoolMetric(out, "claracore_restore_fresh", "Whether the latest verified isolated restore is within its policy age budget.", false)
		return
	}
	status, err := reader.Read()
	available := err == nil && status.TechnicalRestoreVerified && status.SemanticInspection == "not_proven" && status.MaxAge > 0
	writeBoolMetric(out, "claracore_restore_receipt_available", "Whether a valid bounded isolated-restore receipt is available.", available)
	if !available {
		writeBoolMetric(out, "claracore_restore_fresh", "Whether the latest verified isolated restore is within its policy age budget.", false)
		return
	}
	age := now.Sub(status.RestoredAt)
	clockValid := age >= -5*time.Minute
	if age < 0 {
		age = 0
	}
	writeBoolMetric(out, "claracore_restore_fresh", "Whether the latest verified isolated restore is within its policy age budget.", clockValid && age <= status.MaxAge)
	writeMetric(out, "claracore_restore_age_seconds", "Age of the latest verified isolated restore rehearsal.", "gauge", "", age.Seconds())
	writeMetric(out, "claracore_restore_max_age_seconds", "Configured maximum acceptable isolated-restore rehearsal age.", "gauge", "", status.MaxAge.Seconds())
	writeMetric(out, "claracore_restore_table_count", "Tables enumerated and checked in the latest isolated restore.", "gauge", "", float64(status.TableCount))
	writeMetric(out, "claracore_restore_migration_count", "Migration ledger rows enumerated in the latest isolated restore.", "gauge", "", float64(status.MigrationCount))
}

func writeIntegrityMetrics(out *strings.Builder, reader ports.IntegrityStatusReader, schemaVersion string, now time.Time) {
	if reader == nil {
		writeBoolMetric(out, "claracore_integrity_receipt_available", "Whether a valid bounded data-integrity receipt is available.", false)
		writeBoolMetric(out, "claracore_integrity_fresh", "Whether the latest data-integrity check is within its age budget.", false)
		writeBoolMetric(out, "claracore_integrity_verified", "Whether the latest completed data-integrity check found zero violations.", false)
		return
	}
	status, err := reader.Read()
	available := err == nil && status.MaxAge > 0 && status.RulesChecked > 0 && schemaVersion != "" && status.SchemaVersion == schemaVersion
	writeBoolMetric(out, "claracore_integrity_receipt_available", "Whether a valid bounded data-integrity receipt is available.", available)
	if !available {
		writeBoolMetric(out, "claracore_integrity_fresh", "Whether the latest data-integrity check is within its age budget.", false)
		writeBoolMetric(out, "claracore_integrity_verified", "Whether the latest completed data-integrity check found zero violations.", false)
		return
	}
	age := now.Sub(status.CheckedAt)
	clockValid := age >= -5*time.Minute
	if age < 0 {
		age = 0
	}
	writeBoolMetric(out, "claracore_integrity_fresh", "Whether the latest data-integrity check is within its age budget.", clockValid && age <= status.MaxAge)
	writeBoolMetric(out, "claracore_integrity_verified", "Whether the latest completed data-integrity check found zero violations.", status.Verified && status.Violations == 0)
	writeMetric(out, "claracore_integrity_age_seconds", "Age of the latest completed data-integrity check.", "gauge", "", age.Seconds())
	writeMetric(out, "claracore_integrity_max_age_seconds", "Configured maximum acceptable data-integrity check age.", "gauge", "", status.MaxAge.Seconds())
	writeMetric(out, "claracore_integrity_rules_checked", "Fixed relationship rules completed by the latest data-integrity check.", "gauge", "", float64(status.RulesChecked))
	writeMetric(out, "claracore_integrity_violations", "Rule violation count from the latest data-integrity check; overlapping rules may count one row more than once.", "gauge", "", float64(status.Violations))
}

func writeBoolMetric(out *strings.Builder, name, help string, value bool) {
	number := float64(0)
	if value {
		number = 1
	}
	writeMetric(out, name, help, "gauge", "", number)
}

func writeMetric(out *strings.Builder, name, help, metricType, labels string, value float64) {
	fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s %s\n%s%s %s\n",
		name, help, name, metricType, name, labels, strconv.FormatFloat(value, 'g', -1, 64))
}

func metricLabel(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"")
	return replacer.Replace(value)
}
