// Package portableexport defines and verifies ClaraCore's versioned team export format.
// It deliberately depends only on the Go standard library so verification can run
// without configuration, a database, Redis, or an embedding service.
package portableexport

import "time"

const (
	FormatName      = "claracore-team-export"
	FormatVersion   = 1
	ManifestName    = "manifest.json"
	ReadmeName      = "README.txt"
	manifestMaxSize = 1 << 20
)

var datasetNames = []string{
	"members.jsonl",
	"agents.jsonl",
	"spaces.jsonl",
	"space_permissions.jsonl",
	"memories.jsonl",
	"cards.jsonl",
	"card_recall_triggers.jsonl",
	"labels.jsonl",
	"links.jsonl",
	"shared_lines.jsonl",
	"shared_line_positions.jsonl",
	"shared_line_position_history.jsonl",
	"shared_line_handoffs.jsonl",
}

var excludedNames = []string{
	"password_hashes",
	"token_hashes",
	"sessions",
	"embeddings",
	"audit_logs",
	"external_mirrors",
	"runtime_queues",
}

type FileReceipt struct {
	Name    string `json:"name"`
	Records int64  `json:"records"`
	SHA256  string `json:"sha256"`
}

type Manifest struct {
	Format          string        `json:"format"`
	FormatVersion   int           `json:"format_version"`
	SchemaVersion   string        `json:"schema_version"`
	TenantID        string        `json:"tenant_id"`
	TenantName      string        `json:"tenant_name"`
	TenantCreatedAt time.Time     `json:"tenant_created_at"`
	GeneratedAt     time.Time     `json:"generated_at"`
	Complete        bool          `json:"complete"`
	Files           []FileReceipt `json:"files"`
	Excluded        []string      `json:"excluded"`
}

// DataFiles returns the canonical JSONL files for format version 1.
func DataFiles() []string {
	return append([]string(nil), datasetNames...)
}

// ContentFiles returns every file that must be listed in manifest.files.
func ContentFiles() []string {
	return append([]string{ReadmeName}, datasetNames...)
}

// ExcludedData returns the categories that format version 1 must explicitly exclude.
func ExcludedData() []string {
	return append([]string(nil), excludedNames...)
}
