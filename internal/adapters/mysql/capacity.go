package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

type TenantCapacityReader struct {
	db *sql.DB
}

func NewTenantCapacityReader(db *sql.DB) *TenantCapacityReader {
	return &TenantCapacityReader{db: db}
}

// ReadTenantCapacity computes one exact repeatable-read snapshot. The queries
// return counts and byte totals only; no tenant content leaves the adapter.
func (r *TenantCapacityReader) ReadTenantCapacity(ctx context.Context, tenantID platform.ID) (ports.TenantCapacitySnapshot, error) {
	if r == nil || r.db == nil || tenantID == "" {
		return ports.TenantCapacitySnapshot{}, fmt.Errorf("mysql: invalid tenant capacity input")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return ports.TenantCapacitySnapshot{}, fmt.Errorf("mysql: begin tenant capacity snapshot: %w", err)
	}
	defer tx.Rollback()

	var out ports.TenantCapacitySnapshot
	if err := tx.QueryRowContext(ctx, `
		SELECT UTC_TIMESTAMP(3), members, agents, spaces
		FROM (SELECT COUNT(*) AS members FROM users WHERE tenant_id = ?) AS member_totals
		CROSS JOIN (SELECT COUNT(*) AS agents FROM agents WHERE tenant_id = ?) AS agent_totals
		CROSS JOIN (SELECT COUNT(*) AS spaces FROM spaces WHERE tenant_id = ?) AS space_totals`,
		tenantID, tenantID, tenantID,
	).Scan(&out.MeasuredAt, &out.Members, &out.Agents, &out.Spaces); err != nil {
		return ports.TenantCapacitySnapshot{}, fmt.Errorf("mysql: read tenant governance capacity: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `
		SELECT memories, cards, triggers, labels, links,
			memory_bytes + card_bytes + trigger_bytes + label_bytes + link_bytes
		FROM (SELECT COUNT(*) AS memories, COALESCE(SUM(OCTET_LENGTH(title) + OCTET_LENGTH(body)), 0) AS memory_bytes FROM memories WHERE tenant_id = ?) AS memory_totals
		CROSS JOIN (SELECT COUNT(*) AS cards, COALESCE(SUM(OCTET_LENGTH(title) + OCTET_LENGTH(card_type) + COALESCE(OCTET_LENGTH(source), 0) + COALESCE(OCTET_LENGTH(body_original), 0) + COALESCE(OCTET_LENGTH(body_distilled), 0) + COALESCE(OCTET_LENGTH(body_understanding), 0) + COALESCE(OCTET_LENGTH(body_why_keep), 0) + COALESCE(OCTET_LENGTH(body_unresolved), 0)), 0) AS card_bytes FROM cards WHERE tenant_id = ?) AS card_totals
		CROSS JOIN (SELECT COUNT(*) AS triggers, COALESCE(SUM(OCTET_LENGTH(trigger_text)), 0) AS trigger_bytes FROM card_recall_triggers WHERE tenant_id = ?) AS trigger_totals
		CROSS JOIN (SELECT COUNT(*) AS labels, COALESCE(SUM(OCTET_LENGTH(label)), 0) AS label_bytes FROM labels WHERE tenant_id = ?) AS label_totals
		CROSS JOIN (SELECT COUNT(*) AS links, COALESCE(SUM(COALESCE(OCTET_LENGTH(note), 0)), 0) AS link_bytes FROM links WHERE tenant_id = ?) AS link_totals`,
		tenantID, tenantID, tenantID, tenantID, tenantID,
	).Scan(&out.Memories, &out.Cards, &out.RecallTriggers, &out.Labels, &out.Links, &out.KnowledgeLogicalBytes); err != nil {
		return ports.TenantCapacitySnapshot{}, fmt.Errorf("mysql: read tenant knowledge capacity: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `
		SELECT line_count, position_count, history_count, handoff_count, line_bytes + position_bytes + history_bytes + handoff_bytes
		FROM (SELECT COUNT(*) AS line_count, COALESCE(SUM(OCTET_LENGTH(title)), 0) AS line_bytes FROM continuity_lines WHERE tenant_id = ?) AS line_totals
		CROSS JOIN (SELECT COUNT(*) AS position_count, COALESCE(SUM(OCTET_LENGTH(summary) + OCTET_LENGTH(interpretation_status) + OCTET_LENGTH(facts_used) + OCTET_LENGTH(next_step)), 0) AS position_bytes FROM continuity_positions WHERE tenant_id = ?) AS position_totals
		CROSS JOIN (SELECT COUNT(*) AS history_count, COALESCE(SUM(OCTET_LENGTH(summary) + OCTET_LENGTH(interpretation_status) + OCTET_LENGTH(facts_used) + OCTET_LENGTH(next_step)), 0) AS history_bytes FROM continuity_position_history WHERE tenant_id = ?) AS history_totals
		CROSS JOIN (SELECT COUNT(*) AS handoff_count, COALESCE(SUM(OCTET_LENGTH(objective) + OCTET_LENGTH(completed) + OCTET_LENGTH(open_items) + OCTET_LENGTH(next_step)), 0) AS handoff_bytes FROM continuity_handoffs WHERE tenant_id = ?) AS handoff_totals`,
		tenantID, tenantID, tenantID, tenantID,
	).Scan(&out.ContinuityLines, &out.ContinuityPositions, &out.ContinuityHistory, &out.ContinuityHandoffs, &out.ContinuityLogicalBytes); err != nil {
		return ports.TenantCapacitySnapshot{}, fmt.Errorf("mysql: read tenant continuity capacity: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `
		SELECT embeddings, embedding_bytes, sources, runs, documents, vectors,
			source_bytes + run_bytes + document_bytes + vector_bytes
		FROM (SELECT COUNT(*) AS embeddings, COALESCE(SUM(OCTET_LENGTH(source_text) + COALESCE(OCTET_LENGTH(vector), 0)), 0) AS embedding_bytes FROM embeddings WHERE tenant_id = ?) AS embedding_totals
		CROSS JOIN (SELECT COUNT(*) AS sources, COALESCE(SUM(OCTET_LENGTH(name) + OCTET_LENGTH(source_system) + COALESCE(OCTET_LENGTH(last_error), 0)), 0) AS source_bytes FROM external_sources WHERE tenant_id = ?) AS source_totals
		CROSS JOIN (SELECT COUNT(*) AS runs, COALESCE(SUM(COALESCE(OCTET_LENGTH(error_message), 0)), 0) AS run_bytes FROM external_sync_runs WHERE tenant_id = ?) AS run_totals
		CROSS JOIN (SELECT COUNT(*) AS documents, COALESCE(SUM(OCTET_LENGTH(upstream_source_id) + OCTET_LENGTH(external_id) + OCTET_LENGTH(kind) + OCTET_LENGTH(title) + OCTET_LENGTH(source_ref) + OCTET_LENGTH(revision) + COALESCE(OCTET_LENGTH(upstream_updated_at), 0)), 0) AS document_bytes FROM external_documents WHERE tenant_id = ?) AS document_totals
		CROSS JOIN (SELECT COUNT(*) AS vectors, COALESCE(SUM(OCTET_LENGTH(vector)), 0) AS vector_bytes FROM external_document_vectors WHERE tenant_id = ?) AS vector_totals`,
		tenantID, tenantID, tenantID, tenantID, tenantID,
	).Scan(&out.Embeddings, &out.EmbeddingLogicalBytes, &out.ExternalSources, &out.ExternalSyncRuns, &out.ExternalDocuments, &out.ExternalVectors, &out.ExternalLogicalBytes); err != nil {
		return ports.TenantCapacitySnapshot{}, fmt.Errorf("mysql: read tenant index and external capacity: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(OCTET_LENGTH(action) + COALESCE(OCTET_LENGTH(metadata), 0)), 0)
		FROM audit_logs WHERE tenant_id = ?`, tenantID,
	).Scan(&out.AuditEvents, &out.AuditLogicalBytes); err != nil {
		return ports.TenantCapacitySnapshot{}, fmt.Errorf("mysql: read tenant audit capacity: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ports.TenantCapacitySnapshot{}, fmt.Errorf("mysql: finish tenant capacity snapshot: %w", err)
	}
	return out, nil
}
