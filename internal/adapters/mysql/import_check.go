package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"claracore-server/internal/portableexport"
	"claracore-server/internal/teamimport"
)

var ErrTenantImportReceiptNotProven = errors.New("mysql: pristine team import receipt is not proven")

type TenantImportCheckInput struct {
	AdminUsername  string
	EmbeddingModel string
}

type TenantImportCheckResult struct {
	OK                        bool                     `json:"ok"`
	Status                    string                   `json:"status"`
	TenantID                  string                   `json:"tenantId"`
	TenantName                string                   `json:"tenantName"`
	AdminUsername             string                   `json:"adminUsername"`
	Records                   int64                    `json:"records"`
	MembersDisabled           int64                    `json:"membersDisabled"`
	Datasets                  []teamimport.DatasetPlan `json:"datasets"`
	SourceCredentialsImported bool                     `json:"sourceCredentialsImported"`
	ExcludedRuntimeRows       int64                    `json:"excludedRuntimeRows"`
	ImportAuditID             string                   `json:"importAuditId"`
	ImportedAt                time.Time                `json:"importedAt"`
	IndexingStatus            string                   `json:"indexingStatus"`
	IndexingQueued            int64                    `json:"indexingQueued"`
	IndexModel                string                   `json:"indexModel"`
	IndexingPending           int64                    `json:"indexingPending"`
	IndexingReady             int64                    `json:"indexingReady"`
	IndexingFailed            int64                    `json:"indexingFailed"`
}

type TenantImportChecker struct {
	db            *sql.DB
	schemaVersion string
}

func NewTenantImportChecker(db *sql.DB, schemaVersion string) *TenantImportChecker {
	return &TenantImportChecker{db: db, schemaVersion: schemaVersion}
}

// Check reconstructs a strict, read-only receipt for the short recovery
// window after -import-export committed but before the normal service starts.
// It deliberately rejects a deployment that has already accepted normal
// traffic, because tokens, sessions, changed passwords, or processed vectors
// can no longer prove the pristine post-import boundary.
func (c *TenantImportChecker) Check(ctx context.Context, in TenantImportCheckInput) (TenantImportCheckResult, error) {
	if c == nil || c.db == nil || strings.TrimSpace(c.schemaVersion) == "" {
		return TenantImportCheckResult{}, errors.New("mysql: invalid team import checker dependencies")
	}
	if in.AdminUsername == "" || in.AdminUsername != strings.TrimSpace(in.AdminUsername) {
		return TenantImportCheckResult{}, fmt.Errorf("%w: import administrator username is required without surrounding whitespace", ErrTenantImportReceiptNotProven)
	}
	if in.EmbeddingModel != strings.TrimSpace(in.EmbeddingModel) || len([]rune(in.EmbeddingModel)) > 64 {
		return TenantImportCheckResult{}, fmt.Errorf("%w: embedding model is invalid", ErrTenantImportReceiptNotProven)
	}
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return TenantImportCheckResult{}, fmt.Errorf("mysql: begin team import receipt check: %w", err)
	}
	defer tx.Rollback()

	tenantID, tenantName, err := checkImportedTenant(ctx, tx)
	if err != nil {
		return TenantImportCheckResult{}, err
	}
	audit, err := checkImportAudit(ctx, tx, tenantID, c.schemaVersion)
	if err != nil {
		return TenantImportCheckResult{}, err
	}
	if err := checkImportAdministrator(ctx, tx, tenantID, audit.UserID, in.AdminUsername); err != nil {
		return TenantImportCheckResult{}, err
	}
	disabled, err := checkDisabledImportedMembers(ctx, tx, tenantID, audit.UserID, audit.Metadata.MembersDisabled)
	if err != nil {
		return TenantImportCheckResult{}, err
	}
	datasets, records, err := countImportedDatasets(ctx, tx, tenantID)
	if err != nil {
		return TenantImportCheckResult{}, err
	}
	if records != audit.Metadata.Records {
		return TenantImportCheckResult{}, receiptFailure("dataset records=%d, import audit records=%d", records, audit.Metadata.Records)
	}
	excludedRows, err := countExcludedRuntimeRows(ctx, tx, tenantID)
	if err != nil {
		return TenantImportCheckResult{}, err
	}
	if excludedRows != 0 || audit.Metadata.SourceCredentialsImported {
		return TenantImportCheckResult{}, receiptFailure("excluded runtime rows=%d or source credentials claim is invalid", excludedRows)
	}
	pending, ready, failed, totalEmbeddings, invalidEmbeddings, err := countImportEmbeddings(ctx, tx, tenantID, in.EmbeddingModel)
	if err != nil {
		return TenantImportCheckResult{}, err
	}
	if err := validateImportIndexReceipt(audit.Metadata, in.EmbeddingModel, pending, ready, failed, totalEmbeddings, invalidEmbeddings); err != nil {
		return TenantImportCheckResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return TenantImportCheckResult{}, fmt.Errorf("mysql: finish team import receipt check: %w", err)
	}
	return TenantImportCheckResult{
		OK: true, Status: "verified", TenantID: tenantID, TenantName: tenantName,
		AdminUsername: in.AdminUsername, Records: records, MembersDisabled: disabled, Datasets: datasets,
		SourceCredentialsImported: false, ExcludedRuntimeRows: excludedRows,
		ImportAuditID: audit.ID, ImportedAt: audit.ImportedAt,
		IndexingStatus: audit.Metadata.IndexingStatus, IndexingQueued: audit.Metadata.IndexingQueued,
		IndexModel: audit.Metadata.IndexModel, IndexingPending: pending, IndexingReady: ready, IndexingFailed: failed,
	}, nil
}

