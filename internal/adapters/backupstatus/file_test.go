package backupstatus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validReceipt = `{"ok":true,"database":"claracore_server","archive":"claracore-server_claracore_server_20260829T020000Z.sql.gz","created_at":"20260829T020000Z","verified_at":"20260829T020100Z","age_seconds":60,"max_age_seconds":93600,"sha256":"verified","gzip":"verified","recoverability":"not_proven"}`

func TestFileReadsBoundedVerifiedReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup-status.json")
	if err := os.WriteFile(path, []byte(validReceipt), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := NewFile(path).Read()
	if err != nil {
		t.Fatal(err)
	}
	if status.Database != "claracore_server" || status.Archive != "claracore-server_claracore_server_20260829T020000Z.sql.gz" ||
		!status.CreatedAt.Equal(time.Date(2026, 8, 29, 2, 0, 0, 0, time.UTC)) || status.MaxAge != 26*time.Hour ||
		!status.IntegrityVerified || status.Recoverability != "not_proven" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestFileRejectsUnsafeOrUntrustedReceipt(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "unknown field", body: strings.TrimSuffix(validReceipt, "}") + `,"password":"secret"}`, want: "unknown field"},
		{name: "archive path", body: strings.Replace(validReceipt, `"archive":"claracore-server_`, `"archive":"../claracore-server_`, 1), want: "archive name"},
		{name: "archive timestamp mismatch", body: strings.Replace(validReceipt, "20260829T020000Z.sql.gz", "20260829T030000Z.sql.gz", 1), want: "created_at"},
		{name: "recoverability overclaim", body: strings.Replace(validReceipt, "not_proven", "proven", 1), want: "recoverability"},
		{name: "trailing object", body: validReceipt + `{}`, want: "exactly one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "backup-status.json")
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

func TestFileRejectsSymlinkAndOversizedReceipt(t *testing.T) {
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
