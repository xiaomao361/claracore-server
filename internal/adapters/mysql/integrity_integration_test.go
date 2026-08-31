//go:build integration

package mysql_test

import (
	"context"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/platform"
)

func TestDataIntegrityCheckerUsesReadOnlySnapshotAndFindsBoundedViolations(t *testing.T) {
	db := openTemporaryImportDatabase(t)
	ctx := context.Background()

	clean, err := mysql.NewDataIntegrityChecker(db, "0010").Check(ctx)
	if err != nil {
		t.Fatalf("check empty isolated deployment: %v", err)
	}
	if !clean.OK || clean.Status != "verified" || clean.SchemaVersion != "0010" || clean.Snapshot != "repeatable_read_read_only" || clean.RulesChecked < 20 || clean.Violations != 0 || len(clean.Issues) != 0 {
		t.Fatalf("unexpected clean integrity receipt: %+v", clean)
	}

	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := db.ExecContext(ctx, `INSERT INTO users (id,tenant_id,username,password_hash,display_name,role,must_change_pw,status,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, ids.New(), ids.New(), "orphan-user", "not-a-real-secret", "Orphan", "member", 1, "disabled", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO labels (tenant_id,entity_type,entity_id,label,created_at) VALUES (?,?,?,?,?)`, ids.New(), "card", ids.New(), "orphan", now); err != nil {
		t.Fatal(err)
	}

	broken, err := mysql.NewDataIntegrityChecker(db, "0010").Check(ctx)
	if err != nil {
		t.Fatalf("check deliberately inconsistent deployment: %v", err)
	}
	if broken.OK || broken.Status != "violations_found" || broken.Violations != 2 || len(broken.Issues) != 2 {
		t.Fatalf("unexpected violation receipt: %+v", broken)
	}
	if broken.Issues[0].Domain != "governance" || broken.Issues[0].Rule != "user_tenant" || broken.Issues[0].Violations != 1 {
		t.Fatalf("first issue is not stable and bounded: %+v", broken.Issues[0])
	}
	if broken.Issues[1].Domain != "knowledge" || broken.Issues[1].Rule != "label_entity" || broken.Issues[1].Violations != 1 {
		t.Fatalf("second issue is not stable and bounded: %+v", broken.Issues[1])
	}

	var userCount, labelCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&userCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM labels`).Scan(&labelCount); err != nil {
		t.Fatal(err)
	}
	if userCount != 1 || labelCount != 1 {
		t.Fatalf("read-only checker changed data: users=%d labels=%d", userCount, labelCount)
	}
}
