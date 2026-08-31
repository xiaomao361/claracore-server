package integritystatus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/ports"
)

func TestPublishAndReadHealthyReceiptAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrity-status.json")
	checkedAt := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if err := Publish(path, ports.IntegrityStatus{Verified: true, SchemaVersion: "0010", CheckedAt: checkedAt, MaxAge: 25 * time.Hour, RulesChecked: 29}); err != nil {
		t.Fatal(err)
	}
	status, err := NewFile(path).Read()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Verified || status.SchemaVersion != "0010" || !status.CheckedAt.Equal(checkedAt) || status.MaxAge != 25*time.Hour || status.RulesChecked != 29 || status.Violations != 0 || len(status.Issues) != 0 {
		t.Fatalf("unexpected integrity status: %+v", status)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt permission = %v err=%v", info.Mode().Perm(), err)
	}
}

func TestPublishAndReadViolationReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrity-status.json")
	status := ports.IntegrityStatus{
		SchemaVersion: "0010", CheckedAt: time.Now().UTC(), MaxAge: 25 * time.Hour,
		RulesChecked: 29, Violations: 2,
		Issues: []ports.IntegrityIssue{{Domain: "knowledge", Rule: "memory_space", Violations: 2}},
	}
	if err := Publish(path, status); err != nil {
		t.Fatal(err)
	}
	got, err := NewFile(path).Read()
	if err != nil || got.Verified || got.Violations != 2 || len(got.Issues) != 1 || got.Issues[0].Rule != "memory_space" {
		t.Fatalf("unexpected violation receipt: %+v err=%v", got, err)
	}
}

func TestFileRejectsUntrustedReceiptsAndTargets(t *testing.T) {
	valid := `{"format":"claracore-data-integrity/v1","status":"verified","schema_version":"0010","checked_at":"2026-08-29T12:00:00Z","max_age_seconds":90000,"rules_checked":29,"violations":0,"issues":[]}`
	for _, tc := range []struct{ name, body, want string }{
		{name: "unknown field", body: strings.TrimSuffix(valid, "}") + `,"secret":"x"}`, want: "unknown field"},
		{name: "non UTC", body: strings.Replace(valid, "12:00:00Z", "20:00:00+08:00", 1), want: "UTC"},
		{name: "false healthy", body: strings.Replace(valid, `"violations":0`, `"violations":1`, 1), want: "inconsistent"},
		{name: "trailing", body: valid + `{}`, want: "exactly one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "status.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewFile(path).Read(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "status.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Publish(link, ports.IntegrityStatus{Verified: true, SchemaVersion: "0010", CheckedAt: time.Now().UTC(), MaxAge: time.Hour, RulesChecked: 29}); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink target must fail: %v", err)
	}
}
