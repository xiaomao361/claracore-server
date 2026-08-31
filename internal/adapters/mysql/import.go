package mysql

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
	"claracore-server/internal/portableexport"
	"claracore-server/internal/ports"
	"claracore-server/internal/teamimport"
)

var ErrTenantImportDeploymentNotEmpty = errors.New("mysql: team import requires an empty deployment")

type TenantImporter struct {
	db            *sql.DB
	schemaVersion string
	hasher        ports.PasswordHasher
	clock         platform.Clock
}

type TenantImportInput struct {
	AdminUsername  string
	AdminPassword  string
	EmbeddingModel string
}

type TenantImportResult struct {
	OK                        bool      `json:"ok"`
	Status                    string    `json:"status"`
	TenantID                  string    `json:"tenantId"`
	AdminUsername             string    `json:"adminUsername"`
	Records                   int64     `json:"records"`
	MembersDisabled           int64     `json:"membersDisabled"`
	SourceCredentialsImported bool      `json:"sourceCredentialsImported"`
	AdminMustChangePassword   bool      `json:"adminMustChangePassword"`
	IndexingRequired          bool      `json:"indexingRequired"`
	IndexingStatus            string    `json:"indexingStatus"`
	IndexingQueued            int64     `json:"indexingQueued"`
	IndexModel                string    `json:"indexModel"`
	ImportedAt                time.Time `json:"importedAt"`
}

func NewTenantImporter(db *sql.DB, schemaVersion string, hasher ports.PasswordHasher, clock platform.Clock) *TenantImporter {
	return &TenantImporter{db: db, schemaVersion: schemaVersion, hasher: hasher, clock: clock}
}

