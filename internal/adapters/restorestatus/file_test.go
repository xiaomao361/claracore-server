package restorestatus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validReceipt = `{"ok":true,"database":"claracore_server","archive":"claracore-server_claracore_server_20260829T020000Z.sql.gz","archive_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","report":"claracore-server_claracore_server_20260829T020000Z.sql.gz.restore-report.tsv","report_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","restored_at":"20260829T030000Z","max_age_seconds":2592000,"table_count":25,"migration_count":10,"latest_migration":"0010","table_checks":"verified","row_counts":"recorded","target_cleanup":"verified","semantic_inspection":"not_proven"}`

func TestFileReadsBoundedTechnicalRestoreReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restore-status.json")
	if err := os.WriteFile(path, []byte(validReceipt), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := NewFile(path).Read()
	if err != nil {
		t.Fatal(err)
	}
	if status.Database != "claracore_server" || status.TableCount != 25 || status.MigrationCount != 10 || status.LatestMigration != "0010" ||
		!status.RestoredAt.Equal(time.Date(2026, 8, 29, 3, 0, 0, 0, time.UTC)) || status.MaxAge != 30*24*time.Hour ||
		!status.TechnicalRestoreVerified || status.SemanticInspection != "not_proven" {
		t.Fatalf("unexpected restore status: %+v", status)
	}
}

func TestFileRejectsUnsafeOrOverclaimingRestoreReceipt(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "unknown field", body: strings.TrimSuffix(validReceipt, "}") + `,"target_database":"secret"}`, want: "unknown field"},
		{name: "archive path", body: strings.Replace(validReceipt, `"archive":"claracore-server_`, `"archive":"../claracore-server_`, 1), want: "archive name"},
		{name: "database mismatch", body: strings.Replace(validReceipt, `"database":"claracore_server"`, `"database":"other"`, 1), want: "archive name"},
		{name: "report path", body: strings.Replace(validReceipt, `"report":"claracore-server_`, `"report":"../claracore-server_`, 1), want: "report name"},
		{name: "semantic overclaim", body: strings.Replace(validReceipt, "not_proven", "verified", 1), want: "evidence marker"},
		{name: "cleanup not verified", body: strings.Replace(validReceipt, `"target_cleanup":"verified"`, `"target_cleanup":"retained"`, 1), want: "evidence marker"},
		{name: "future archive", body: strings.Replace(validReceipt, "20260829T020000Z.sql.gz", "20260830T020000Z.sql.gz", 1), want: "restored_at"},
		{name: "trailing object", body: validReceipt + `{}`, want: "exactly one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "restore-status.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := NewFile(path).Read()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestFileRejectsSymlinkAndOversizedRestoreReceipt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(validReceipt), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "status.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFile(link).Read(); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink receipt must fail: %v", err)
	}

	large := filepath.Join(dir, "large.json")
	if err := os.WriteFile(large, make([]byte, maxReceiptBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFile(large).Read(); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("oversized receipt must fail: %v", err)
	}
}
