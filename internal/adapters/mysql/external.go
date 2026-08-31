package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"claracore-server/internal/domain/external"
	"claracore-server/internal/platform"
)

// ExternalStore persists only mirror metadata, sync receipts, and derived
// vectors. No method accepts an upstream document body.
type ExternalStore struct {
	db *sql.DB
}

var _ external.Store = (*ExternalStore)(nil)

func NewExternalStore(db *sql.DB) *ExternalStore { return &ExternalStore{db: db} }

func (s *ExternalStore) EnsureSource(ctx context.Context, source external.MirrorSource) (external.MirrorSource, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return external.MirrorSource{}, fmt.Errorf("mysql: begin external source registration: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var spaceCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM spaces WHERE id = ? AND tenant_id = ?`, string(source.SpaceID), string(source.TenantID)).Scan(&spaceCount); err != nil {
		return external.MirrorSource{}, fmt.Errorf("mysql: validate external source space: %w", err)
	}
	if spaceCount != 1 {
		return external.MirrorSource{}, errors.New("mysql: external source target space not found in tenant")
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO external_sources
		   (id, tenant_id, space_id, kind, name, source_system, status, last_sync_status,
		    last_started_at, last_succeeded_at, last_error, document_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, 0, ?, ?) AS v
		 ON DUPLICATE KEY UPDATE updated_at = v.updated_at`,
		string(source.ID), string(source.TenantID), string(source.SpaceID), source.Kind, source.Name,
		source.System, source.Status, external.SyncNever, source.CreatedAt.UTC(), source.UpdatedAt.UTC())
	if err != nil {
		return external.MirrorSource{}, fmt.Errorf("mysql: register external source: %w", err)
	}
	var out external.MirrorSource
	var id, tenantID, spaceID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id, tenant_id, space_id, kind, name, source_system, status, created_at, updated_at
		 FROM external_sources WHERE tenant_id = ? AND name = ?`,
		string(source.TenantID), source.Name,
	).Scan(&id, &tenantID, &spaceID, &out.Kind, &out.Name, &out.System, &out.Status, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return external.MirrorSource{}, fmt.Errorf("mysql: load external source: %w", err)
	}
	out.ID, out.TenantID, out.SpaceID = platform.ID(id), platform.ID(tenantID), platform.ID(spaceID)
	out.CreatedAt, out.UpdatedAt = out.CreatedAt.UTC(), out.UpdatedAt.UTC()
	if out.SpaceID != source.SpaceID || out.Kind != source.Kind || out.System != source.System || out.Status != external.SourceActive {
		return external.MirrorSource{}, errors.New("mysql: external source name already exists with different immutable configuration")
	}
	if err := tx.Commit(); err != nil {
		return external.MirrorSource{}, fmt.Errorf("mysql: commit external source registration: %w", err)
	}
	return out, nil
}

