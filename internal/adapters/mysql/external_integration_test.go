//go:build integration

package mysql_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/domain/external"
	"claracore-server/internal/platform"
)

func TestExternalStoreBodyFreeMirrorAndSyncLease(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	clock := platform.FixedClock{T: time.Now().UTC().Truncate(time.Millisecond)}
	ids := platform.NewIDGenerator(clock)
	tenantID, spaceID := ids.New(), ids.New()
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM external_document_vectors WHERE tenant_id = ?`,
			`DELETE FROM external_documents WHERE tenant_id = ?`,
			`DELETE FROM external_sync_runs WHERE tenant_id = ?`,
			`DELETE FROM external_sources WHERE tenant_id = ?`,
			`DELETE FROM spaces WHERE tenant_id = ?`,
			`DELETE FROM tenants WHERE id = ?`,
		} {
			if _, err := db.ExecContext(context.Background(), stmt, string(tenantID)); err != nil {
				t.Errorf("cleanup external fixture: %v", err)
			}
		}
	})
	if _, err := db.ExecContext(ctx, `INSERT INTO tenants (id, name, status, created_at) VALUES (?, 'external-test', 'active', ?)`, string(tenantID), clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO spaces (id, tenant_id, type, name, owner_user_id, created_at) VALUES (?, ?, 'team', 'team', NULL, ?)`, string(spaceID), string(tenantID), clock.Now()); err != nil {
		t.Fatal(err)
	}

	store := mysql.NewExternalStore(db)
	source, err := store.EnsureSource(ctx, external.MirrorSource{
		ID: ids.New(), TenantID: tenantID, SpaceID: spaceID, Kind: "atlas_mcp", Name: "atlas-docs",
		System: "shared-docs", Status: external.SourceActive, CreatedAt: clock.Now(), UpdatedAt: clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	request1 := external.SyncRequest{ID: ids.New(), TenantID: tenantID, ExternalSourceID: source.ID, RequestedAt: clock.Now()}
	if accepted, err := store.QueueSyncRequest(ctx, request1); err != nil || !accepted {
		t.Fatalf("first durable manual request should be accepted: accepted=%v err=%v", accepted, err)
	}
	if accepted, err := store.QueueSyncRequest(ctx, external.SyncRequest{ID: ids.New(), TenantID: tenantID, ExternalSourceID: source.ID, RequestedAt: clock.Now()}); err != nil || accepted {
		t.Fatalf("pending manual requests must coalesce: accepted=%v err=%v", accepted, err)
	}
	claim1, claimed, err := store.ClaimSyncRequest(ctx, tenantID, source.Name, ids.New(), clock.Now().Add(10*time.Minute), clock.Now())
	if err != nil || !claimed || claim1.RequestID != request1.ID {
		t.Fatalf("first worker failed to claim durable request: claimed=%v claim=%+v err=%v", claimed, claim1, err)
	}
	if _, claimed, err := store.ClaimSyncRequest(ctx, tenantID, source.Name, ids.New(), clock.Now().Add(10*time.Minute), clock.Now()); err != nil || claimed {
		t.Fatalf("live request claim must exclude a second instance: claimed=%v err=%v", claimed, err)
	}
	request2 := external.SyncRequest{ID: ids.New(), TenantID: tenantID, ExternalSourceID: source.ID, RequestedAt: clock.Now()}
	if accepted, err := store.QueueSyncRequest(ctx, request2); err != nil || !accepted {
		t.Fatalf("one follow-up request should queue while the first is claimed: accepted=%v err=%v", accepted, err)
	}
	requestHealth, err := store.ListSourceHealth(ctx, tenantID)
	if err != nil || len(requestHealth) != 1 || !requestHealth[0].SyncClaimed || requestHealth[0].SyncClaimUntil == nil || requestHealth[0].SyncRequestedAt == nil {
		t.Fatalf("source health must expose both active claim and queued follow-up: health=%+v err=%v", requestHealth, err)
	}
	if err := store.ResolveSyncRequest(ctx, claim1, false, clock.Now()); err != nil {
		t.Fatal(err)
	}
	claim2, claimed, err := store.ClaimSyncRequest(ctx, tenantID, source.Name, ids.New(), clock.Now().Add(10*time.Minute), clock.Now())
	if err != nil || !claimed || claim2.RequestID != request2.ID {
		t.Fatalf("follow-up request was not preserved: claimed=%v claim=%+v err=%v", claimed, claim2, err)
	}
	if err := store.ResolveSyncRequest(ctx, claim2, true, clock.Now()); err != nil {
		t.Fatal(err)
	}
	reclaimed, claimed, err := store.ClaimSyncRequest(ctx, tenantID, source.Name, ids.New(), clock.Now().Add(10*time.Minute), clock.Now())
	if err != nil || !claimed || reclaimed.RequestID != request2.ID || reclaimed.ClaimToken == claim2.ClaimToken {
		t.Fatalf("released request must be reclaimable by another instance: claimed=%v claim=%+v err=%v", claimed, reclaimed, err)
	}
	if err := store.ResolveSyncRequest(ctx, reclaimed, false, clock.Now()); err != nil {
		t.Fatal(err)
	}
	run1 := external.SyncRun{
		ID: ids.New(), TenantID: tenantID, ExternalSourceID: source.ID, Status: external.SyncRunning,
		StartedAt: clock.Now(), LeaseUntil: clock.Now().Add(10 * time.Minute), CreatedAt: clock.Now(),
	}
	if err := store.BeginSync(ctx, run1); err != nil {
		t.Fatal(err)
	}
	run2 := run1
	run2.ID = ids.New()
	if err := store.BeginSync(ctx, run2); !errors.Is(err, external.ErrSyncBusy) {
		t.Fatalf("concurrent instance must not start a second destructive snapshot: %v", err)
	}

	doc := external.MirrorDocument{
		ID: ids.New(), TenantID: tenantID, SpaceID: spaceID, ExternalSourceID: source.ID,
		UpstreamSourceID: "capture-1", ExternalID: "doc-1", Kind: "yuque_document", Title: "Runbook",
		SourceRef: "https://docs.example/doc-1", Revision: "rev-1", State: external.DocumentActive,
		LastSeenRunID: run1.ID, CreatedAt: clock.Now(), UpdatedAt: clock.Now(),
	}
	vector := external.DocumentVector{
		ID: ids.New(), TenantID: tenantID, ExternalDocumentID: doc.ID, Model: "bge-m3", Revision: doc.Revision,
		InputHash: strings.Repeat("a", 64), Vector: []float32{0.25, -0.5}, CreatedAt: clock.Now(), UpdatedAt: clock.Now(),
	}
	if err := store.SaveIndexedDocument(ctx, doc, vector); err != nil {
		t.Fatal(err)
	}
	state, found, err := store.FindIndexState(ctx, source.ID, doc.ExternalID, vector.Model)
	if err != nil || !found || !state.Ready || state.DocumentID != doc.ID || state.Revision != doc.Revision {
		t.Fatalf("saved vector must be queryable by stable external identity: state=%+v found=%v err=%v", state, found, err)
	}
	missing, err := store.CompleteSync(ctx, source.ID, run1.ID, external.SyncReport{Seen: 1, Indexed: 1}, clock.Now())
	if err != nil || missing != 0 {
		t.Fatalf("first complete sync failed: missing=%d err=%v", missing, err)
	}
	health, err := store.ListSourceHealth(ctx, tenantID)
	if err != nil || len(health) != 1 || health[0].LastSyncStatus != external.SyncSucceeded || health[0].DocumentCount != 1 || health[0].SyncRunID != "" || health[0].SyncClaimed || health[0].SyncRequestedAt != nil {
		t.Fatalf("successful sync health receipt incomplete: health=%+v err=%v", health, err)
	}
	receipts, err := store.ListSyncRuns(ctx, tenantID, 10)
	if err != nil || len(receipts) != 1 || receipts[0].ID != run1.ID || receipts[0].SourceName != source.Name ||
		receipts[0].Status != external.SyncSucceeded || receipts[0].Seen != 1 || receipts[0].Indexed != 1 || receipts[0].FinishedAt == nil {
		t.Fatalf("successful sync receipt query incomplete: receipts=%+v err=%v", receipts, err)
	}
	if hidden, err := store.ListSyncRuns(ctx, ids.New(), 10); err != nil || len(hidden) != 0 {
		t.Fatalf("sync receipts must be tenant-scoped in SQL: hidden=%+v err=%v", hidden, err)
	}
	candidates, err := store.SearchCandidates(ctx, external.SearchQuery{
		TenantID: tenantID, SpaceIDs: []platform.ID{spaceID}, Query: "Runbook", Model: vector.Model, Limit: 10,
	})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("body-free search candidate missing: candidates=%+v err=%v", candidates, err)
	}
	if candidates[0].Document.ID != doc.ID || candidates[0].Document.SourceRef != doc.SourceRef || candidates[0].SourceName != source.Name || len(candidates[0].Vector) != 2 {
		t.Fatalf("candidate must carry source metadata and derived vector: %+v", candidates[0])
	}
	if hidden, err := store.SearchCandidates(ctx, external.SearchQuery{
		TenantID: tenantID, SpaceIDs: []platform.ID{ids.New()}, Query: "Runbook", Model: vector.Model, Limit: 10,
	}); err != nil || len(hidden) != 0 {
		t.Fatalf("unreadable space must be filtered in SQL: hidden=%+v err=%v", hidden, err)
	}

	// Metadata and vector are one transaction. Force the vector INSERT to fail;
	// the new document row must not survive on its own.
	badDoc := doc
	badDoc.ID, badDoc.ExternalID, badDoc.Title = ids.New(), "doc-bad", "bad"
	badVector := vector
	badVector.ID, badVector.ExternalDocumentID, badVector.Model = ids.New(), badDoc.ID, strings.Repeat("m", 65)
	if err := store.SaveIndexedDocument(ctx, badDoc, badVector); err == nil {
		t.Fatal("oversized vector model should force a transactional write failure")
	}
	var orphan int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_documents WHERE id = ?`, string(badDoc.ID)).Scan(&orphan); err != nil || orphan != 0 {
		t.Fatalf("metadata must roll back when vector write fails: count=%d err=%v", orphan, err)
	}

	// A later complete snapshot that sees nothing marks the prior mirror missing;
	// a failed snapshot must never do this cleanup.
	run3 := run1
	run3.ID, run3.StartedAt, run3.CreatedAt = ids.New(), clock.Now().Add(time.Second), clock.Now().Add(time.Second)
	run3.LeaseUntil = run3.StartedAt.Add(10 * time.Minute)
	if err := store.BeginSync(ctx, run3); err != nil {
		t.Fatal(err)
	}
	if err := store.FailSync(ctx, source.ID, run3.ID, "upstream unavailable", run3.StartedAt); err != nil {
		t.Fatal(err)
	}
	candidates, err = store.SearchCandidates(ctx, external.SearchQuery{
		TenantID: tenantID, SpaceIDs: []platform.ID{spaceID}, Query: "Runbook", Model: vector.Model, Limit: 10,
	})
	if err != nil || len(candidates) != 1 || candidates[0].LastSyncStatus != external.SyncFailed || candidates[0].LastSucceededAt == nil {
		t.Fatalf("failed refresh must retain searchable mirror with honest freshness: candidates=%+v err=%v", candidates, err)
	}
	var active int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_documents WHERE id = ? AND state = 'active'`, string(doc.ID)).Scan(&active); err != nil || active != 1 {
		t.Fatalf("failed snapshot must retain last usable mirror: active=%d err=%v", active, err)
	}
	run4 := run3
	run4.ID, run4.StartedAt, run4.CreatedAt = ids.New(), clock.Now().Add(2*time.Second), clock.Now().Add(2*time.Second)
	run4.LeaseUntil = run4.StartedAt.Add(10 * time.Minute)
	if err := store.BeginSync(ctx, run4); err != nil {
		t.Fatal(err)
	}
	missing, err = store.CompleteSync(ctx, source.ID, run4.ID, external.SyncReport{}, run4.StartedAt)
	if err != nil || missing != 1 {
		t.Fatalf("complete empty snapshot must mark one missing document: missing=%d err=%v", missing, err)
	}
}
