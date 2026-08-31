//go:build integration

package mysql_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/platform"
)

func TestTenantCapacityIndexesAndConfiguredDatabaseRead(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	wantIndexes := map[string]string{
		"memories": "idx_memories_tenant_capacity", "cards": "idx_cards_tenant_capacity",
		"card_recall_triggers": "idx_triggers_tenant_capacity", "links": "idx_links_tenant_capacity",
		"embeddings": "idx_embeddings_tenant_capacity",
	}
	for table, index := range wantIndexes {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=? AND seq_in_index=1 AND column_name='tenant_id'`, table, index).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("tenant capacity index missing or malformed: table=%s index=%s count=%d", table, index, count)
		}
	}
	var tenant string
	if err := db.QueryRowContext(ctx, `SELECT id FROM tenants ORDER BY id LIMIT 1`).Scan(&tenant); err != nil {
		if err == sql.ErrNoRows {
			t.Skip("configured database has no tenant")
		}
		t.Fatal(err)
	}
	snapshot, err := mysql.NewTenantCapacityReader(db).ReadTenantCapacity(ctx, platform.ID(tenant))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MeasuredAt.IsZero() || snapshot.Members < 0 || snapshot.TotalLogicalBytes() < 0 {
		t.Fatalf("invalid configured database capacity receipt: measured=%v members=%d bytes=%d", snapshot.MeasuredAt, snapshot.Members, snapshot.TotalLogicalBytes())
	}
}

func TestTenantCapacityReaderReturnsExactIsolatedLogicalSnapshot(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(wd, "..", "..", ".."))
	db := openTemporaryImportDatabase(t)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	tenant, foreign := ids.New(), ids.New()
	user, agent, space := ids.New(), ids.New(), ids.New()
	memory, card, trigger, link := ids.New(), ids.New(), ids.New(), ids.New()
	line, position, history, handoff := ids.New(), ids.New(), ids.New(), ids.New()
	externalSource, syncRun, externalDocument, externalVector := ids.New(), ids.New(), ids.New(), ids.New()

	mustExecExport(t, db, `INSERT INTO tenants (id,name,status,created_at) VALUES (?,?,?,?),(?,?,?,?)`, tenant, "Capacity", "active", now, foreign, "Foreign", "active", now)
	mustExecExport(t, db, `INSERT INTO users (id,tenant_id,username,password_hash,display_name,role,must_change_pw,status,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, user, tenant, "admin", "test-only", "Admin", "admin", 0, "active", now)
	mustExecExport(t, db, `INSERT INTO agents (id,tenant_id,user_id,slug,display_name,created_at) VALUES (?,?,?,?,?,?)`, agent, tenant, user, "codex", "Codex", now)
	mustExecExport(t, db, `INSERT INTO spaces (id,tenant_id,type,name,owner_user_id,created_at) VALUES (?,?,?,?,?,?)`, space, tenant, "team", "Team", nil, now)

	memoryTitle, memoryBody := "记忆", "payload"
	cardTitle, cardType, cardSource, cardDistilled := "Card", "practice", "source", "distilled"
	triggerText, labelText, linkNote := "when needed", "capacity", "related note"
	mustExecExport(t, db, `INSERT INTO memories (id,tenant_id,space_id,author_user_id,author_agent_id,title,body,status,version,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, memory, tenant, space, user, agent, memoryTitle, memoryBody, "active", 1, now, now)
	mustExecExport(t, db, `INSERT INTO cards (id,tenant_id,space_id,author_user_id,author_agent_id,title,card_type,status,source,body_distilled,version,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, card, tenant, space, user, agent, cardTitle, cardType, "active", cardSource, cardDistilled, 1, now, now)
	mustExecExport(t, db, `INSERT INTO card_recall_triggers (id,tenant_id,card_id,space_id,trigger_text,sort_order,created_at) VALUES (?,?,?,?,?,?,?)`, trigger, tenant, card, space, triggerText, 0, now)
	mustExecExport(t, db, `INSERT INTO labels (tenant_id,entity_type,entity_id,label,created_at) VALUES (?,?,?,?,?)`, tenant, "card", card, labelText, now)
	mustExecExport(t, db, `INSERT INTO links (id,tenant_id,from_type,from_id,to_type,to_id,kind,note,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, link, tenant, "card", card, "memory", memory, "related", linkNote, now)

	mustExecExport(t, db, `INSERT INTO continuity_lines (id,tenant_id,owner_user_id,owner_agent_id,title,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`, line, tenant, user, agent, "Capacity line", "active", now, now)
	mustExecExport(t, db, `INSERT INTO continuity_positions (id,tenant_id,line_id,summary,interpretation_status,facts_used,next_step,writer_user_id,writer_agent_id,version,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, position, tenant, line, "Current", "active", `["fact"]`, "Next", user, agent, 1, now, now)
	mustExecExport(t, db, `INSERT INTO continuity_position_history (id,tenant_id,line_id,position_id,position_version,summary,interpretation_status,facts_used,next_step,writer_user_id,writer_agent_id,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, history, tenant, line, position, 1, "Previous", "active", `["fact"]`, "Next", user, agent, now)
	mustExecExport(t, db, `INSERT INTO continuity_handoffs (id,tenant_id,line_id,writer_user_id,writer_agent_id,objective,completed,open_items,next_step,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, handoff, tenant, line, user, agent, "Ship", `["done"]`, `["open"]`, "Verify", now)

	sourceText, vectorBytes := "embedding input", []byte{1, 2, 3, 4}
	mustExecExport(t, db, `INSERT INTO embeddings (id,tenant_id,entity_type,entity_id,model,content_hash,source_text,vector,dim,status,attempts,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, ids.New(), tenant, "memory", memory, "test", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", sourceText, vectorBytes, 1, "ready", 0, now, now)
	mustExecExport(t, db, `INSERT INTO external_sources (id,tenant_id,space_id,kind,name,source_system,status,last_sync_status,document_count,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, externalSource, tenant, space, "atlas", "Docs", "atlas", "active", "succeeded", 1, now, now)
	mustExecExport(t, db, `INSERT INTO external_sync_runs (id,tenant_id,external_source_id,status,seen_count,indexed_count,unchanged_count,missing_count,started_at,finished_at,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, syncRun, tenant, externalSource, "succeeded", 1, 1, 0, 0, now, now, now)
	mustExecExport(t, db, `INSERT INTO external_documents (id,tenant_id,space_id,external_source_id,upstream_source_id,external_id,kind,title,source_ref,revision,state,last_seen_run_id,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, externalDocument, tenant, space, externalSource, "atlas", "doc-1", "document", "External", "atlas://doc-1", "v1", "active", syncRun, now, now)
	mustExecExport(t, db, `INSERT INTO external_document_vectors (id,tenant_id,external_document_id,model,revision,input_hash,vector,dim,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, externalVector, tenant, externalDocument, "test", "v1", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", []byte{5, 6, 7, 8}, 1, now, now)
	mustExecExport(t, db, `INSERT INTO audit_logs (id,tenant_id,user_id,source,kind,access,outcome,action,metadata,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, ids.New(), tenant, user, "web", "governance", "read", "success", "capacity.read", `{"page":"system"}`, now)

	// Foreign rows use large payloads so any tenant-scope regression is obvious.
	mustExecExport(t, db, `INSERT INTO memories (id,tenant_id,space_id,author_user_id,title,body,status,version,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, ids.New(), foreign, ids.New(), ids.New(), "foreign", "foreign payload must not count", "active", 1, now, now)
	mustExecExport(t, db, `INSERT INTO audit_logs (id,tenant_id,user_id,source,kind,access,outcome,action,metadata,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, ids.New(), foreign, ids.New(), "web", "trace", "read", "success", "foreign", `{"large":"foreign payload"}`, now)

	snapshot, err := mysql.NewTenantCapacityReader(db).ReadTenantCapacity(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.MeasuredAt.IsZero() || snapshot.Members != 1 || snapshot.Agents != 1 || snapshot.Spaces != 1 {
		t.Fatalf("unexpected governance snapshot: %+v", snapshot)
	}
	if snapshot.Memories != 1 || snapshot.Cards != 1 || snapshot.RecallTriggers != 1 || snapshot.Labels != 1 || snapshot.Links != 1 || snapshot.KnowledgeRows() != 5 {
		t.Fatalf("unexpected knowledge counts: %+v", snapshot)
	}
	wantKnowledgeBytes := byteLen(memoryTitle, memoryBody, cardTitle, cardType, cardSource, cardDistilled, triggerText, labelText, linkNote)
	if snapshot.KnowledgeLogicalBytes != wantKnowledgeBytes {
		t.Fatalf("knowledge bytes=%d want=%d", snapshot.KnowledgeLogicalBytes, wantKnowledgeBytes)
	}
	if snapshot.ContinuityLines != 1 || snapshot.ContinuityPositions != 1 || snapshot.ContinuityHistory != 1 || snapshot.ContinuityHandoffs != 1 || snapshot.ContinuityRows() != 4 || snapshot.ContinuityLogicalBytes <= 0 {
		t.Fatalf("unexpected continuity capacity: %+v", snapshot)
	}
	if snapshot.Embeddings != 1 || snapshot.EmbeddingLogicalBytes != int64(len(sourceText)+len(vectorBytes)) {
		t.Fatalf("unexpected embedding capacity: %+v", snapshot)
	}
	if snapshot.ExternalSources != 1 || snapshot.ExternalSyncRuns != 1 || snapshot.ExternalDocuments != 1 || snapshot.ExternalVectors != 1 || snapshot.ExternalRows() != 4 || snapshot.ExternalLogicalBytes <= 0 {
		t.Fatalf("unexpected external capacity: %+v", snapshot)
	}
	if snapshot.AuditEvents != 1 || snapshot.AuditLogicalBytes <= 0 || snapshot.TotalLogicalBytes() <= snapshot.KnowledgeLogicalBytes {
		t.Fatalf("unexpected audit or total capacity: %+v", snapshot)
	}
}

func byteLen(values ...string) int64 {
	var total int64
	for _, value := range values {
		total += int64(len([]byte(value)))
	}
	return total
}