func checkImportedTenant(ctx context.Context, tx *sql.Tx) (string, string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,name,status FROM tenants ORDER BY id LIMIT 2`)
	if err != nil {
		return "", "", fmt.Errorf("mysql: read imported tenant: %w", err)
	}
	defer rows.Close()
	var tenantID, tenantName, status string
	count := 0
	for rows.Next() {
		if err := rows.Scan(&tenantID, &tenantName, &status); err != nil {
			return "", "", fmt.Errorf("mysql: scan imported tenant: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return "", "", fmt.Errorf("mysql: iterate imported tenant: %w", err)
	}
	if count != 1 || status != "active" {
		return "", "", receiptFailure("expected exactly one active imported tenant, found %d with status %q", count, status)
	}
	return tenantID, tenantName, nil
}

type importAuditMetadata struct {
	FormatVersion             int    `json:"formatVersion"`
	SchemaVersion             string `json:"schemaVersion"`
	Records                   int64  `json:"records"`
	MembersDisabled           int64  `json:"membersDisabled"`
	SourceCredentialsImported bool   `json:"sourceCredentialsImported"`
	IndexingStatus            string `json:"indexingStatus"`
	IndexingQueued            int64  `json:"indexingQueued"`
	IndexModel                string `json:"indexModel"`
}

type importAuditReceipt struct {
	ID         string
	UserID     string
	ImportedAt time.Time
	Metadata   importAuditMetadata
}

func checkImportAudit(ctx context.Context, tx *sql.Tx, tenantID, schemaVersion string) (importAuditReceipt, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,user_id,entity_id,metadata,created_at FROM audit_logs WHERE tenant_id=? AND source='cli' AND kind='governance' AND access='write' AND outcome='success' AND action='admin.import.apply' ORDER BY created_at,id LIMIT 2`, tenantID)
	if err != nil {
		return importAuditReceipt{}, fmt.Errorf("mysql: read team import audit: %w", err)
	}
	defer rows.Close()
	var receipt importAuditReceipt
	var entityID string
	var metadata []byte
	count := 0
	for rows.Next() {
		if err := rows.Scan(&receipt.ID, &receipt.UserID, &entityID, &metadata, &receipt.ImportedAt); err != nil {
			return importAuditReceipt{}, fmt.Errorf("mysql: scan team import audit: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return importAuditReceipt{}, fmt.Errorf("mysql: iterate team import audit: %w", err)
	}
	if count != 1 || receipt.ID == "" || receipt.UserID == "" || entityID != tenantID {
		return importAuditReceipt{}, receiptFailure("expected exactly one matching import audit, found %d", count)
	}
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt.Metadata); err != nil {
		return importAuditReceipt{}, receiptFailure("decode import audit metadata: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return importAuditReceipt{}, receiptFailure("import audit metadata has trailing content")
	}
	if receipt.Metadata.FormatVersion != portableexport.FormatVersion || receipt.Metadata.SchemaVersion != schemaVersion || receipt.Metadata.Records < 0 || receipt.Metadata.MembersDisabled < 0 || receipt.ImportedAt.IsZero() {
		return importAuditReceipt{}, receiptFailure("import audit metadata does not match the current binary/schema")
	}
	return receipt, nil
}

func checkImportAdministrator(ctx context.Context, tx *sql.Tx, tenantID, auditUserID, username string) error {
	var id, role, status string
	var mustChange bool
	err := tx.QueryRowContext(ctx, `SELECT id,role,status,must_change_pw FROM users WHERE tenant_id=? AND username=?`, tenantID, username).Scan(&id, &role, &status, &mustChange)
	if errors.Is(err, sql.ErrNoRows) {
		return receiptFailure("selected import administrator does not exist")
	}
	if err != nil {
		return fmt.Errorf("mysql: read import administrator: %w", err)
	}
	if id != auditUserID || role != "admin" || status != "active" || !mustChange {
		return receiptFailure("selected import administrator is not in the pristine takeover state")
	}
	return nil
}

func checkDisabledImportedMembers(ctx context.Context, tx *sql.Tx, tenantID, takeoverUserID string, expected int64) (int64, error) {
	var total, disabled, unsafe int64
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(status='disabled'),0),COALESCE(SUM(status<>'disabled' OR must_change_pw<>1),0) FROM users WHERE tenant_id=? AND id<>?`, tenantID, takeoverUserID).Scan(&total, &disabled, &unsafe)
	if err != nil {
		return 0, fmt.Errorf("mysql: check imported members: %w", err)
	}
	if total != expected || disabled != expected || unsafe != 0 {
		return 0, receiptFailure("non-takeover members total=%d disabled=%d unsafe=%d audit=%d", total, disabled, unsafe, expected)
	}
	return disabled, nil
}

var importedDatasetCounts = []struct {
	name  string
	query string
}{
	{"members.jsonl", `SELECT COUNT(*) FROM users WHERE tenant_id=?`},
	{"agents.jsonl", `SELECT COUNT(*) FROM agents WHERE tenant_id=?`},
	{"spaces.jsonl", `SELECT COUNT(*) FROM spaces WHERE tenant_id=?`},
	{"space_permissions.jsonl", `SELECT COUNT(*) FROM space_permissions p JOIN spaces s ON s.id=p.space_id WHERE s.tenant_id=?`},
	{"memories.jsonl", `SELECT COUNT(*) FROM memories WHERE tenant_id=?`},
	{"cards.jsonl", `SELECT COUNT(*) FROM cards WHERE tenant_id=?`},
	{"card_recall_triggers.jsonl", `SELECT COUNT(*) FROM card_recall_triggers WHERE tenant_id=?`},
	{"labels.jsonl", `SELECT COUNT(*) FROM labels WHERE tenant_id=?`},
	{"links.jsonl", `SELECT COUNT(*) FROM links WHERE tenant_id=?`},
	{"shared_lines.jsonl", `SELECT COUNT(*) FROM continuity_lines WHERE tenant_id=?`},
	{"shared_line_positions.jsonl", `SELECT COUNT(*) FROM continuity_positions WHERE tenant_id=?`},
	{"shared_line_position_history.jsonl", `SELECT COUNT(*) FROM continuity_position_history WHERE tenant_id=?`},
	{"shared_line_handoffs.jsonl", `SELECT COUNT(*) FROM continuity_handoffs WHERE tenant_id=?`},
}

func countImportedDatasets(ctx context.Context, tx *sql.Tx, tenantID string) ([]teamimport.DatasetPlan, int64, error) {
	datasets := make([]teamimport.DatasetPlan, 0, len(importedDatasetCounts))
	var total int64
	for _, dataset := range importedDatasetCounts {
		var count int64
		if err := tx.QueryRowContext(ctx, dataset.query, tenantID).Scan(&count); err != nil {
			return nil, 0, fmt.Errorf("mysql: count imported %s: %w", dataset.name, err)
		}
		datasets = append(datasets, teamimport.DatasetPlan{Name: dataset.name, Records: count})
		total += count
	}
	return datasets, total, nil
}

func countExcludedRuntimeRows(ctx context.Context, tx *sql.Tx, tenantID string) (int64, error) {
	queries := []string{
		`SELECT COUNT(*) FROM tokens WHERE tenant_id=?`,
		`SELECT COUNT(*) FROM sessions WHERE tenant_id=?`,
		`SELECT COUNT(*) FROM external_sources WHERE tenant_id=?`,
		`SELECT COUNT(*) FROM external_sync_runs WHERE tenant_id=?`,
		`SELECT COUNT(*) FROM external_documents WHERE tenant_id=?`,
		`SELECT COUNT(*) FROM external_document_vectors WHERE tenant_id=?`,
	}
	var total int64
	for _, query := range queries {
		var count int64
		if err := tx.QueryRowContext(ctx, query, tenantID).Scan(&count); err != nil {
			return 0, fmt.Errorf("mysql: count excluded import runtime: %w", err)
		}
		total += count
	}
	var otherAudits int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs WHERE tenant_id=? AND NOT (source='cli' AND kind='governance' AND access='write' AND outcome='success' AND action='admin.import.apply')`, tenantID).Scan(&otherAudits); err != nil {
		return 0, fmt.Errorf("mysql: count non-import audit rows: %w", err)
	}
	return total + otherAudits, nil
}

