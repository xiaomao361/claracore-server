package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DataIntegrityIssue is deliberately content-free. Operators need a stable
// rule name and a count to decide whether a deployment is safe to serve; IDs,
// titles, bodies, and other tenant data belong in a separately authorized
// investigation, not in a routine machine-readable receipt.
type DataIntegrityIssue struct {
	Domain     string `json:"domain"`
	Rule       string `json:"rule"`
	Violations int64  `json:"violations"`
}

type DataIntegrityResult struct {
	OK            bool                 `json:"ok"`
	Status        string               `json:"status"`
	SchemaVersion string               `json:"schemaVersion"`
	Snapshot      string               `json:"snapshot"`
	CheckedAt     time.Time            `json:"checkedAt"`
	RulesChecked  int                  `json:"rulesChecked"`
	Violations    int64                `json:"violations"`
	Issues        []DataIntegrityIssue `json:"issues"`
}

type DataIntegrityChecker struct {
	db            *sql.DB
	schemaVersion string
}

func NewDataIntegrityChecker(db *sql.DB, schemaVersion string) *DataIntegrityChecker {
	return &DataIntegrityChecker{db: db, schemaVersion: schemaVersion}
}

type integrityRule struct {
	domain string
	name   string
	query  string
}

// integrityRules is intentionally fixed in the binary. This makes the receipt
// comparable across instances and keeps a database row from choosing SQL or
// causing arbitrary tenant data to be printed. Migrations intentionally use no
// foreign keys, so these rules are the independent operational check for the
// cross-table guarantees normally enforced by domain writes.
var integrityRules = []integrityRule{
	{domain: "governance", name: "user_tenant", query: `SELECT COUNT(*) FROM users u LEFT JOIN tenants t ON t.id=u.tenant_id WHERE t.id IS NULL`},
	{domain: "governance", name: "agent_identity", query: `SELECT COUNT(*) FROM agents a LEFT JOIN tenants t ON t.id=a.tenant_id LEFT JOIN users u ON u.id=a.user_id AND u.tenant_id=a.tenant_id WHERE t.id IS NULL OR u.id IS NULL`},
	{domain: "governance", name: "token_identity", query: `SELECT COUNT(*) FROM tokens x LEFT JOIN tenants t ON t.id=x.tenant_id LEFT JOIN users u ON u.id=x.user_id AND u.tenant_id=x.tenant_id LEFT JOIN agents a ON a.id=x.agent_id AND a.tenant_id=x.tenant_id AND a.user_id=x.user_id WHERE t.id IS NULL OR u.id IS NULL OR a.id IS NULL`},
	{domain: "governance", name: "space_owner", query: `SELECT COUNT(*) FROM spaces s LEFT JOIN tenants t ON t.id=s.tenant_id LEFT JOIN users u ON u.id=s.owner_user_id AND u.tenant_id=s.tenant_id WHERE t.id IS NULL OR s.type NOT IN ('personal','project','team') OR (s.type='team' AND s.owner_user_id IS NOT NULL) OR (s.type IN ('personal','project') AND (s.owner_user_id IS NULL OR u.id IS NULL))`},
	{domain: "governance", name: "space_permission_principal", query: `SELECT COUNT(*) FROM space_permissions p LEFT JOIN spaces s ON s.id=p.space_id LEFT JOIN users u ON u.id=p.user_id AND u.tenant_id=s.tenant_id WHERE s.id IS NULL OR u.id IS NULL`},
	{domain: "governance", name: "space_permission_grantor", query: `SELECT COUNT(*) FROM space_permissions p LEFT JOIN spaces s ON s.id=p.space_id LEFT JOIN users u ON u.id=p.granted_by AND u.tenant_id=s.tenant_id WHERE s.id IS NULL OR u.id IS NULL`},
	{domain: "governance", name: "session_identity", query: `SELECT COUNT(*) FROM sessions s LEFT JOIN tenants t ON t.id=s.tenant_id LEFT JOIN users u ON u.id=s.user_id AND u.tenant_id=s.tenant_id WHERE t.id IS NULL OR u.id IS NULL`},
	{domain: "governance", name: "audit_actor_scope", query: `SELECT COUNT(*) FROM audit_logs l LEFT JOIN tenants t ON t.id=l.tenant_id LEFT JOIN users u ON u.id=l.user_id AND u.tenant_id=l.tenant_id LEFT JOIN agents a ON a.id=l.agent_id AND a.tenant_id=l.tenant_id AND a.user_id=l.user_id LEFT JOIN spaces s ON s.id=l.space_id AND s.tenant_id=l.tenant_id WHERE t.id IS NULL OR u.id IS NULL OR (l.agent_id IS NOT NULL AND a.id IS NULL) OR (l.space_id IS NOT NULL AND s.id IS NULL)`},
	{domain: "knowledge", name: "memory_tenant", query: `SELECT COUNT(*) FROM memories m LEFT JOIN tenants t ON t.id=m.tenant_id WHERE t.id IS NULL`},
	{domain: "knowledge", name: "memory_space", query: `SELECT COUNT(*) FROM memories m LEFT JOIN spaces s ON s.id=m.space_id AND s.tenant_id=m.tenant_id WHERE s.id IS NULL`},
	{domain: "knowledge", name: "memory_author_user", query: `SELECT COUNT(*) FROM memories m LEFT JOIN users u ON u.id=m.author_user_id AND u.tenant_id=m.tenant_id WHERE u.id IS NULL`},
	{domain: "knowledge", name: "memory_author_agent", query: `SELECT COUNT(*) FROM memories m LEFT JOIN agents a ON a.id=m.author_agent_id AND a.tenant_id=m.tenant_id AND a.user_id=m.author_user_id WHERE m.author_agent_id IS NOT NULL AND a.id IS NULL`},
	{domain: "knowledge", name: "card_tenant", query: `SELECT COUNT(*) FROM cards c LEFT JOIN tenants t ON t.id=c.tenant_id WHERE t.id IS NULL`},
	{domain: "knowledge", name: "card_space", query: `SELECT COUNT(*) FROM cards c LEFT JOIN spaces s ON s.id=c.space_id AND s.tenant_id=c.tenant_id WHERE s.id IS NULL`},
	{domain: "knowledge", name: "card_author_user", query: `SELECT COUNT(*) FROM cards c LEFT JOIN users u ON u.id=c.author_user_id AND u.tenant_id=c.tenant_id WHERE u.id IS NULL`},
	{domain: "knowledge", name: "card_author_agent", query: `SELECT COUNT(*) FROM cards c LEFT JOIN agents a ON a.id=c.author_agent_id AND a.tenant_id=c.tenant_id AND a.user_id=c.author_user_id WHERE c.author_agent_id IS NOT NULL AND a.id IS NULL`},
	{domain: "knowledge", name: "card_trigger_scope", query: `SELECT COUNT(*) FROM card_recall_triggers r LEFT JOIN cards c ON c.id=r.card_id AND c.tenant_id=r.tenant_id AND c.space_id=r.space_id LEFT JOIN spaces s ON s.id=r.space_id AND s.tenant_id=r.tenant_id WHERE c.id IS NULL OR s.id IS NULL`},
	{domain: "knowledge", name: "label_entity", query: `SELECT COUNT(*) FROM labels l LEFT JOIN memories m ON l.entity_type='memory' AND m.id=l.entity_id AND m.tenant_id=l.tenant_id LEFT JOIN cards c ON l.entity_type='card' AND c.id=l.entity_id AND c.tenant_id=l.tenant_id WHERE (l.entity_type='memory' AND m.id IS NULL) OR (l.entity_type='card' AND c.id IS NULL) OR l.entity_type NOT IN ('memory','card')`},
	{domain: "knowledge", name: "link_source", query: `SELECT COUNT(*) FROM links l LEFT JOIN memories m ON l.from_type='memory' AND m.id=l.from_id AND m.tenant_id=l.tenant_id LEFT JOIN cards c ON l.from_type='card' AND c.id=l.from_id AND c.tenant_id=l.tenant_id WHERE (l.from_type='memory' AND m.id IS NULL) OR (l.from_type='card' AND c.id IS NULL) OR l.from_type NOT IN ('memory','card')`},
	{domain: "knowledge", name: "link_target", query: `SELECT COUNT(*) FROM links l LEFT JOIN memories m ON l.to_type='memory' AND m.id=l.to_id AND m.tenant_id=l.tenant_id LEFT JOIN cards c ON l.to_type='card' AND c.id=l.to_id AND c.tenant_id=l.tenant_id WHERE (l.to_type='memory' AND m.id IS NULL) OR (l.to_type='card' AND c.id IS NULL) OR l.to_type NOT IN ('memory','card')`},
	{domain: "knowledge", name: "embedding_entity", query: `SELECT COUNT(*) FROM embeddings e LEFT JOIN memories m ON e.entity_type='memory' AND m.id=e.entity_id AND m.tenant_id=e.tenant_id LEFT JOIN cards c ON e.entity_type='card' AND c.id=e.entity_id AND c.tenant_id=e.tenant_id LEFT JOIN card_recall_triggers r ON e.entity_type='card_trigger' AND r.id=e.entity_id AND r.tenant_id=e.tenant_id WHERE (e.entity_type='memory' AND m.id IS NULL) OR (e.entity_type='card' AND c.id IS NULL) OR (e.entity_type='card_trigger' AND r.id IS NULL) OR e.entity_type NOT IN ('memory','card','card_trigger')`},
	{domain: "continuity", name: "line_owner", query: `SELECT COUNT(*) FROM continuity_lines l LEFT JOIN tenants t ON t.id=l.tenant_id LEFT JOIN users u ON u.id=l.owner_user_id AND u.tenant_id=l.tenant_id LEFT JOIN agents a ON a.id=l.owner_agent_id AND a.tenant_id=l.tenant_id AND a.user_id=l.owner_user_id WHERE t.id IS NULL OR u.id IS NULL OR a.id IS NULL`},
	{domain: "continuity", name: "current_position", query: `SELECT COUNT(*) FROM continuity_positions p LEFT JOIN continuity_lines l ON l.id=p.line_id AND l.tenant_id=p.tenant_id LEFT JOIN users u ON u.id=p.writer_user_id AND u.tenant_id=p.tenant_id LEFT JOIN agents a ON a.id=p.writer_agent_id AND a.tenant_id=p.tenant_id AND a.user_id=p.writer_user_id WHERE l.id IS NULL OR u.id IS NULL OR a.id IS NULL`},
	{domain: "continuity", name: "position_history", query: `SELECT COUNT(*) FROM continuity_position_history h LEFT JOIN continuity_lines l ON l.id=h.line_id AND l.tenant_id=h.tenant_id LEFT JOIN continuity_positions p ON p.id=h.position_id AND p.line_id=h.line_id AND p.tenant_id=h.tenant_id LEFT JOIN users u ON u.id=h.writer_user_id AND u.tenant_id=h.tenant_id LEFT JOIN agents a ON a.id=h.writer_agent_id AND a.tenant_id=h.tenant_id AND a.user_id=h.writer_user_id WHERE l.id IS NULL OR p.id IS NULL OR u.id IS NULL OR a.id IS NULL`},
	{domain: "continuity", name: "handoff_writer", query: `SELECT COUNT(*) FROM continuity_handoffs h LEFT JOIN continuity_lines l ON l.id=h.line_id AND l.tenant_id=h.tenant_id LEFT JOIN users u ON u.id=h.writer_user_id AND u.tenant_id=h.tenant_id LEFT JOIN agents a ON a.id=h.writer_agent_id AND a.tenant_id=h.tenant_id AND a.user_id=h.writer_user_id WHERE l.id IS NULL OR u.id IS NULL OR a.id IS NULL`},
	{domain: "external", name: "source_scope", query: `SELECT COUNT(*) FROM external_sources x LEFT JOIN tenants t ON t.id=x.tenant_id LEFT JOIN spaces s ON s.id=x.space_id AND s.tenant_id=x.tenant_id WHERE t.id IS NULL OR s.id IS NULL`},
	{domain: "external", name: "sync_run_source", query: `SELECT COUNT(*) FROM external_sync_runs r LEFT JOIN external_sources s ON s.id=r.external_source_id AND s.tenant_id=r.tenant_id WHERE s.id IS NULL`},
	{domain: "external", name: "document_scope", query: `SELECT COUNT(*) FROM external_documents d LEFT JOIN external_sources s ON s.id=d.external_source_id AND s.tenant_id=d.tenant_id AND s.space_id=d.space_id LEFT JOIN external_sync_runs r ON r.id=d.last_seen_run_id AND r.tenant_id=d.tenant_id AND r.external_source_id=d.external_source_id WHERE s.id IS NULL OR r.id IS NULL`},
	{domain: "external", name: "document_vector", query: `SELECT COUNT(*) FROM external_document_vectors v LEFT JOIN external_documents d ON d.id=v.external_document_id AND d.tenant_id=v.tenant_id WHERE d.id IS NULL`},
}

