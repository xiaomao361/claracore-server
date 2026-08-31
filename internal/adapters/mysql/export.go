package mysql

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"claracore-server/internal/platform"
	"claracore-server/internal/portableexport"
	"claracore-server/internal/ports"
)

type TenantExporter struct {
	db            *sql.DB
	schemaVersion string
}

func NewTenantExporter(db *sql.DB, schemaVersion string) *TenantExporter {
	return &TenantExporter{db: db, schemaVersion: schemaVersion}
}

type exportColumn struct {
	name string
	kind exportValueKind
}

type exportValueKind uint8

const (
	exportText exportValueKind = iota
	exportInteger
	exportBoolean
	exportTime
	exportJSON
)

type exportDataset struct {
	name    string
	query   string
	columns []exportColumn
}

func (e *TenantExporter) Export(ctx context.Context, in ports.TenantExportInput, dst io.Writer) (ports.TenantExportResult, error) {
	if e == nil || e.db == nil || in.TenantID == "" || in.GeneratedAt.IsZero() || in.MaxBytes <= 0 || dst == nil {
		return ports.TenantExportResult{}, fmt.Errorf("mysql: invalid tenant export input")
	}
	generatedAt := in.GeneratedAt.UTC()
	limited := &exportLimitWriter{dst: dst, remaining: in.MaxBytes}
	tx, err := e.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return ports.TenantExportResult{}, fmt.Errorf("mysql: begin tenant export snapshot: %w", err)
	}
	defer tx.Rollback()

	var tenantName string
	var tenantCreatedAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT name, created_at FROM tenants WHERE id = ?`, in.TenantID).Scan(&tenantName, &tenantCreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ports.TenantExportResult{}, fmt.Errorf("mysql: export tenant not found")
		}
		return ports.TenantExportResult{}, fmt.Errorf("mysql: read export tenant: %w", err)
	}

	zw := zip.NewWriter(limited)
	receipts := make([]portableexport.FileReceipt, 0, len(tenantExportDatasets)+1)
	readme := []byte("ClaraCore portable team export\n\nThis archive contains tenant-scoped governance identities (without credentials), spaces, Memory, Card, labels, links, and Shared Line history. It is a portability format, not a database restore image. Password hashes, token hashes, sessions, embeddings, audit logs, external mirrors, and runtime queues are excluded. Use the matching release binary with -verify-export and -plan-import before any migration. Import is only supported into an empty deployment and requires an explicit administrator takeover.\n")
	receipt, err := writeExportBytes(zw, "README.txt", readme, generatedAt)
	if err != nil {
		return ports.TenantExportResult{}, exportWriteError(err)
	}
	receipts = append(receipts, receipt)

	var recordCount int64
	for _, dataset := range tenantExportDatasets {
		receipt, err := writeExportDataset(ctx, tx, zw, in.TenantID, generatedAt, dataset)
		if err != nil {
			return ports.TenantExportResult{}, exportWriteError(err)
		}
		receipts = append(receipts, receipt)
		recordCount += receipt.Records
	}
	manifest := portableexport.Manifest{
		Format: portableexport.FormatName, FormatVersion: portableexport.FormatVersion,
		SchemaVersion: e.schemaVersion, TenantID: string(in.TenantID), TenantName: tenantName,
		TenantCreatedAt: tenantCreatedAt.UTC(),
		GeneratedAt:     generatedAt, Complete: true, Files: receipts,
		Excluded: portableexport.ExcludedData(),
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ports.TenantExportResult{}, fmt.Errorf("mysql: encode tenant export manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if _, err := writeExportBytes(zw, portableexport.ManifestName, manifestBytes, generatedAt); err != nil {
		return ports.TenantExportResult{}, exportWriteError(err)
	}
	if err := zw.Close(); err != nil {
		return ports.TenantExportResult{}, exportWriteError(err)
	}
	if err := tx.Commit(); err != nil {
		return ports.TenantExportResult{}, fmt.Errorf("mysql: finish tenant export snapshot: %w", err)
	}
	return ports.TenantExportResult{
		FormatVersion: portableexport.FormatVersion, GeneratedAt: generatedAt,
		RecordCount: recordCount, BytesWritten: limited.written,
	}, nil
}

func exportWriteError(err error) error {
	if errors.Is(err, ports.ErrTenantExportTooLarge) {
		return fmt.Errorf("mysql: %w", err)
	}
	return fmt.Errorf("mysql: write tenant export: %w", err)
}

type exportLimitWriter struct {
	dst       io.Writer
	remaining int64
	written   int64
}

func (w *exportLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ports.ErrTenantExportTooLarge
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	w.written += int64(n)
	return n, err
}

func writeExportBytes(zw *zip.Writer, name string, body []byte, generatedAt time.Time) (portableexport.FileReceipt, error) {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetModTime(generatedAt)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return portableexport.FileReceipt{}, err
	}
	hash := sha256.New()
	if _, err := io.MultiWriter(w, hash).Write(body); err != nil {
		return portableexport.FileReceipt{}, err
	}
	return portableexport.FileReceipt{Name: name, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func writeExportDataset(ctx context.Context, tx *sql.Tx, zw *zip.Writer, tenantID platform.ID, generatedAt time.Time, dataset exportDataset) (portableexport.FileReceipt, error) {
	header := &zip.FileHeader{Name: dataset.name, Method: zip.Deflate}
	header.SetModTime(generatedAt)
	w, err := zw.CreateHeader(header)
	if err != nil {
		return portableexport.FileReceipt{}, err
	}
	hash := sha256.New()
	encoder := json.NewEncoder(io.MultiWriter(w, hash))
	encoder.SetEscapeHTML(false)
	rows, err := tx.QueryContext(ctx, dataset.query, tenantID)
	if err != nil {
		return portableexport.FileReceipt{}, fmt.Errorf("query %s: %w", dataset.name, err)
	}
	defer rows.Close()
	var count int64
	for rows.Next() {
		values := make([]any, len(dataset.columns))
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return portableexport.FileReceipt{}, fmt.Errorf("scan %s: %w", dataset.name, err)
		}
		record := make(map[string]any, len(values))
		for i, column := range dataset.columns {
			value, err := exportValue(values[i], column.kind)
			if err != nil {
				return portableexport.FileReceipt{}, fmt.Errorf("encode %s.%s: %w", dataset.name, column.name, err)
			}
			record[column.name] = value
		}
		if err := encoder.Encode(record); err != nil {
			return portableexport.FileReceipt{}, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return portableexport.FileReceipt{}, fmt.Errorf("iterate %s: %w", dataset.name, err)
	}
	return portableexport.FileReceipt{Name: dataset.name, Records: count, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func exportValue(value any, kind exportValueKind) (any, error) {
	if value == nil {
		return nil, nil
	}
	if t, ok := value.(time.Time); ok {
		if kind != exportTime {
			return nil, fmt.Errorf("unexpected timestamp")
		}
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	text := ""
	switch value := value.(type) {
	case []byte:
		text = string(value)
	case string:
		text = value
	case int64:
		text = strconv.FormatInt(value, 10)
	case uint64:
		text = strconv.FormatUint(value, 10)
	default:
		return nil, fmt.Errorf("unsupported SQL value %T", value)
	}
	switch kind {
	case exportText:
		return text, nil
	case exportInteger:
		return strconv.ParseInt(text, 10, 64)
	case exportBoolean:
		if text == "0" {
			return false, nil
		}
		if text == "1" {
			return true, nil
		}
		return nil, fmt.Errorf("invalid boolean %q", text)
	case exportTime:
		parsed, err := time.Parse("2006-01-02 15:04:05.999999", text)
		if err != nil {
			return nil, err
		}
		return parsed.UTC().Format(time.RFC3339Nano), nil
	case exportJSON:
		raw := json.RawMessage(append([]byte(nil), text...))
		if !json.Valid(raw) {
			return nil, fmt.Errorf("invalid JSON")
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("unknown export value kind")
	}
}

func columns(kind exportValueKind, names ...string) []exportColumn {
	out := make([]exportColumn, len(names))
	for i, name := range names {
		out[i] = exportColumn{name: name, kind: kind}
	}
	return out
}

func joinColumns(groups ...[]exportColumn) []exportColumn {
	var out []exportColumn
	for _, group := range groups {
		out = append(out, group...)
	}
	return out
}

var tenantExportDatasets = []exportDataset{
	{name: "members.jsonl", query: `SELECT id, username, display_name, role, must_change_pw, status, created_at FROM users WHERE tenant_id = ? ORDER BY id`, columns: joinColumns(columns(exportText, "id", "username", "display_name", "role"), columns(exportBoolean, "must_change_password"), columns(exportText, "status"), columns(exportTime, "created_at"))},
	{name: "agents.jsonl", query: `SELECT id, user_id, slug, display_name, created_at FROM agents WHERE tenant_id = ? ORDER BY id`, columns: joinColumns(columns(exportText, "id", "user_id", "slug", "display_name"), columns(exportTime, "created_at"))},
	{name: "spaces.jsonl", query: `SELECT id, type, name, owner_user_id, created_at FROM spaces WHERE tenant_id = ? ORDER BY id`, columns: joinColumns(columns(exportText, "id", "type", "name", "owner_user_id"), columns(exportTime, "created_at"))},
	{name: "space_permissions.jsonl", query: `SELECT p.space_id, p.user_id, p.can_read, p.can_write, p.granted_by, p.created_at FROM space_permissions p JOIN spaces s ON s.id = p.space_id WHERE s.tenant_id = ? ORDER BY p.space_id, p.user_id`, columns: joinColumns(columns(exportText, "space_id", "user_id"), columns(exportBoolean, "can_read", "can_write"), columns(exportText, "granted_by"), columns(exportTime, "created_at"))},
	{name: "memories.jsonl", query: `SELECT id, space_id, author_user_id, author_agent_id, title, body, status, copied_from_type, copied_from_id, version, created_at, updated_at FROM memories WHERE tenant_id = ? ORDER BY id`, columns: joinColumns(columns(exportText, "id", "space_id", "author_user_id", "author_agent_id", "title", "body", "status", "copied_from_type", "copied_from_id"), columns(exportInteger, "version"), columns(exportTime, "created_at", "updated_at"))},
	{name: "cards.jsonl", query: `SELECT id, space_id, author_user_id, author_agent_id, title, card_type, status, source, source_name, source_key, source_hash, body_original, body_distilled, body_understanding, body_why_keep, body_unresolved, copied_from_type, copied_from_id, version, created_at, updated_at FROM cards WHERE tenant_id = ? ORDER BY id`, columns: joinColumns(columns(exportText, "id", "space_id", "author_user_id", "author_agent_id", "title", "card_type", "status", "source", "source_name", "source_key", "source_hash", "body_original", "body_distilled", "body_understanding", "body_why_keep", "body_unresolved", "copied_from_type", "copied_from_id"), columns(exportInteger, "version"), columns(exportTime, "created_at", "updated_at"))},
	{name: "card_recall_triggers.jsonl", query: `SELECT id, card_id, space_id, trigger_text, sort_order, created_at FROM card_recall_triggers WHERE tenant_id = ? ORDER BY card_id, sort_order, id`, columns: joinColumns(columns(exportText, "id", "card_id", "space_id", "trigger_text"), columns(exportInteger, "sort_order"), columns(exportTime, "created_at"))},
	{name: "labels.jsonl", query: `SELECT entity_type, entity_id, label, created_at FROM labels WHERE tenant_id = ? ORDER BY entity_type, entity_id, label`, columns: joinColumns(columns(exportText, "entity_type", "entity_id", "label"), columns(exportTime, "created_at"))},
	{name: "links.jsonl", query: `SELECT id, from_type, from_id, to_type, to_id, kind, note, created_at FROM links WHERE tenant_id = ? ORDER BY id`, columns: joinColumns(columns(exportText, "id", "from_type", "from_id", "to_type", "to_id", "kind", "note"), columns(exportTime, "created_at"))},
	{name: "shared_lines.jsonl", query: `SELECT id, owner_user_id, owner_agent_id, title, status, created_at, updated_at FROM continuity_lines WHERE tenant_id = ? ORDER BY id`, columns: joinColumns(columns(exportText, "id", "owner_user_id", "owner_agent_id", "title", "status"), columns(exportTime, "created_at", "updated_at"))},
	{name: "shared_line_positions.jsonl", query: `SELECT id, line_id, summary, interpretation_status, facts_used, next_step, writer_user_id, writer_agent_id, version, created_at, updated_at FROM continuity_positions WHERE tenant_id = ? ORDER BY line_id, id`, columns: joinColumns(columns(exportText, "id", "line_id", "summary", "interpretation_status"), columns(exportJSON, "facts_used"), columns(exportText, "next_step", "writer_user_id", "writer_agent_id"), columns(exportInteger, "version"), columns(exportTime, "created_at", "updated_at"))},
	{name: "shared_line_position_history.jsonl", query: `SELECT id, line_id, position_id, position_version, summary, interpretation_status, facts_used, next_step, writer_user_id, writer_agent_id, created_at FROM continuity_position_history WHERE tenant_id = ? ORDER BY line_id, created_at, id`, columns: joinColumns(columns(exportText, "id", "line_id", "position_id"), columns(exportInteger, "position_version"), columns(exportText, "summary", "interpretation_status"), columns(exportJSON, "facts_used"), columns(exportText, "next_step", "writer_user_id", "writer_agent_id"), columns(exportTime, "created_at"))},
	{name: "shared_line_handoffs.jsonl", query: `SELECT id, line_id, writer_user_id, writer_agent_id, objective, completed, open_items, next_step, created_at FROM continuity_handoffs WHERE tenant_id = ? ORDER BY line_id, created_at, id`, columns: joinColumns(columns(exportText, "id", "line_id", "writer_user_id", "writer_agent_id", "objective"), columns(exportJSON, "completed", "open_items"), columns(exportText, "next_step"), columns(exportTime, "created_at"))},
}