func countImportEmbeddings(ctx context.Context, tx *sql.Tx, tenantID, expectedModel string) (pending, ready, failed, total, invalid int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(status='pending'),0),COALESCE(SUM(status='ready'),0),COALESCE(SUM(status='failed'),0),COUNT(*),COALESCE(SUM(model<>? OR attempts<>0 OR vector IS NOT NULL OR last_error IS NOT NULL),0) FROM embeddings WHERE tenant_id=?`, expectedModel, tenantID).Scan(&pending, &ready, &failed, &total, &invalid)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("mysql: count imported embeddings: %w", err)
	}
	return pending, ready, failed, total, invalid, nil
}

func validateImportIndexReceipt(metadata importAuditMetadata, expectedModel string, pending, ready, failed, total, invalid int64) error {
	if metadata.IndexModel != expectedModel || metadata.IndexingQueued < 0 {
		return receiptFailure("import index model or queued count differs from current configuration")
	}
	switch metadata.IndexingStatus {
	case "queued":
		if expectedModel == "" || metadata.IndexingQueued == 0 || total != metadata.IndexingQueued || pending != total || ready != 0 || failed != 0 || invalid != 0 {
			return receiptFailure("queued import index is not in the pristine pending state")
		}
	case "backfill_required":
		if expectedModel != "" || metadata.IndexingQueued != 0 || total != 0 {
			return receiptFailure("backfill-required import index boundary is inconsistent")
		}
	case "not_required":
		if metadata.IndexingQueued != 0 || total != 0 {
			return receiptFailure("not-required import index boundary is inconsistent")
		}
	default:
		return receiptFailure("unknown import indexing status %q", metadata.IndexingStatus)
	}
	return nil
}

func receiptFailure(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTenantImportReceiptNotProven, fmt.Sprintf(format, args...))
}
