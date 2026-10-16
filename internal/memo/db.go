// Package memo 是团队 agent 的记忆与知识库：一张 entries 表，按调用者归属，可选共享。
//
// 只有一个实现（SQLite），所以不抽 Store 接口；测试直接用临时文件里的真库。
package memo

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// schemaVersion 记在 PRAGMA user_version。新基线从 1 开始，与旧 MySQL 迁移不兼容。
const schemaVersion = 1

const schema = `
CREATE TABLE entries (
  id               TEXT PRIMARY KEY,
  kind             TEXT NOT NULL CHECK (kind IN ('memory', 'knowledge')),
  owner            TEXT NOT NULL,
  shared           INTEGER NOT NULL DEFAULT 0 CHECK (shared IN (0, 1)),
  title            TEXT NOT NULL,
  body             TEXT NOT NULL,
  tags             TEXT NOT NULL DEFAULT '',
  source           TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'superseded')),
  superseded_by    TEXT REFERENCES entries(id),
  supersede_reason TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL,
  embed_model      TEXT,
  embedding        BLOB,
  embed_error      TEXT
);
CREATE INDEX entries_owner  ON entries(owner, status, created_at);
CREATE INDEX entries_shared ON entries(shared, status, created_at);
`

// Open 打开（必要时初始化）数据库。
//
// 单连接：SQLite 的写本来就串行，多连接只会换来 SQLITE_BUSY。
// 这个服务的量级（一个团队的 agent）用不着读写分离。
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("读取 schema 版本: %w", err)
	}
	switch v {
	case schemaVersion:
		return nil
	case 0:
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(schema); err != nil {
			return fmt.Errorf("建表: %w", err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return err
		}
		return tx.Commit()
	default:
		// 库比程序新：拒绝启动，而不是拿旧代码去写新结构。
		return fmt.Errorf("数据库 schema 版本 %d 高于程序支持的 %d", v, schemaVersion)
	}
}
