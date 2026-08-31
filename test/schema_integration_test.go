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
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/config"
	"claracore-server/migrations"
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

// TestDatabaseMigrationsMatchCurrentBinary 是 readiness 的真实数据库证据：
// 迁移数量相同还不够，每个已应用版本的 checksum 也必须与本次二进制一致，
// 并且数据库不能含有当前二进制不认识的更新版本。
func TestDatabaseMigrationsMatchCurrentBinary(t *testing.T) {
	db := openDB(t)
	migrator := mysql.NewMigrator(db, migrations.FS, slog.New(slog.NewTextHandler(io.Discard, nil)))
	status, err := migrator.Check(context.Background())
	if err != nil {
		t.Fatalf("真实数据库不能接收当前二进制流量: %v", err)
	}
	if status.Expected == 0 || status.Expected != status.Applied || status.LatestVersion == "" {
		t.Fatalf("迁移状态摘要不完整: %+v", status)
	}
	t.Logf("数据库迁移与当前二进制一致：version=%s applied=%d", status.LatestVersion, status.Applied)
}

func TestUpgradePlanDetectsPendingMigrationAgainstRealDatabaseWithoutWriting(t *testing.T) {
	db := openDB(t)
	files := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = &fstest.MapFile{Data: body}
	}
	files["0014_upgrade_plan_probe.sql"] = &fstest.MapFile{Data: []byte("ALTER TABLE tenants ADD COLUMN upgrade_plan_probe INT NULL;\n")}

	plan, err := mysql.NewMigrator(db, files, slog.New(slog.NewTextHandler(io.Discard, nil))).Plan(context.Background())
	if err != nil {
		t.Fatalf("read-only upgrade plan against real database failed: %v", err)
	}
	if plan.CurrentVersion != "0013" || plan.TargetVersion != "0014" || plan.Pending != 1 ||
		plan.NextMigration != "0014_upgrade_plan_probe.sql" || plan.Applied != 13 || plan.Expected != 14 {
		t.Fatalf("unexpected real-database upgrade plan: %+v", plan)
	}

	var probeColumnCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'tenants' AND COLUMN_NAME = 'upgrade_plan_probe'`).Scan(&probeColumnCount); err != nil {
		t.Fatal(err)
	}
	if probeColumnCount != 0 {
		t.Fatal("upgrade planning executed the synthetic migration")
	}
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

func TestWebCredentialGenerationColumnsAreSafe(t *testing.T) {
	db := openDB(t)

	rows, err := db.Query(`
		SELECT TABLE_NAME, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()
		  AND COLUMN_NAME = 'auth_version'
		  AND TABLE_NAME IN ('users', 'sessions')
		ORDER BY TABLE_NAME`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var table, columnType, nullable string
		var defaultValue sql.NullString
		if err := rows.Scan(&table, &columnType, &nullable, &defaultValue); err != nil {
			t.Fatal(err)
		}
		seen[table] = true
		if columnType != "bigint unsigned" || nullable != "NO" || !defaultValue.Valid || defaultValue.String != "1" {
			t.Errorf("%s.auth_version has unsafe shape: type=%q nullable=%q default=%q", table, columnType, nullable, defaultValue.String)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !seen["users"] || !seen["sessions"] || len(seen) != 2 {
		t.Fatalf("auth_version columns missing: %+v", seen)
	}
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

// TestFullTextIndexesUseNgram 验证库里**实际生效的**全文索引都带 ngram 分词器。
//
// test/schema_test.go 的规则 7 是对迁移文件的文本检查，挡得住新写的迁移，
// 挡不住「这个环境的库是从别处恢复的」或「有人手工改过索引」。而这条失效时
// 不报错——索引建得好好的，MATCH 也能跑，只是中文永远返回空。
//
// 0003 给 card_recall_triggers 补的那个索引尤其要盯：它是 card_recall
// 在没有 embedding 时唯一的降级路径。
func TestFullTextIndexesUseNgram(t *testing.T) {
	db := openDB(t)

	// 读 SHOW CREATE TABLE 而不是 information_schema.STATISTICS.COMMENT：
	// 后者在 8.0.36 上是空串（第一版这条检查就写错在这里，跑出来三张表
	// 全报"没用 ngram"，而库里其实是对的）。解析器名只出现在建表语句的
	// /*!50100 WITH PARSER `ngram` */ 注释里，那是权威来源。
	want := map[string]string{
		"memories":             "ft_memories",
		"cards":                "ft_cards",
		"card_recall_triggers": "ft_triggers", // 0003 补的，card_recall 的降级路径
	}
	for table, index := range want {
		var name, ddl string
		if err := db.QueryRow("SHOW CREATE TABLE "+table).Scan(&name, &ddl); err != nil {
			t.Fatalf("读不到 %s 的建表语句，先执行 make migrate: %v", table, err)
		}
		if !strings.Contains(ddl, "FULLTEXT KEY `"+index+"`") {
			t.Errorf("%s 上没有全文索引 %s——检索的第一段粗筛会退化成全表扫", table, index)
			continue
		}
		if !strings.Contains(strings.ToLower(ddl), "with parser `ngram`") {
			t.Errorf("%s.%s 没有使用 ngram 分词器\n  原因: 默认分词器按空格切词，中文整段被当作一个词——不报错，只是永远返回空\n  DDL: %s",
				table, index, ddl)
		}
	}
}

// TestEmbeddingsTableShape 盯住 0003 落地时改掉草案的两处。
//
// 它们都不是"写得好看一点"，而是设计成立的前提：vector 可空让这张表
// 同时是索引队列（写入不依赖外部服务），uk_emb 让同一实体在同一模型下
// 只有一行（换模型是新增行、可灰度重建）。
func TestEmbeddingsTableShape(t *testing.T) {
	db := openDB(t)

	var nullable string
	err := db.QueryRow(`
		SELECT IS_NULLABLE FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'embeddings' AND COLUMN_NAME = 'vector'`).Scan(&nullable)
	if err != nil {
		t.Fatalf("embeddings 表不存在或缺少 vector 列，先执行 make migrate: %v", err)
	}
	if nullable != "YES" {
		t.Error("vector 必须可空：这张表同时是索引队列，NOT NULL 会逼我们用空 BLOB 冒充「还没算」")
	}

	var cols int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'embeddings'
		  AND INDEX_NAME = 'uk_emb' AND NON_UNIQUE = 0`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 3 {
		t.Errorf("uk_emb 应是 (entity_type, entity_id, model) 三列唯一键，实际 %d 列\n  它保证同一实体在同一模型下只有一行，换模型时新旧向量共存", cols)
	}
}

func TestExternalMirrorSchemaStoresNoAuthoritativeBody(t *testing.T) {
	db := openDB(t)
	for _, table := range []string{"external_sources", "external_sync_runs", "external_documents", "external_document_vectors"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("external mirror table %s missing: count=%d err=%v", table, count, err)
		}
	}
	rows, err := db.Query(`SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN ('external_documents', 'external_document_vectors')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	forbidden := map[string]bool{"body": true, "content": true, "source_text": true, "raw_json": true, "metadata": true}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		if forbidden[column] {
			t.Errorf("external mirror must not persist authoritative body column %s", column)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var nullable string
	if err := db.QueryRow(`SELECT IS_NULLABLE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'external_document_vectors' AND COLUMN_NAME = 'vector'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" {
		t.Fatal("external metadata and vector must publish atomically; vector cannot be a pending NULL placeholder")
	}
}
