//go:build integration

package mysql_test

import (
	"context"
	"errors"
	"testing"
	"time"

	cryptoadapter "claracore-server/internal/adapters/crypto"
	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

func TestPersistedAuthVersionRejectsLateOldPasswordSession(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	clock := &platform.FixedClock{T: now}
	ids := platform.NewIDGenerator(clock)
	store := mysql.NewGovernanceStore(db)
	sessions := mysql.NewSessionStore(db)

	tenant := governance.Tenant{ID: ids.New(), Name: "session-race-it", Status: governance.StatusActive, CreatedAt: now}
	user := governance.User{
		ID: ids.New(), TenantID: tenant.ID, Username: "session-race-user", PasswordHash: "old-password-hash",
		DisplayName: "Session Race", Role: governance.RoleAdmin, Status: governance.StatusActive, CreatedAt: now,
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM sessions WHERE user_id = ?`, string(user.ID))
		_, _ = db.ExecContext(context.Background(), `DELETE FROM users WHERE id = ?`, string(user.ID))
		_, _ = db.ExecContext(context.Background(), `DELETE FROM tenants WHERE id = ?`, string(tenant.ID))
	})
	if err := store.CreateTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	created, err := store.FindUserByID(ctx, tenant.ID, user.ID)
	if err != nil || created.AuthVersion != 1 {
		t.Fatalf("initial auth version = %+v err=%v", created, err)
	}

	const plaintext = "late-session-token"
	late := ports.Session{
		TokenHash: hashOf(plaintext), TenantID: string(tenant.ID), UserID: string(user.ID), AuthVersion: created.AuthVersion,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now, LastSeenAt: now,
	}
	if err := store.UpdateUserPassword(ctx, tenant.ID, user.ID, "new-password-hash", true, now); err != nil {
		t.Fatal(err)
	}
	// This insert deliberately happens after the reset update and models the
	// login that verified the old hash before the reset committed.
	if err := sessions.Create(ctx, late); err != nil {
		t.Fatal(err)
	}

	svc := governance.NewService(store, clock, ids, cryptoadapter.NewArgon2id(cryptoadapter.Argon2idParams{}), sessions)
	if _, err := svc.ResumeSession(ctx, plaintext); !errors.Is(err, governance.ErrSessionInvalid) {
		t.Fatalf("late stale session must be rejected, got %v", err)
	}
	if _, found, err := sessions.Find(ctx, late.TokenHash); err != nil || found {
		t.Fatalf("stale session should be deleted: found=%v err=%v", found, err)
	}
	updated, err := store.FindUserByID(ctx, tenant.ID, user.ID)
	if err != nil || updated.AuthVersion != 2 {
		t.Fatalf("updated auth version = %+v err=%v", updated, err)
	}
	if err := store.UpdateUserStatus(ctx, tenant.ID, user.ID, governance.StatusDisabled, now); err != nil {
		t.Fatal(err)
	}
	disabled, err := store.FindUserByID(ctx, tenant.ID, user.ID)
	if err != nil || disabled.AuthVersion != 3 {
		t.Fatalf("disable auth version = %+v err=%v", disabled, err)
	}
	if err := store.UpdateUserStatus(ctx, tenant.ID, user.ID, governance.StatusDisabled, now); err != nil {
		t.Fatal(err)
	}
	idempotent, err := store.FindUserByID(ctx, tenant.ID, user.ID)
	if err != nil || idempotent.AuthVersion != 3 {
		t.Fatalf("idempotent disable must not advance auth version: %+v err=%v", idempotent, err)
	}
}