func (s *ExternalStore) QueueSyncRequest(ctx context.Context, request external.SyncRequest) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE external_sources
		 SET sync_request_id = ?, sync_requested_at = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ? AND status = ? AND sync_request_id IS NULL`,
		string(request.ID), request.RequestedAt.UTC(), request.RequestedAt.UTC(),
		string(request.ExternalSourceID), string(request.TenantID), external.SourceActive)
	if err != nil {
		return false, fmt.Errorf("mysql: queue external sync request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mysql: count queued external sync request: %w", err)
	}
	return n == 1, nil
}

func (s *ExternalStore) ClaimSyncRequest(ctx context.Context, tenantID platform.ID, sourceName string, claimToken platform.ID, leaseUntil, at time.Time) (external.SyncRequestClaim, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return external.SyncRequestClaim{}, false, fmt.Errorf("mysql: begin external sync request claim: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var sourceID string
	var pendingID, claimedID sql.NullString
	var claimUntil sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT id, sync_request_id, sync_request_claim_id, sync_request_claim_until
		 FROM external_sources
		 WHERE tenant_id = ? AND name = ? AND status = ?
		 FOR UPDATE`, string(tenantID), sourceName, external.SourceActive,
	).Scan(&sourceID, &pendingID, &claimedID, &claimUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return external.SyncRequestClaim{}, false, nil
	}
	if err != nil {
		return external.SyncRequestClaim{}, false, fmt.Errorf("mysql: inspect external sync request claim: %w", err)
	}
	if claimedID.Valid && claimUntil.Valid && claimUntil.Time.After(at) {
		return external.SyncRequestClaim{}, false, nil
	}
	requestID := claimedID.String
	takePending := requestID == ""
	if takePending {
		requestID = pendingID.String
	}
	if requestID == "" {
		return external.SyncRequestClaim{}, false, nil
	}
	if takePending {
		_, err = tx.ExecContext(ctx,
			`UPDATE external_sources
			 SET sync_request_id = NULL, sync_requested_at = NULL,
			     sync_request_claim_id = ?, sync_request_claim_token = ?, sync_request_claim_until = ?, updated_at = ?
			 WHERE id = ? AND tenant_id = ?`,
			requestID, string(claimToken), leaseUntil.UTC(), at.UTC(), sourceID, string(tenantID))
	} else {
		_, err = tx.ExecContext(ctx,
			`UPDATE external_sources
			 SET sync_request_claim_token = ?, sync_request_claim_until = ?, updated_at = ?
			 WHERE id = ? AND tenant_id = ?`,
			string(claimToken), leaseUntil.UTC(), at.UTC(), sourceID, string(tenantID))
	}
	if err != nil {
		return external.SyncRequestClaim{}, false, fmt.Errorf("mysql: claim external sync request: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return external.SyncRequestClaim{}, false, fmt.Errorf("mysql: commit external sync request claim: %w", err)
	}
	return external.SyncRequestClaim{
		RequestID: platform.ID(requestID), TenantID: tenantID, ExternalSourceID: platform.ID(sourceID),
		ClaimToken: claimToken, LeaseUntil: leaseUntil.UTC(),
	}, true, nil
}

func (s *ExternalStore) ResolveSyncRequest(ctx context.Context, claim external.SyncRequestClaim, retry bool, at time.Time) error {
	var (
		res sql.Result
		err error
	)
	if retry {
		res, err = s.db.ExecContext(ctx,
			`UPDATE external_sources
			 SET sync_request_claim_token = NULL, sync_request_claim_until = NULL, updated_at = ?
			 WHERE id = ? AND tenant_id = ? AND sync_request_claim_id = ? AND sync_request_claim_token = ?`,
			at.UTC(), string(claim.ExternalSourceID), string(claim.TenantID), string(claim.RequestID), string(claim.ClaimToken))
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE external_sources
			 SET sync_request_claim_id = NULL, sync_request_claim_token = NULL, sync_request_claim_until = NULL, updated_at = ?
			 WHERE id = ? AND tenant_id = ? AND sync_request_claim_id = ? AND sync_request_claim_token = ?`,
			at.UTC(), string(claim.ExternalSourceID), string(claim.TenantID), string(claim.RequestID), string(claim.ClaimToken))
	}
	if err != nil {
		return fmt.Errorf("mysql: resolve external sync request: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("mysql: external sync request claim was lost")
	}
	return nil
}

func (s *ExternalStore) BeginSync(ctx context.Context, run external.SyncRun) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysql: begin external sync: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx,
		`UPDATE external_sources
		 SET last_sync_status = ?, last_started_at = ?, last_error = NULL,
		     sync_run_id = ?, sync_lease_until = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ? AND status = ?
		   AND (sync_lease_until IS NULL OR sync_lease_until < ?)`,
		external.SyncRunning, run.StartedAt.UTC(), string(run.ID), run.LeaseUntil.UTC(), run.StartedAt.UTC(),
		string(run.ExternalSourceID), string(run.TenantID), external.SourceActive, run.StartedAt.UTC())
	if err != nil {
		return fmt.Errorf("mysql: mark external source syncing: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return external.ErrSyncBusy
	}
	// If this acquisition succeeded by taking over an expired lease, close the
	// abandoned receipt instead of leaving a permanent "running" row on the
	// dashboard. The old owner can no longer finalize because sync_run_id changed.
	if _, err := tx.ExecContext(ctx,
		`UPDATE external_sync_runs
		 SET status = ?, error_message = ?, finished_at = ?
		 WHERE external_source_id = ? AND status = ?`,
		external.SyncFailed, "sync lease expired before completion", run.StartedAt.UTC(),
		string(run.ExternalSourceID), external.SyncRunning); err != nil {
		return fmt.Errorf("mysql: close expired external sync receipt: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO external_sync_runs
		   (id, tenant_id, external_source_id, status, seen_count, indexed_count, unchanged_count,
		    missing_count, error_message, started_at, finished_at, created_at)
		 VALUES (?, ?, ?, ?, 0, 0, 0, 0, NULL, ?, NULL, ?)`,
		string(run.ID), string(run.TenantID), string(run.ExternalSourceID), run.Status, run.StartedAt.UTC(), run.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("mysql: create external sync receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysql: commit external sync start: %w", err)
	}
	return nil
}

