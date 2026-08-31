//go:build integration

package mysql_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cryptoadapter "claracore-server/internal/adapters/crypto"
	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/config"
	"claracore-server/internal/platform"
	"claracore-server/internal/portableexport"
	"claracore-server/internal/ports"
	"claracore-server/internal/teamimport"
)

func TestTenantExporterProducesIsolatedVerifiablePortableArchive(t *testing.T) {
	db := openTestDB(t)
	cleanupStaleExportFixtures(t, db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	tenant, foreign := ids.New(), ids.New()
	user, member, foreignUser, agent, space := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	teamSpace, personalSpace, memberPersonalSpace := ids.New(), ids.New(), ids.New()
	memory, foreignMemory, card, trigger, link := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	line, position, history, handoff := ids.New(), ids.New(), ids.New(), ids.New()
	cleanupExportTenants(t, db, tenant, foreign)

	mustExecExport(t, db, `INSERT INTO tenants (id,name,status,created_at) VALUES (?,?,?,?),(?,?,?,?)`, tenant, "Team A", "active", now, foreign, "Foreign", "active", now)
	mustExecExport(t, db, `INSERT INTO users (id,tenant_id,username,password_hash,display_name,role,must_change_pw,status,created_at) VALUES (?,?,?,?,?,?,?,?,?),(?,?,?,?,?,?,?,?,?),(?,?,?,?,?,?,?,?,?)`,
		user, tenant, "alice", "ccs-export-integration-secret-password", "Alice", "admin", 0, "active", now,
		member, tenant, "bob", "ccs-export-integration-member-password", "Bob", "member", 0, "active", now,
		foreignUser, foreign, "mallory", "ccs-export-integration-foreign-password", "Mallory", "admin", 0, "active", now)
	mustExecExport(t, db, `INSERT INTO agents (id,tenant_id,user_id,slug,display_name,created_at) VALUES (?,?,?,?,?,?)`, agent, tenant, user, "codex", "Codex", now)
	mustExecExport(t, db, `INSERT INTO tokens (id,tenant_id,user_id,agent_id,name,token_prefix,token_hash,created_at) VALUES (?,?,?,?,?,?,?,?)`, ids.New(), tenant, user, agent, "laptop", "ccs_test", strings.Repeat("a", 64), now)
	mustExecExport(t, db, `INSERT INTO spaces (id,tenant_id,type,name,owner_user_id,created_at) VALUES (?,?,?,?,?,?),(?,?,?,?,?,?),(?,?,?,?,?,?),(?,?,?,?,?,?)`,
		space, tenant, "project", "Portable", user, now,
		teamSpace, tenant, "team", "Team A", nil, now,
		personalSpace, tenant, "personal", "Alice", user, now,
		memberPersonalSpace, tenant, "personal", "Bob", member, now)
	mustExecExport(t, db, `INSERT INTO space_permissions (space_id,user_id,can_read,can_write,granted_by,created_at) VALUES (?,?,?,?,?,?)`, space, user, 1, 1, user, now)
	mustExecExport(t, db, `INSERT INTO memories (id,tenant_id,space_id,author_user_id,author_agent_id,title,body,status,version,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?),(?,?,?,?,?,?,?,?,?,?,?)`,
		memory, tenant, space, user, agent, "Portable memory", "tenant-body-完整", "active", 1, now, now,
		foreignMemory, foreign, space, foreignUser, nil, "Foreign", "foreign-body", "active", 1, now, now)
	mustExecExport(t, db, `INSERT INTO cards (id,tenant_id,space_id,author_user_id,author_agent_id,title,card_type,status,source,body_distilled,body_understanding,version,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, card, tenant, space, user, agent, "Portable card", "practice", "active", "source", "distilled", "understanding", 1, now, now)
	mustExecExport(t, db, `INSERT INTO card_recall_triggers (id,tenant_id,card_id,space_id,trigger_text,sort_order,created_at) VALUES (?,?,?,?,?,?,?)`, trigger, tenant, card, space, "when exporting", 0, now)
	mustExecExport(t, db, `INSERT INTO labels (tenant_id,entity_type,entity_id,label,created_at) VALUES (?,?,?,?,?)`, tenant, "card", card, "portable", now)
	mustExecExport(t, db, `INSERT INTO links (id,tenant_id,from_type,from_id,to_type,to_id,kind,note,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, link, tenant, "card", card, "memory", memory, "related", "kept", now)
	mustExecExport(t, db, `INSERT INTO continuity_lines (id,tenant_id,owner_user_id,owner_agent_id,title,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`, line, tenant, user, agent, "Ship export", "active", now, now)
	mustExecExport(t, db, `INSERT INTO continuity_positions (id,tenant_id,line_id,summary,interpretation_status,facts_used,next_step,writer_user_id,writer_agent_id,version,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, position, tenant, line, "Current", "active", `["fact"]`, "Verify", user, agent, 1, now, now)
	mustExecExport(t, db, `INSERT INTO continuity_position_history (id,tenant_id,line_id,position_id,position_version,summary,interpretation_status,facts_used,next_step,writer_user_id,writer_agent_id,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, history, tenant, line, position, 1, "Current", "active", `["fact"]`, "Verify", user, agent, now)
	mustExecExport(t, db, `INSERT INTO continuity_handoffs (id,tenant_id,line_id,writer_user_id,writer_agent_id,objective,completed,open_items,next_step,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, handoff, tenant, line, user, agent, "Ship", `["code"]`, `["target"]`, "Verify", now)
	mustExecExport(t, db, `INSERT INTO audit_logs (id,tenant_id,user_id,source,kind,access,outcome,action,metadata,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, ids.New(), tenant, user, "web", "governance", "read", "success", "secret-audit", `{"secret":"secret-audit"}`, now)

	var archive bytes.Buffer
	result, err := mysql.NewTenantExporter(db, "0010").Export(ctx, ports.TenantExportInput{
		TenantID: tenant, GeneratedAt: now, MaxBytes: 16 << 20,
	}, &archive)
	if err != nil {
		t.Fatal(err)
	}
	if result.FormatVersion != 1 || result.RecordCount != 17 || result.BytesWritten != int64(archive.Len()) {
		t.Fatalf("unexpected export receipt: %+v archive=%d", result, archive.Len())
	}
	verification, err := portableexport.VerifyReaderAt(bytes.NewReader(archive.Bytes()), int64(archive.Len()), portableexport.DefaultLimits())
	if err != nil {
		t.Fatalf("real MySQL export must pass the shipped offline verifier: %v", err)
	}
	if !verification.OK || verification.TenantID != string(tenant) || verification.SchemaVersion != "0010" || verification.Records != result.RecordCount {
		t.Fatalf("unexpected offline verification receipt: %+v", verification)
	}
	archivePath := filepath.Join(t.TempDir(), "real-mysql-team-export.zip")
	if err := os.WriteFile(archivePath, archive.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	verifiedArchive, err := portableexport.OpenFile(archivePath, portableexport.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer verifiedArchive.Close()
	plan, err := teamimport.Analyze(verifiedArchive, "alice")
	if err != nil {
		t.Fatalf("real MySQL export must pass import semantic preflight: %v", err)
	}
	if !plan.OK || plan.SelectedAdmin != "alice" || plan.Records != result.RecordCount || !plan.RequiresEmptyDeployment || plan.CredentialsImported {
		t.Fatalf("unexpected real import plan: %+v", plan)
	}
	targetDB := openTemporaryImportDatabase(t)
	hasher := cryptoadapter.NewArgon2id(cryptoadapter.DefaultParams())
	importer := mysql.NewTenantImporter(targetDB, "0010", hasher, platform.FixedClock{T: now.Add(time.Hour)})
	importResult, err := importer.Import(ctx, verifiedArchive, mysql.TenantImportInput{AdminUsername: "alice", AdminPassword: "new-import-password", EmbeddingModel: "bge-m3"})
	if err != nil {
		t.Fatalf("import verified archive into empty deployment: %v", err)
	}
	if !importResult.OK || importResult.TenantID != string(tenant) || importResult.AdminUsername != "alice" || importResult.Records != result.RecordCount || importResult.MembersDisabled != 1 || importResult.SourceCredentialsImported || !importResult.AdminMustChangePassword || !importResult.IndexingRequired || importResult.IndexingStatus != "queued" || importResult.IndexingQueued != 3 || importResult.IndexModel != "bge-m3" {
		t.Fatalf("unexpected import result: %+v", importResult)
	}
	assertImportedTeam(t, targetDB, hasher, tenant, user, member, memory, card, trigger, line, "bge-m3")
	checkResult, err := mysql.NewTenantImportChecker(targetDB, "0010").Check(ctx, mysql.TenantImportCheckInput{AdminUsername: "alice", EmbeddingModel: "bge-m3"})
	if err != nil {
		t.Fatalf("reconstruct pristine import receipt after stdout loss: %v", err)
	}
	if !checkResult.OK || checkResult.Status != "verified" || checkResult.TenantID != string(tenant) || checkResult.AdminUsername != "alice" || checkResult.Records != result.RecordCount || checkResult.MembersDisabled != 1 || checkResult.SourceCredentialsImported || checkResult.ExcludedRuntimeRows != 0 || checkResult.ImportAuditID == "" || checkResult.IndexingStatus != "queued" || checkResult.IndexingQueued != 3 || checkResult.IndexingPending != 3 || checkResult.IndexingReady != 0 || checkResult.IndexingFailed != 0 || len(checkResult.Datasets) != len(portableexport.DataFiles()) {
		t.Fatalf("unexpected reconstructed import receipt: %+v", checkResult)
	}
	if _, err := importer.Import(ctx, verifiedArchive, mysql.TenantImportInput{AdminUsername: "alice", AdminPassword: "another-password"}); !errors.Is(err, mysql.ErrTenantImportDeploymentNotEmpty) {
		t.Fatalf("second import must reject non-empty deployment: %v", err)
	}
	if _, err := targetDB.ExecContext(ctx, `UPDATE users SET status='active' WHERE id=?`, member); err != nil {
		t.Fatal(err)
	}
	if _, err := mysql.NewTenantImportChecker(targetDB, "0010").Check(ctx, mysql.TenantImportCheckInput{AdminUsername: "alice", EmbeddingModel: "bge-m3"}); !errors.Is(err, mysql.ErrTenantImportReceiptNotProven) {
		t.Fatalf("post-import member activation must end the pristine recovery window: %v", err)
	}
	disabledDB := openTemporaryImportDatabase(t)
	disabledImporter := mysql.NewTenantImporter(disabledDB, "0010", hasher, platform.FixedClock{T: now.Add(90 * time.Minute)})
	disabledResult, err := disabledImporter.Import(ctx, verifiedArchive, mysql.TenantImportInput{AdminUsername: "alice", AdminPassword: "disabled-model-password"})
	if err != nil {
		t.Fatalf("disabled embedding import must retain the full-text deployment path: %v", err)
	}
	if !disabledResult.IndexingRequired || disabledResult.IndexingStatus != "backfill_required" || disabledResult.IndexingQueued != 0 || disabledResult.IndexModel != "" {
		t.Fatalf("disabled embedding receipt must require a later backfill: %+v", disabledResult)
	}
	var disabledEmbeddingCount int
	if err := disabledDB.QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&disabledEmbeddingCount); err != nil || disabledEmbeddingCount != 0 {
		t.Fatalf("disabled embedding import queued %d rows: %v", disabledEmbeddingCount, err)
	}
	disabledCheck, err := mysql.NewTenantImportChecker(disabledDB, "0010").Check(ctx, mysql.TenantImportCheckInput{AdminUsername: "alice"})
	if err != nil || !disabledCheck.OK || disabledCheck.IndexingStatus != "backfill_required" || disabledCheck.IndexingQueued != 0 || disabledCheck.IndexingPending != 0 {
		t.Fatalf("disabled embedding pristine receipt was not reconstructed: result=%+v err=%v", disabledCheck, err)
	}
	rollbackDB := openTemporaryImportDatabase(t)
	if _, err := rollbackDB.ExecContext(ctx, `ALTER TABLE continuity_handoffs DROP COLUMN next_step`); err != nil {
		t.Fatal(err)
	}
	rollbackImporter := mysql.NewTenantImporter(rollbackDB, "0010", hasher, platform.FixedClock{T: now.Add(2 * time.Hour)})
	if _, err := rollbackImporter.Import(ctx, verifiedArchive, mysql.TenantImportInput{AdminUsername: "alice", AdminPassword: "rollback-password", EmbeddingModel: "bge-m3"}); err == nil {
		t.Fatal("late import storage failure unexpectedly succeeded")
	}
	for _, table := range []string{"tenants", "users", "memories", "cards", "embeddings", "continuity_lines", "audit_logs"} {
		var count int
		if err := rollbackDB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed import left %d rows in %s: %v", count, table, err)
		}
	}
	if _, err := mysql.NewTenantImportChecker(rollbackDB, "0010").Check(ctx, mysql.TenantImportCheckInput{AdminUsername: "alice", EmbeddingModel: "bge-m3"}); !errors.Is(err, mysql.ErrTenantImportReceiptNotProven) {
		t.Fatalf("rolled-back import must not produce a recovery receipt: %v", err)
	}

	files := readExportZIP(t, archive.Bytes())
	manifestRaw, ok := files["manifest.json"]
	if !ok {
		t.Fatal("portable export lacks final manifest")
	}
	var manifest struct {
		Format        string `json:"format"`
		FormatVersion int    `json:"format_version"`
		SchemaVersion string `json:"schema_version"`
		TenantID      string `json:"tenant_id"`
		Complete      bool   `json:"complete"`
		Files         []struct {
			Name    string `json:"name"`
			Records int64  `json:"records"`
			SHA256  string `json:"sha256"`
		} `json:"files"`
		Excluded []string `json:"excluded"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Format != "claracore-team-export" || manifest.FormatVersion != 1 || manifest.SchemaVersion != "0010" || manifest.TenantID != string(tenant) || !manifest.Complete || len(manifest.Files) != 14 {
		t.Fatalf("invalid manifest: %+v", manifest)
	}
	for _, receipt := range manifest.Files {
		body, ok := files[receipt.Name]
		if !ok {
			t.Fatalf("manifest references missing file %q", receipt.Name)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != receipt.SHA256 {
			t.Fatalf("manifest hash mismatch for %s", receipt.Name)
		}
	}
	whole := string(bytes.Join(mapValues(files), nil))
	for _, forbidden := range []string{"ccs-export-integration-secret-password", "ccs-export-integration-member-password", strings.Repeat("a", 64), "foreign-body", "secret-audit"} {
		if strings.Contains(whole, forbidden) {
			t.Fatalf("export leaked excluded or foreign value %q", forbidden)
		}
	}
	if !strings.Contains(string(files["memories.jsonl"]), "tenant-body-完整") || !strings.Contains(string(files["cards.jsonl"]), "Portable card") || !strings.Contains(string(files["shared_line_handoffs.jsonl"]), "Ship") {
		t.Fatal("portable export omitted core Memory, Card, or Shared Line content")
	}
	if !strings.Contains(strings.Join(manifest.Excluded, ","), "password_hashes") || !strings.Contains(strings.Join(manifest.Excluded, ","), "audit_logs") {
		t.Fatalf("manifest does not disclose exclusions: %v", manifest.Excluded)
	}

	var limited bytes.Buffer
	_, err = mysql.NewTenantExporter(db, "0010").Export(ctx, ports.TenantExportInput{TenantID: tenant, GeneratedAt: now, MaxBytes: 1}, &limited)
	if !errors.Is(err, ports.ErrTenantExportTooLarge) {
		t.Fatalf("size boundary must fail with typed error: %v", err)
	}
}

func openTemporaryImportDatabase(t *testing.T) *sql.DB {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	target, err := mysql.Open(context.Background(), cfg.MySQL.DSN(), mysql.PoolConfig{MaxOpenConns: 1, MaxIdleConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Close() })
	for _, table := range []string{
		"tenants", "users", "agents", "tokens", "spaces", "space_permissions",
		"memories", "cards", "card_recall_triggers", "labels", "links", "embeddings", "sessions", "audit_logs",
		"continuity_lines", "continuity_positions", "continuity_position_history", "continuity_handoffs",
		"external_sources", "external_sync_runs", "external_documents", "external_document_vectors",
	} {
		source := "`" + strings.ReplaceAll(cfg.MySQL.Database, "`", "``") + "`.`" + table + "`"
		temporaryName := "ccs_tmp_" + table
		create := `CREATE TEMPORARY TABLE ` + "`" + temporaryName + "`" + ` LIKE ` + source
		if table == "memories" || table == "cards" || table == "card_recall_triggers" {
			create = `CREATE TEMPORARY TABLE ` + "`" + temporaryName + "`" + ` AS SELECT * FROM ` + source + ` AS source_rows WHERE 1=0`
		}
		if _, err := target.ExecContext(context.Background(), create); err != nil {
			t.Fatalf("create isolated temporary import table %s: %v", table, err)
		}
		if _, err := target.ExecContext(context.Background(), `ALTER TABLE `+"`"+temporaryName+"`"+` RENAME TO `+"`"+table+"`"); err != nil {
			t.Fatalf("rename isolated temporary import table %s: %v", table, err)
		}
	}
	return target
}

func assertImportedTeam(t *testing.T, db *sql.DB, hasher *cryptoadapter.Argon2id, tenant, admin, member, memory, card, trigger, line platform.ID, model string) {
	t.Helper()
	var tenantCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tenants WHERE id=?`, tenant).Scan(&tenantCount); err != nil || tenantCount != 1 {
		t.Fatalf("imported tenant count=%d err=%v", tenantCount, err)
	}
	var adminHash, adminStatus, adminRole string
	var adminMustChange bool
	if err := db.QueryRow(`SELECT password_hash,status,role,must_change_pw FROM users WHERE id=?`, admin).Scan(&adminHash, &adminStatus, &adminRole, &adminMustChange); err != nil {
		t.Fatal(err)
	}
	if matches, err := hasher.Verify("new-import-password", adminHash); err != nil || !matches || adminStatus != "active" || adminRole != "admin" || !adminMustChange {
		t.Fatalf("administrator takeover failed: match=%v status=%s role=%s mustChange=%v err=%v", matches, adminStatus, adminRole, adminMustChange, err)
	}
	var memberHash, memberStatus string
	var memberMustChange bool
	if err := db.QueryRow(`SELECT password_hash,status,must_change_pw FROM users WHERE id=?`, member).Scan(&memberHash, &memberStatus, &memberMustChange); err != nil {
		t.Fatal(err)
	}
	if matches, err := hasher.Verify("ccs-export-integration-member-password", memberHash); err != nil || matches || memberStatus != "disabled" || !memberMustChange {
		t.Fatalf("non-takeover member credential boundary failed: sourceMatches=%v status=%s mustChange=%v err=%v", matches, memberStatus, memberMustChange, err)
	}
	for table, id := range map[string]platform.ID{"memories": memory, "cards": card, "continuity_lines": line} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE id=? AND tenant_id=?`, id, tenant).Scan(&count); err != nil || count != 1 {
			t.Fatalf("imported %s count=%d err=%v", table, count, err)
		}
	}
	for _, table := range []string{"tokens", "sessions", "external_sources", "external_sync_runs", "external_documents", "external_document_vectors"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("excluded table %s count=%d err=%v", table, count, err)
		}
	}
	wantEmbeddingText := map[string]string{
		"memory:" + string(memory):        "Portable memory\n\ntenant-body-完整",
		"card:" + string(card):            "Portable card\n\ndistilled\n\nunderstanding",
		"card_trigger:" + string(trigger): "when exporting",
	}
	rows, err := db.Query(`SELECT entity_type,entity_id,model,source_text,content_hash,status,attempts,vector FROM embeddings ORDER BY entity_type,entity_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var entityType, entityID, gotModel, sourceText, contentHash, status string
		var attempts int
		var vector []byte
		if err := rows.Scan(&entityType, &entityID, &gotModel, &sourceText, &contentHash, &status, &attempts, &vector); err != nil {
			t.Fatal(err)
		}
		wantText, ok := wantEmbeddingText[entityType+":"+entityID]
		sum := sha256.Sum256([]byte(sourceText))
		if !ok || sourceText != wantText || gotModel != model || contentHash != hex.EncodeToString(sum[:]) || status != "pending" || attempts != 0 || len(vector) != 0 {
			t.Fatalf("unexpected imported embedding: type=%s id=%s model=%s text=%q hash=%s status=%s attempts=%d vector=%d", entityType, entityID, gotModel, sourceText, contentHash, status, attempts, len(vector))
		}
		seen++
	}
	if err := rows.Err(); err != nil || seen != len(wantEmbeddingText) {
		t.Fatalf("imported embedding rows=%d want=%d err=%v", seen, len(wantEmbeddingText), err)
	}
	var auditCount int
	var auditSource, auditAction string
	if err := db.QueryRow(`SELECT COUNT(*),MIN(source),MIN(action) FROM audit_logs`).Scan(&auditCount, &auditSource, &auditAction); err != nil || auditCount != 1 || auditSource != "cli" || auditAction != "admin.import.apply" {
		t.Fatalf("import audit count=%d source=%s action=%s err=%v", auditCount, auditSource, auditAction, err)
	}
}

func mustExecExport(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func cleanupExportTenants(t *testing.T, db *sql.DB, tenants ...platform.ID) {
	t.Helper()
	t.Cleanup(func() {
		for _, tenant := range tenants {
			deleteExportTenant(t, db, tenant)
		}
	})
}

func cleanupStaleExportFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT DISTINCT tenant_id FROM users WHERE password_hash IN ('secret-password-hash','foreign-secret','ccs-export-integration-secret-password','ccs-export-integration-foreign-password')`)
	if err != nil {
		t.Fatal(err)
	}
	var tenants []platform.ID
	for rows.Next() {
		var tenant platform.ID
		if err := rows.Scan(&tenant); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tenants = append(tenants, tenant)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range tenants {
		deleteExportTenant(t, db, tenant)
	}
}

func deleteExportTenant(t *testing.T, db *sql.DB, tenant platform.ID) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE p FROM space_permissions p JOIN spaces s ON s.id=p.space_id WHERE s.tenant_id=?`, tenant); err != nil {
		t.Errorf("cleanup space_permissions: %v", err)
	}
	for _, table := range []string{"external_document_vectors", "external_documents", "external_sync_runs", "external_sources", "continuity_handoffs", "continuity_position_history", "continuity_positions", "continuity_lines", "audit_logs", "sessions", "embeddings", "card_recall_triggers", "labels", "links", "cards", "memories", "tokens", "agents", "spaces", "users"} {
		if _, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE tenant_id = ?`, tenant); err != nil {
			t.Errorf("cleanup %s: %v", table, err)
		}
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM tenants WHERE id = ?`, tenant); err != nil {
		t.Errorf("cleanup tenants: %v", err)
	}
}

func readExportZIP(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte, len(zr.File))
	for _, file := range zr.File {
		r, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = body
	}
	return files
}

func mapValues(input map[string][]byte) [][]byte {
	out := make([][]byte, 0, len(input))
	for _, value := range input {
		out = append(out, value)
	}
	return out
}