func (i *TenantImporter) Import(ctx context.Context, archive *portableexport.Archive, in TenantImportInput) (TenantImportResult, error) {
	if i == nil || i.db == nil || i.hasher == nil || i.clock == nil || archive == nil {
		return TenantImportResult{}, errors.New("mysql: invalid team importer dependencies")
	}
	plan, err := teamimport.Analyze(archive, in.AdminUsername)
	if err != nil {
		return TenantImportResult{}, err
	}
	if plan.SchemaVersion != i.schemaVersion {
		return TenantImportResult{}, fmt.Errorf("mysql: export schema %s does not match current schema %s", plan.SchemaVersion, i.schemaVersion)
	}
	if len([]rune(in.AdminPassword)) < governance.MinPasswordLen {
		return TenantImportResult{}, fmt.Errorf("mysql: import administrator password must be at least %d characters", governance.MinPasswordLen)
	}
	if in.EmbeddingModel != strings.TrimSpace(in.EmbeddingModel) || len([]rune(in.EmbeddingModel)) > 64 {
		return TenantImportResult{}, errors.New("mysql: import embedding model must be at most 64 characters without surrounding whitespace")
	}
	adminHash, err := i.hasher.Hash(in.AdminPassword)
	if err != nil {
		return TenantImportResult{}, fmt.Errorf("mysql: hash import administrator password: %w", err)
	}
	disabledSecret := make([]byte, 32)
	if _, err := crand.Read(disabledSecret); err != nil {
		return TenantImportResult{}, fmt.Errorf("mysql: generate disabled member credential: %w", err)
	}
	disabledHash, err := i.hasher.Hash(hex.EncodeToString(disabledSecret))
	if err != nil {
		return TenantImportResult{}, fmt.Errorf("mysql: hash disabled member credential: %w", err)
	}

	tx, err := i.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return TenantImportResult{}, fmt.Errorf("mysql: begin team import: %w", err)
	}
	defer tx.Rollback()
	var existingTenant string
	err = tx.QueryRowContext(ctx, `SELECT id FROM tenants ORDER BY id LIMIT 1 FOR UPDATE`).Scan(&existingTenant)
	if err == nil {
		return TenantImportResult{}, ErrTenantImportDeploymentNotEmpty
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return TenantImportResult{}, fmt.Errorf("mysql: check empty deployment: %w", err)
	}
	importedAt := i.clock.Now().UTC()
	ids := platform.NewIDGenerator(i.clock)
	manifest := archive.Manifest()
	if _, err := tx.ExecContext(ctx, `INSERT INTO tenants (id, name, status, created_at) VALUES (?, ?, 'active', ?)`, manifest.TenantID, manifest.TenantName, manifest.TenantCreatedAt.UTC()); err != nil {
		return TenantImportResult{}, fmt.Errorf("mysql: import tenant: %w", err)
	}
	if err := importMembers(ctx, tx, archive, manifest.TenantID, in.AdminUsername, adminHash, disabledHash); err != nil {
		return TenantImportResult{}, err
	}
	importState := &tenantImportState{
		tenantID: platform.ID(manifest.TenantID), model: in.EmbeddingModel, at: importedAt,
		ids: ids, store: &KnowledgeStore{ex: tx}, activeCards: make(map[string]struct{}),
	}
	for _, dataset := range importDatasetsAfterMembers {
		if err := importDataset(ctx, tx, archive, manifest.TenantID, dataset, importState); err != nil {
			return TenantImportResult{}, err
		}
	}
	var adminUserID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE tenant_id=? AND username=?`, manifest.TenantID, in.AdminUsername).Scan(&adminUserID); err != nil {
		return TenantImportResult{}, fmt.Errorf("mysql: resolve imported administrator for audit: %w", err)
	}
	auditMetadata, err := json.Marshal(map[string]any{
		"formatVersion": portableexport.FormatVersion, "schemaVersion": plan.SchemaVersion,
		"records": plan.Records, "membersDisabled": plan.MembersDisabledOnImport,
		"sourceCredentialsImported": false, "indexingStatus": importIndexingStatus(importState.indexable, in.EmbeddingModel, importState.queued),
		"indexingQueued": importState.queued, "indexModel": in.EmbeddingModel,
	})
	if err != nil {
		return TenantImportResult{}, fmt.Errorf("mysql: encode import audit metadata: %w", err)
	}
	auditID := ids.New()
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_logs (id,tenant_id,user_id,agent_id,source,kind,access,outcome,action,entity_type,entity_id,space_id,metadata,created_at) VALUES (?,?,?,NULL,'cli','governance','write','success','admin.import.apply','tenant',?,NULL,?,?)`, auditID, manifest.TenantID, adminUserID, manifest.TenantID, auditMetadata, importedAt); err != nil {
		return TenantImportResult{}, fmt.Errorf("mysql: record team import audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return TenantImportResult{}, fmt.Errorf("mysql: commit team import: %w", err)
	}
	return TenantImportResult{
		OK: true, Status: "imported", TenantID: plan.TenantID, AdminUsername: plan.SelectedAdmin,
		Records: plan.Records, MembersDisabled: plan.MembersDisabledOnImport,
		SourceCredentialsImported: false, AdminMustChangePassword: true,
		IndexingRequired: importState.indexable > 0,
		IndexingStatus:   importIndexingStatus(importState.indexable, in.EmbeddingModel, importState.queued),
		IndexingQueued:   importState.queued, IndexModel: in.EmbeddingModel, ImportedAt: importedAt,
	}, nil
}

type importDatasetSpec struct {
	name  string
	query string
	args  func(map[string]any, string) ([]any, error)
}