func (s *ExternalStore) FindIndexState(ctx context.Context, sourceID platform.ID, externalID, model string) (external.IndexState, bool, error) {
	var state external.IndexState
	var documentID string
	var vectorID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT d.id, d.revision, v.id
		 FROM external_documents d
		 LEFT JOIN external_document_vectors v ON v.external_document_id = d.id AND v.model = ? AND v.revision = d.revision
		 WHERE d.external_source_id = ? AND d.external_id = ?`,
		model, string(sourceID), externalID,
	).Scan(&documentID, &state.Revision, &vectorID)
	if errors.Is(err, sql.ErrNoRows) {
		return external.IndexState{}, false, nil
	}
	if err != nil {
		return external.IndexState{}, false, fmt.Errorf("mysql: inspect external document index: %w", err)
	}
	state.DocumentID, state.Ready = platform.ID(documentID), vectorID.Valid
	return state, true, nil
}

func (s *ExternalStore) TouchDocument(ctx context.Context, document external.MirrorDocument) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE external_documents
		 SET upstream_source_id = ?, kind = ?, title = ?, source_ref = ?, upstream_updated_at = ?,
		     state = ?, last_seen_run_id = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ? AND external_source_id = ? AND external_id = ? AND revision = ?`,
		document.UpstreamSourceID, document.Kind, document.Title, document.SourceRef, nullIfEmpty(document.UpstreamUpdatedAt),
		document.State, string(document.LastSeenRunID), document.UpdatedAt.UTC(), string(document.ID), string(document.TenantID),
		string(document.ExternalSourceID), document.ExternalID, document.Revision)
	if err != nil {
		return fmt.Errorf("mysql: touch external document: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("mysql: external document changed while marking it seen")
	}
	return nil
}