func (c *DataIntegrityChecker) Check(ctx context.Context) (DataIntegrityResult, error) {
	if c == nil || c.db == nil || strings.TrimSpace(c.schemaVersion) == "" {
		return DataIntegrityResult{}, errors.New("mysql: invalid data integrity checker dependencies")
	}
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return DataIntegrityResult{}, fmt.Errorf("mysql: begin data integrity check: %w", err)
	}
	defer tx.Rollback()

	result := DataIntegrityResult{
		OK:            true,
		Status:        "verified",
		SchemaVersion: c.schemaVersion,
		Snapshot:      "repeatable_read_read_only",
		CheckedAt:     time.Now().UTC().Truncate(time.Millisecond),
		RulesChecked:  len(integrityRules),
		Issues:        make([]DataIntegrityIssue, 0),
	}
	for _, rule := range integrityRules {
		var violations int64
		if err := tx.QueryRowContext(ctx, rule.query).Scan(&violations); err != nil {
			return DataIntegrityResult{}, fmt.Errorf("mysql: check data integrity rule %s.%s: %w", rule.domain, rule.name, err)
		}
		if violations == 0 {
			continue
		}
		result.OK = false
		result.Status = "violations_found"
		result.Violations += violations
		result.Issues = append(result.Issues, DataIntegrityIssue{Domain: rule.domain, Rule: rule.name, Violations: violations})
	}
	if err := tx.Commit(); err != nil {
		return DataIntegrityResult{}, fmt.Errorf("mysql: finish data integrity check: %w", err)
	}
	return result, nil
}
