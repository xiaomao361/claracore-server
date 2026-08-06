// Package migrations 把 SQL 迁移文件编进二进制。
//
// 编进去而不是运行时读目录：部署只有一个文件，不存在「二进制和 migrations/
// 版本对不上」这种排查起来最费时间的故障。
package migrations

import "embed"

// FS 持有全部迁移文件，命名规范 NNNN_描述.sql（由 test/schema_test.go 强制）。
//
//go:embed *.sql
var FS embed.FS
