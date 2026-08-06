//go:build integration

// 集成测试：验证迁移在真实 MySQL 上产生的结构符合设计意图。
//
// 与 schema_test.go 的分工：那边检查 SQL 文本（进 CI，不需要库），
// 这边检查库里的实际结果（需要真实 MySQL，用 make itest 跑）。
// 前者能挡住写错的迁移，挡不住「MySQL 接受了但语义不是你以为的那样」。
//
// 所有写入都在事务内并最终 rollback，不留痕迹。
package test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/config"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	// config.Load 读的是相对路径 .env，而测试的工作目录是 test/
	t.Chdir(repoRoot(t))

	cfg, err := config.Load()
	if err != nil {
		t.Skipf("配置不可用，跳过集成测试: %v", err)
	}
	db, err := mysql.Open(context.Background(), cfg.MySQL.DSN(), mysql.PoolConfig{
		MaxOpenConns: 4, MaxIdleConns: 2, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Skipf("数据库不可达，跳过集成测试: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestFixedCharColumnsAreAsciiBin 确认 ascii_bin 真的落到了库里。
// 迁移文件写了不代表生效——列级字符集会被表级默认值以意外的方式覆盖。
func TestFixedCharColumnsAreAsciiBin(t *testing.T) {
	db := openDB(t)

	rows, err := db.Query(`
		SELECT TABLE_NAME, COLUMN_NAME, COLLATION_NAME
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()
		  AND DATA_TYPE = 'char'
		ORDER BY TABLE_NAME, COLUMN_NAME`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var table, column string
		var collation sql.NullString
		if err := rows.Scan(&table, &column, &collation); err != nil {
			t.Fatal(err)
		}
		n++
		if collation.String != "ascii_bin" {
			t.Errorf("%s.%s 的排序规则是 %q，应为 ascii_bin\n  原因: ID/hash 是纯 ASCII，utf8mb4 让索引膨胀 4 倍且引入无意义的大小写折叠",
				table, column, collation.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("没有查到任何 CHAR 列——迁移可能没跑，先执行 make migrate")
	}
	t.Logf("检查了 %d 个定长 CHAR 列", n)
}

// TestSpaceUniquenessConstraints 验证 02-data-model.md §2.2 的两条设计约束
// 已由数据库强制，而非仅靠应用层的 SELECT-then-INSERT（并发下必失效）。
func TestSpaceUniquenessConstraints(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 测试数据一律回滚，不落库

	const insert = `INSERT INTO spaces (id, tenant_id, type, name, owner_user_id, created_at)
	                VALUES (?, ?, ?, ?, ?, UTC_TIMESTAMP(3))`

	const (
		tenantA = "01TESTTENANTAAAAAAAAAAAAAA"
		tenantB = "01TESTTENANTBBBBBBBBBBBBBB"
		userA   = "01TESTUSERAAAAAAAAAAAAAAAA"
	)

	t.Run("每用户只能有一个 personal 空间", func(t *testing.T) {
		if _, err := tx.ExecContext(ctx, insert, "01TESTSPACEPERSONAL1AAAAAA", tenantA, "personal", "个人", userA); err != nil {
			t.Fatalf("第一个 personal 空间应当插入成功: %v", err)
		}
		_, err := tx.ExecContext(ctx, insert, "01TESTSPACEPERSONAL2AAAAAA", tenantA, "personal", "又一个个人", userA)
		if err == nil {
			t.Fatal("同一用户插入第二个 personal 空间竟然成功了——uk_spaces_personal 没起作用")
		}
		if !isDuplicateKey(err) {
			t.Fatalf("期望唯一键冲突，实际: %v", err)
		}
	})

	t.Run("每租户只能有一个 team 空间", func(t *testing.T) {
		if _, err := tx.ExecContext(ctx, insert, "01TESTSPACETEAM1AAAAAAAAAA", tenantB, "team", "团队", nil); err != nil {
			t.Fatalf("第一个 team 空间应当插入成功: %v", err)
		}
		_, err := tx.ExecContext(ctx, insert, "01TESTSPACETEAM2AAAAAAAAAA", tenantB, "team", "又一个团队", nil)
		if err == nil {
			t.Fatal("同一租户插入第二个 team 空间竟然成功了——uk_spaces_team 没起作用")
		}
		if !isDuplicateKey(err) {
			t.Fatalf("期望唯一键冲突，实际: %v", err)
		}
	})

	// 这条是前两条的边界：约束必须只作用于 personal/team，
	// 否则「一个人建两个项目空间」会被误杀。
	t.Run("同一用户可以有多个 project 空间", func(t *testing.T) {
		if _, err := tx.ExecContext(ctx, insert, "01TESTSPACEPROJECT1AAAAAAA", tenantA, "project", "项目一", userA); err != nil {
			t.Fatalf("第一个 project 空间应当插入成功: %v", err)
		}
		if _, err := tx.ExecContext(ctx, insert, "01TESTSPACEPROJECT2AAAAAAA", tenantA, "project", "项目二", userA); err != nil {
			t.Fatalf("同一用户的第二个 project 空间被误杀了: %v", err)
		}
	})
}

func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Duplicate entry")
}
