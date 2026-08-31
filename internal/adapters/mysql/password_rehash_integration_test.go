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
)

func TestPasswordHashUpgradeCASPreservesCredentialGeneration(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewGovernanceStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)

	weakHasher := cryptoadapter.NewArgon2id(cryptoadapter.Argon2idParams{
		Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	})
	currentHasher := cryptoadapter.NewArgon2id(cryptoadapter.Argon2idParams{
		Memory: 16 * 1024, Iterations: 2, Parallelism: 1, SaltLength: 16, KeyLength: 32,
	})
	weakHash, err := weakHasher.Hash("integration-password")
	if err != nil {
		t.Fatal(err)
	}
	currentHash, err := currentHasher.Hash("integration-password")
	if err != nil {
		t.Fatal(err)
	}
	staleReplacement, err := currentHasher.Hash("stale-replacement")
	if err != nil {
		t.Fatal(err)
	}

	errRollback := errors.New("password rehash integration test rollback")
	err = store.Atomically(ctx, func(st governance.Store) error {
		tenant := governance.Tenant{
			ID: ids.New(), Name: "密码升级 CAS 测试", Status: governance.StatusActive, CreatedAt: now,
		}
		user := governance.User{
			ID: ids.New(), TenantID: tenant.ID, Username: "rehash-cas", PasswordHash: weakHash,
			AuthVersion: governance.InitialAuthVersion, DisplayName: "密码升级测试",
			Role: governance.RoleMember, MustChangePw: true, Status: governance.StatusActive, CreatedAt: now,
		}
		if err := st.CreateTenant(ctx, tenant); err != nil {
			return err
		}
		if err := st.CreateUser(ctx, user); err != nil {
			return err
		}

		updated, err := st.UpgradeUserPasswordHash(ctx, tenant.ID, user.ID, weakHash, currentHash, user.AuthVersion)
		if err != nil {
			return err
		}
		if !updated {
			t.Fatal("匹配旧哈希和认证代次时应当升级")
		}
		got, err := st.FindUserByID(ctx, tenant.ID, user.ID)
		if err != nil {
			return err
		}
		if got.PasswordHash != currentHash || got.AuthVersion != user.AuthVersion || !got.MustChangePw {
			t.Fatalf("升级只能替换派生哈希，不能改变认证代次或改密标志: %+v", got)
		}

		updated, err = st.UpgradeUserPasswordHash(ctx, tenant.ID, user.ID, weakHash, staleReplacement, user.AuthVersion)
		if err != nil {
			return err
		}
		if updated {
			t.Fatal("旧哈希已经变化后，过期 CAS 不得覆盖新哈希")
		}
		updated, err = st.UpgradeUserPasswordHash(ctx, tenant.ID, user.ID, currentHash, staleReplacement, user.AuthVersion+1)
		if err != nil {
			return err
		}
		if updated {
			t.Fatal("认证代次不匹配时不得升级")
		}
		got, err = st.FindUserByID(ctx, tenant.ID, user.ID)
		if err != nil {
			return err
		}
		if got.PasswordHash != currentHash {
			t.Fatal("失败的 CAS 改写了当前哈希")
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("事务验证失败: %v", err)
	}
}
