package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	// 驱动注册。DSN 由 config.MySQL.DSN() 构造，那里已锁定 UTC。
	_ "github.com/go-sql-driver/mysql"
)

// PoolConfig 是连接池参数。取值来自 config，但本包不 import config——
// 适配器不该认识配置文件的形状，装配在 cmd/server 完成。
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// Open 建立连接池并立即验证连通性。
//
// sql.Open 本身是惰性的，不连库也不报错——把连不上推迟到第一次查询，
// 意味着配置错误会伪装成业务故障。这里主动 Ping 一次，让它在启动时就炸。
func Open(ctx context.Context, dsn string, pool PoolConfig) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("构造连接池: %w", err)
	}

	db.SetMaxOpenConns(pool.MaxOpenConns)
	db.SetMaxIdleConns(pool.MaxIdleConns)
	db.SetConnMaxLifetime(pool.ConnMaxLifetime)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 MySQL 失败: %w", err)
	}
	return db, nil
}