func (s *ExternalStore) SaveIndexedDocument(ctx context.Context, document external.MirrorDocument, vector external.DocumentVector) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysql: begin external document write: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	_, err = tx.ExecContext(ctx,
		`INSERT INTO external_documents
		   (id, tenant_id, space_id, external_source_id, upstream_source_id, external_id, kind, title,
		    source_ref, revision, upstream_updated_at, state, last_seen_run_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) AS v
		 ON DUPLICATE KEY UPDATE
		   tenant_id = v.tenant_id, space_id = v.space_id, upstream_source_id = v.upstream_source_id,
		   kind = v.kind, title = v.title, source_ref = v.source_ref, revision = v.revision,
		   upstream_updated_at = v.upstream_updated_at, state = v.state,
		   last_seen_run_id = v.last_seen_run_id, updated_at = v.updated_at`,
		string(document.ID), string(document.TenantID), string(document.SpaceID), string(document.ExternalSourceID),
		document.UpstreamSourceID, document.ExternalID, document.Kind, document.Title, document.SourceRef,
		document.Revision, nullIfEmpty(document.UpstreamUpdatedAt), document.State, string(document.LastSeenRunID),
		document.CreatedAt.UTC(), document.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("mysql: save external document metadata: %w", err)
	}
	var actualDocumentID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM external_documents WHERE external_source_id = ? AND external_id = ?`,
		string(document.ExternalSourceID), document.ExternalID,
	).Scan(&actualDocumentID); err != nil {
		return fmt.Errorf("mysql: resolve external document identity: %w", err)
	}
	vector.ExternalDocumentID = platform.ID(actualDocumentID)
	_, err = tx.ExecContext(ctx,
		`INSERT INTO external_document_vectors
		   (id, tenant_id, external_document_id, model, revision, input_hash, vector, dim, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) AS v
		 ON DUPLICATE KEY UPDATE revision = v.revision, input_hash = v.input_hash, vector = v.vector,
		   dim = v.dim, updated_at = v.updated_at`,
		string(vector.ID), string(vector.TenantID), actualDocumentID, vector.Model, vector.Revision,
		vector.InputHash, encodeVector(vector.Vector), len(vector.Vector), vector.CreatedAt.UTC(), vector.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("mysql: save external document vector: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysql: commit external document write: %w", err)
	}
	return nil
}

func (s *ExternalStore) CompleteSync(ctx context.Context, sourceID, runID platform.ID, report external.SyncReport, at time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("mysql: begin external sync completion: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx,
		`UPDATE external_documents SET state = ?, updated_at = ?
		 WHERE external_source_id = ? AND state = ? AND last_seen_run_id <> ?`,
		external.DocumentMissing, at.UTC(), string(sourceID), external.DocumentActive, string(runID))
	if err != nil {
		return 0, fmt.Errorf("mysql: mark missing external documents: %w", err)
	}
	missing64, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mysql: count missing external documents: %w", err)
	}
	missing := int(missing64)
	var active int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM external_documents WHERE external_source_id = ? AND state = ?`,
		string(sourceID), external.DocumentActive).Scan(&active); err != nil {
		return 0, fmt.Errorf("mysql: count active external documents: %w", err)
	}
	res, err = tx.ExecContext(ctx,
		`UPDATE external_sync_runs
		 SET status = ?, seen_count = ?, indexed_count = ?, unchanged_count = ?, missing_count = ?,
		     error_message = NULL, finished_at = ?
		 WHERE id = ? AND external_source_id = ? AND status = ?`,
		external.SyncSucceeded, report.Seen, report.Indexed, report.Unchanged, missing, at.UTC(),
		string(runID), string(sourceID), external.SyncRunning)
	if err != nil {
		return 0, fmt.Errorf("mysql: complete external sync receipt: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, errors.New("mysql: external sync receipt was not running")
	}
	res, err = tx.ExecContext(ctx,
		`UPDATE external_sources
		 SET last_sync_status = ?, last_succeeded_at = ?, last_error = NULL, document_count = ?,
		     sync_run_id = NULL, sync_lease_until = NULL, updated_at = ?
		 WHERE id = ? AND sync_run_id = ?`,
		external.SyncSucceeded, at.UTC(), active, at.UTC(), string(sourceID), string(runID))
	if err != nil {
		return 0, fmt.Errorf("mysql: complete external source status: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, external.ErrSyncBusy
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("mysql: commit external sync completion: %w", err)
	}
	return missing, nil
}

func (s *ExternalStore) FailSync(ctx context.Context, sourceID, runID platform.ID, reason string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysql: begin external sync failure receipt: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	_, err = tx.ExecContext(ctx,
		`UPDATE external_sync_runs SET status = ?, error_message = ?, finished_at = ?
		 WHERE id = ? AND external_source_id = ? AND status = ?`,
		external.SyncFailed, reason, at.UTC(), string(runID), string(sourceID), external.SyncRunning)
	if err != nil {
		return fmt.Errorf("mysql: fail external sync receipt: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE external_sources
		 SET last_sync_status = ?, last_error = ?, sync_run_id = NULL, sync_lease_until = NULL, updated_at = ?
		 WHERE id = ? AND sync_run_id = ?`,
		external.SyncFailed, reason, at.UTC(), string(sourceID), string(runID))
	if err != nil {
		return fmt.Errorf("mysql: fail external source status: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysql: commit external sync failure receipt: %w", err)
	}
	return nil
}

func (s *ExternalStore) ListSourceHealth(ctx context.Context, tenantID platform.ID) ([]external.SourceHealth, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, space_id, kind, name, source_system, last_sync_status,
		        last_started_at, last_succeeded_at, last_error, document_count,
		        sync_run_id, sync_lease_until, sync_requested_at, sync_request_claim_id, sync_request_claim_until
		 FROM external_sources
		 WHERE tenant_id = ? AND status = ?
		 ORDER BY name, id`, string(tenantID), external.SourceActive)
	if err != nil {
		return nil, fmt.Errorf("mysql: list external source health: %w", err)
	}
	defer rows.Close()
	var out []external.SourceHealth
	for rows.Next() {
		var health external.SourceHealth
		var id, spaceID string
		var started, succeeded, lease, requested, claimUntil sql.NullTime
		var lastError, runID, claimID sql.NullString
		if err := rows.Scan(&id, &spaceID, &health.Kind, &health.Name, &health.System, &health.LastSyncStatus,
			&started, &succeeded, &lastError, &health.DocumentCount, &runID, &lease, &requested, &claimID, &claimUntil); err != nil {
			return nil, fmt.Errorf("mysql: scan external source health: %w", err)
		}
		health.ID, health.SpaceID = platform.ID(id), platform.ID(spaceID)
		health.LastError, health.SyncRunID = lastError.String, platform.ID(runID.String)
		health.SyncClaimed = claimID.Valid
		if started.Valid {
			t := started.Time.UTC()
			health.LastStartedAt = &t
		}
		if succeeded.Valid {
			t := succeeded.Time.UTC()
			health.LastSucceededAt = &t
		}
		if lease.Valid {
			t := lease.Time.UTC()
			health.SyncLeaseUntil = &t
		}
		if requested.Valid {
			t := requested.Time.UTC()
			health.SyncRequestedAt = &t
		}
		if claimUntil.Valid {
			t := claimUntil.Time.UTC()
			health.SyncClaimUntil = &t
		}
		out = append(out, health)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: iterate external source health: %w", err)
	}
	return out, nil
}

func (s *ExternalStore) ListSyncRuns(ctx context.Context, tenantID platform.ID, limit int) ([]external.SyncRunReceipt, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.external_source_id, s.name, s.source_system, r.status,
		        r.seen_count, r.indexed_count, r.unchanged_count, r.missing_count,
		        r.error_message, r.started_at, r.finished_at
		 FROM external_sync_runs r
		 JOIN external_sources s ON s.id = r.external_source_id AND s.tenant_id = r.tenant_id
		 WHERE r.tenant_id = ?
		 ORDER BY r.started_at DESC, r.id DESC
		 LIMIT ?`, string(tenantID), limit)
	if err != nil {
		return nil, fmt.Errorf("mysql: list external sync receipts: %w", err)
	}
	defer rows.Close()
	var out []external.SyncRunReceipt
	for rows.Next() {
		var receipt external.SyncRunReceipt
		var id, sourceID string
		var message sql.NullString
		var finished sql.NullTime
		if err := rows.Scan(&id, &sourceID, &receipt.SourceName, &receipt.SourceSystem, &receipt.Status,
			&receipt.Seen, &receipt.Indexed, &receipt.Unchanged, &receipt.Missing,
			&message, &receipt.StartedAt, &finished); err != nil {
			return nil, fmt.Errorf("mysql: scan external sync receipt: %w", err)
		}
		receipt.ID, receipt.ExternalSourceID = platform.ID(id), platform.ID(sourceID)
		receipt.ErrorMessage, receipt.StartedAt = message.String, receipt.StartedAt.UTC()
		if finished.Valid {
			t := finished.Time.UTC()
			receipt.FinishedAt = &t
		}
		out = append(out, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: iterate external sync receipts: %w", err)
	}
	return out, nil
}

// SearchCandidates is the body-free first stage for external retrieval. Tenant
// and readable-space predicates are applied in SQL before title, source
// metadata, or vectors enter process memory.
func (s *ExternalStore) SearchCandidates(ctx context.Context, q external.SearchQuery) ([]external.SearchCandidate, error) {
	if len(q.SpaceIDs) == 0 {
		return nil, nil
	}
	args := []any{literalLikePattern(q.Query), string(q.TenantID)}
	args = append(args, idArgs(q.SpaceIDs)...)
	args = append(args, external.DocumentActive, external.SourceActive, q.Model, q.Limit)
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id, d.tenant_id, d.space_id, d.external_source_id,
		        d.upstream_source_id, d.external_id, d.kind, d.title, d.source_ref,
		        d.revision, d.upstream_updated_at, d.state, d.last_seen_run_id,
		        d.created_at, d.updated_at,
		        s.name, s.source_system, s.last_sync_status, s.last_succeeded_at,
		        CASE WHEN d.title LIKE ? ESCAPE '=' THEN 1 ELSE 0 END AS title_score,
		        v.vector, v.dim
		 FROM external_documents d
		 JOIN external_sources s ON s.id = d.external_source_id AND s.tenant_id = d.tenant_id
		 JOIN external_document_vectors v ON v.external_document_id = d.id
		      AND v.tenant_id = d.tenant_id AND v.revision = d.revision
		 WHERE d.tenant_id = ? AND d.space_id IN (`+placeholders(len(q.SpaceIDs))+`)
		   AND d.state = ? AND s.status = ? AND v.model = ?
		 ORDER BY title_score DESC, d.updated_at DESC, d.id ASC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: search external candidates: %w", err)
	}
	defer rows.Close()
	var out []external.SearchCandidate
	for rows.Next() {
		var (
			candidate                                      external.SearchCandidate
			docID, tenantID, spaceID, sourceID, lastSeenID string
			upstreamUpdated                                sql.NullString
			lastSucceeded                                  sql.NullTime
			vectorBytes                                    []byte
			dim                                            int
		)
		if err := rows.Scan(
			&docID, &tenantID, &spaceID, &sourceID,
			&candidate.Document.UpstreamSourceID, &candidate.Document.ExternalID,
			&candidate.Document.Kind, &candidate.Document.Title, &candidate.Document.SourceRef,
			&candidate.Document.Revision, &upstreamUpdated, &candidate.Document.State, &lastSeenID,
			&candidate.Document.CreatedAt, &candidate.Document.UpdatedAt,
			&candidate.SourceName, &candidate.SourceSystem, &candidate.LastSyncStatus, &lastSucceeded,
			&candidate.TitleScore, &vectorBytes, &dim,
		); err != nil {
			return nil, fmt.Errorf("mysql: scan external candidate: %w", err)
		}
		candidate.Document.ID = platform.ID(docID)
		candidate.Document.TenantID = platform.ID(tenantID)
		candidate.Document.SpaceID = platform.ID(spaceID)
		candidate.Document.ExternalSourceID = platform.ID(sourceID)
		candidate.Document.LastSeenRunID = platform.ID(lastSeenID)
		candidate.Document.UpstreamUpdatedAt = upstreamUpdated.String
		candidate.Document.CreatedAt = candidate.Document.CreatedAt.UTC()
		candidate.Document.UpdatedAt = candidate.Document.UpdatedAt.UTC()
		if lastSucceeded.Valid {
			t := lastSucceeded.Time.UTC()
			candidate.LastSucceededAt = &t
		}
		candidate.Vector = decodeVector(vectorBytes)
		if len(candidate.Vector) != dim {
			return nil, fmt.Errorf("mysql: external candidate vector dimension mismatch for %s", docID)
		}
		out = append(out, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: iterate external candidates: %w", err)
	}
	return out, nil
}