var importDatasetsAfterMembers = []importDatasetSpec{
	{name: "agents.jsonl", query: `INSERT INTO agents (id,tenant_id,user_id,slug,display_name,created_at) VALUES (?,?,?,?,?,?)`, args: importAgentArgs},
	{name: "spaces.jsonl", query: `INSERT INTO spaces (id,tenant_id,type,name,owner_user_id,created_at) VALUES (?,?,?,?,?,?)`, args: importSpaceArgs},
	{name: "space_permissions.jsonl", query: `INSERT INTO space_permissions (space_id,user_id,can_read,can_write,granted_by,created_at) VALUES (?,?,?,?,?,?)`, args: importPermissionArgs},
	{name: "memories.jsonl", query: `INSERT INTO memories (id,tenant_id,space_id,author_user_id,author_agent_id,title,body,status,copied_from_type,copied_from_id,version,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, args: importMemoryArgs},
	{name: "cards.jsonl", query: `INSERT INTO cards (id,tenant_id,space_id,author_user_id,author_agent_id,title,card_type,status,source,source_name,source_key,source_hash,body_original,body_distilled,body_understanding,body_why_keep,body_unresolved,copied_from_type,copied_from_id,version,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args: importCardArgs},
	{name: "card_recall_triggers.jsonl", query: `INSERT INTO card_recall_triggers (id,tenant_id,card_id,space_id,trigger_text,sort_order,created_at) VALUES (?,?,?,?,?,?,?)`, args: importTriggerArgs},
	{name: "labels.jsonl", query: `INSERT INTO labels (tenant_id,entity_type,entity_id,label,created_at) VALUES (?,?,?,?,?)`, args: importLabelArgs},
	{name: "links.jsonl", query: `INSERT INTO links (id,tenant_id,from_type,from_id,to_type,to_id,kind,note,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, args: importLinkArgs},
	{name: "shared_lines.jsonl", query: `INSERT INTO continuity_lines (id,tenant_id,owner_user_id,owner_agent_id,title,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`, args: importLineArgs},
	{name: "shared_line_positions.jsonl", query: `INSERT INTO continuity_positions (id,tenant_id,line_id,summary,interpretation_status,facts_used,next_step,writer_user_id,writer_agent_id,version,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, args: importPositionArgs},
	{name: "shared_line_position_history.jsonl", query: `INSERT INTO continuity_position_history (id,tenant_id,line_id,position_id,position_version,summary,interpretation_status,facts_used,next_step,writer_user_id,writer_agent_id,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, args: importHistoryArgs},
	{name: "shared_line_handoffs.jsonl", query: `INSERT INTO continuity_handoffs (id,tenant_id,line_id,writer_user_id,writer_agent_id,objective,completed,open_items,next_step,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, args: importHandoffArgs},
}

func importMembers(ctx context.Context, tx *sql.Tx, archive *portableexport.Archive, tenantID, adminUsername, adminHash, disabledHash string) error {
	return forEachImportRecord(archive, "members.jsonl", func(record map[string]any) error {
		username, err := importString(record, "username")
		if err != nil {
			return err
		}
		passwordHash, status := disabledHash, "disabled"
		if username == adminUsername {
			passwordHash, status = adminHash, "active"
		}
		args, err := importValues(record, tenantID,
			stringKey("id"), literal(tenantID), literal(username), literal(passwordHash), stringKey("display_name"), stringKey("role"), literal(true), literal(status), timeKey("created_at"))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO users (id,tenant_id,username,password_hash,display_name,role,must_change_pw,status,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, args...)
		return err
	})
}

func importDataset(ctx context.Context, tx *sql.Tx, archive *portableexport.Archive, tenantID string, spec importDatasetSpec, state *tenantImportState) error {
	return forEachImportRecord(archive, spec.name, func(record map[string]any) error {
		args, err := spec.args(record, tenantID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, spec.query, args...); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
		return state.afterRecord(ctx, spec.name, record)
	})
}

type tenantImportState struct {
	tenantID    platform.ID
	model       string
	at          time.Time
	ids         *platform.IDGenerator
	store       *KnowledgeStore
	activeCards map[string]struct{}
	indexable   int64
	queued      int64
}

func (s *tenantImportState) afterRecord(ctx context.Context, dataset string, record map[string]any) error {
	if s == nil {
		return nil
	}
	var (
		embedding knowledge.Embedding
		ok        bool
	)
	switch dataset {
	case "memories.jsonl":
		status, err := importString(record, "status")
		if err != nil || status != knowledge.MemoryActive {
			return err
		}
		id, err := importString(record, "id")
		if err != nil {
			return err
		}
		title, err := importString(record, "title")
		if err != nil {
			return err
		}
		body, err := importString(record, "body")
		if err != nil {
			return err
		}
		if strings.TrimSpace(title) == "" && strings.TrimSpace(body) == "" {
			return nil
		}
		s.indexable++
		if s.model == "" {
			return nil
		}
		embedding, ok = knowledge.NewPendingMemoryEmbedding(s.ids, s.tenantID, platform.ID(id), s.model, title, body, s.at)
	case "cards.jsonl":
		status, err := importString(record, "status")
		if err != nil || status != knowledge.CardActive {
			return err
		}
		id, err := importString(record, "id")
		if err != nil {
			return err
		}
		s.activeCards[id] = struct{}{}
		title, err := importString(record, "title")
		if err != nil {
			return err
		}
		distilled, err := importOptionalString(record, "body_distilled")
		if err != nil {
			return err
		}
		understanding, err := importOptionalString(record, "body_understanding")
		if err != nil {
			return err
		}
		if strings.TrimSpace(title) == "" && strings.TrimSpace(distilled) == "" && strings.TrimSpace(understanding) == "" {
			return nil
		}
		s.indexable++
		if s.model == "" {
			return nil
		}
		embedding, ok = knowledge.NewPendingCardEmbedding(s.ids, s.tenantID, platform.ID(id), s.model, title, distilled, understanding, s.at)
	case "card_recall_triggers.jsonl":
		cardID, err := importString(record, "card_id")
		if err != nil {
			return err
		}
		if _, active := s.activeCards[cardID]; !active {
			return nil
		}
		id, err := importString(record, "id")
		if err != nil {
			return err
		}
		trigger, err := importString(record, "trigger_text")
		if err != nil {
			return err
		}
		if strings.TrimSpace(trigger) == "" {
			return nil
		}
		s.indexable++
		if s.model == "" {
			return nil
		}
		embedding, ok = knowledge.NewPendingTriggerEmbedding(s.ids, s.tenantID, platform.ID(id), s.model, trigger, s.at)
	default:
		return nil
	}
	if !ok {
		return nil
	}
	if err := s.store.EnqueueEmbedding(ctx, embedding); err != nil {
		return fmt.Errorf("queue imported embedding: %w", err)
	}
	s.queued++
	return nil
}

func forEachImportRecord(archive *portableexport.Archive, name string, accept func(map[string]any) error) error {
	r, err := archive.OpenDataset(name)
	if err != nil {
		return err
	}
	defer r.Close()
	decoder := json.NewDecoder(r)
	decoder.UseNumber()
	var count int64
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("mysql: decode import %s record %d: %w", name, count+1, err)
		}
		count++
		if err := accept(record); err != nil {
			return fmt.Errorf("mysql: import %s record %d: %w", name, count, err)
		}
	}
	return nil
}

type importValue func(map[string]any) (any, error)

func importValues(record map[string]any, _ string, values ...importValue) ([]any, error) {
	out := make([]any, len(values))
	for index, value := range values {
		resolved, err := value(record)
		if err != nil {
			return nil, err
		}
		out[index] = resolved
	}
	return out, nil
}

func literal(value any) importValue { return func(map[string]any) (any, error) { return value, nil } }
func stringKey(key string) importValue {
	return func(record map[string]any) (any, error) { return importString(record, key) }
}
func nullableStringKey(key string) importValue {
	return func(record map[string]any) (any, error) {
		if record[key] == nil {
			return nil, nil
		}
		return importString(record, key)
	}
}
func boolKey(key string) importValue {
	return func(record map[string]any) (any, error) {
		value, ok := record[key].(bool)
		if !ok {
			return nil, fmt.Errorf("field %s is not boolean", key)
		}
		return value, nil
	}
}
func intKey(key string) importValue {
	return func(record map[string]any) (any, error) {
		value, ok := record[key].(json.Number)
		if !ok {
			return nil, fmt.Errorf("field %s is not an integer", key)
		}
		return value.Int64()
	}
}
func timeKey(key string) importValue {
	return func(record map[string]any) (any, error) {
		value, err := importString(record, key)
		if err != nil {
			return nil, err
		}
		stamp, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, fmt.Errorf("field %s is not a timestamp", key)
		}
		return stamp.UTC(), nil
	}
}
func jsonKey(key string) importValue {
	return func(record map[string]any) (any, error) {
		body, err := json.Marshal(record[key])
		if err != nil {
			return nil, fmt.Errorf("field %s is not JSON: %w", key, err)
		}
		return body, nil
	}
}

func importString(record map[string]any, key string) (string, error) {
	value, ok := record[key].(string)
	if !ok {
		return "", fmt.Errorf("field %s is not text", key)
	}
	return value, nil
}

func importOptionalString(record map[string]any, key string) (string, error) {
	if record[key] == nil {
		return "", nil
	}
	return importString(record, key)
}

func args(record map[string]any, tenantID string, values ...importValue) ([]any, error) {
	return importValues(record, tenantID, values...)
}

func importAgentArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("id"), literal(tenant), stringKey("user_id"), stringKey("slug"), stringKey("display_name"), timeKey("created_at"))
}
func importSpaceArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("id"), literal(tenant), stringKey("type"), stringKey("name"), nullableStringKey("owner_user_id"), timeKey("created_at"))
}
func importPermissionArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("space_id"), stringKey("user_id"), boolKey("can_read"), boolKey("can_write"), stringKey("granted_by"), timeKey("created_at"))
}
func importMemoryArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("id"), literal(tenant), stringKey("space_id"), stringKey("author_user_id"), nullableStringKey("author_agent_id"), stringKey("title"), stringKey("body"), stringKey("status"), nullableStringKey("copied_from_type"), nullableStringKey("copied_from_id"), intKey("version"), timeKey("created_at"), timeKey("updated_at"))
}
func importCardArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("id"), literal(tenant), stringKey("space_id"), stringKey("author_user_id"), nullableStringKey("author_agent_id"), stringKey("title"), stringKey("card_type"), stringKey("status"), nullableStringKey("source"), nullableStringKey("source_name"), nullableStringKey("source_key"), nullableStringKey("source_hash"), nullableStringKey("body_original"), nullableStringKey("body_distilled"), nullableStringKey("body_understanding"), nullableStringKey("body_why_keep"), nullableStringKey("body_unresolved"), nullableStringKey("copied_from_type"), nullableStringKey("copied_from_id"), intKey("version"), timeKey("created_at"), timeKey("updated_at"))
}
func importTriggerArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("id"), literal(tenant), stringKey("card_id"), stringKey("space_id"), stringKey("trigger_text"), intKey("sort_order"), timeKey("created_at"))
}
func importLabelArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, literal(tenant), stringKey("entity_type"), stringKey("entity_id"), stringKey("label"), timeKey("created_at"))
}
func importLinkArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("id"), literal(tenant), stringKey("from_type"), stringKey("from_id"), stringKey("to_type"), stringKey("to_id"), stringKey("kind"), nullableStringKey("note"), timeKey("created_at"))
}
func importLineArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("id"), literal(tenant), stringKey("owner_user_id"), stringKey("owner_agent_id"), stringKey("title"), stringKey("status"), timeKey("created_at"), timeKey("updated_at"))
}
func importPositionArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("id"), literal(tenant), stringKey("line_id"), stringKey("summary"), stringKey("interpretation_status"), jsonKey("facts_used"), stringKey("next_step"), stringKey("writer_user_id"), stringKey("writer_agent_id"), intKey("version"), timeKey("created_at"), timeKey("updated_at"))
}
func importHistoryArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("id"), literal(tenant), stringKey("line_id"), stringKey("position_id"), intKey("position_version"), stringKey("summary"), stringKey("interpretation_status"), jsonKey("facts_used"), stringKey("next_step"), stringKey("writer_user_id"), stringKey("writer_agent_id"), timeKey("created_at"))
}
func importHandoffArgs(r map[string]any, tenant string) ([]any, error) {
	return args(r, tenant, stringKey("id"), literal(tenant), stringKey("line_id"), stringKey("writer_user_id"), stringKey("writer_agent_id"), stringKey("objective"), jsonKey("completed"), jsonKey("open_items"), stringKey("next_step"), timeKey("created_at"))
}

func importIndexingStatus(indexable int64, model string, queued int64) string {
	if indexable == 0 {
		return "not_required"
	}
	if model == "" {
		return "backfill_required"
	}
	if queued == 0 {
		return "not_required"
	}
	return "queued"
}
