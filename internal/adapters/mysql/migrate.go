// Package mysql 是 MySQL 适配器。
//
// SQL 只允许出现在本包（docs/05-code-structure.md §3.3）。
package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
)

// migrateLockName 是迁移期间持有的 MySQL 咨询锁。
// 多实例部署时，所有实例启动即尝试迁移是常态；没有这把锁，
// 两个实例会同时对同一张表执行 DDL。
const migrateLockName = "ccs:schema_migrate"

var reMigrationName = regexp.MustCompile(`^(\d{4})_[\w-]+\.sql$`)

// Migrator 顺序应用 migrations/ 下的 SQL 文件，并在 schema_migrations 中记账。
//
// 关于事务：MySQL 的 DDL 会隐式提交，把整个迁移包进事务是**做不到**的假象。
// 因此这里的保证是「顺序执行 + 精确记账」：任一语句失败即中止，
// 已成功的文件已经落账，报错信息给出停在哪个文件的第几条语句，
// 人工修复后重跑会从下一个文件继续。不假装有原子性。
type Migrator struct {
	db     *sql.DB
	fsys   fs.FS
	logger *slog.Logger
}

func NewMigrator(db *sql.DB, fsys fs.FS, logger *slog.Logger) *Migrator {
	return &Migrator{db: db, fsys: fsys, logger: logger}
}

type migration struct {
	version  string // "0001"
	name     string // "0001_governance.sql"
	body     string
	checksum string // sha256 hex
}

// SchemaStatus 是当前二进制内置迁移与数据库账本的一致性摘要。
// 它只包含可安全写进启动日志或健康探针的数量与版本，不暴露 SQL 或连接信息。
type SchemaStatus struct {
	LatestVersion string
	Expected      int
	Applied       int
}

// SchemaPlan is a read-only upgrade assessment. Pending migrations mean the
// current strict-schema binary cannot be rolled back by swapping only the
// executable after those migrations are applied.
type SchemaPlan struct {
	CurrentVersion string
	TargetVersion  string
	NextMigration  string
	Expected       int
	Applied        int
	Pending        int
}

// Run 应用所有尚未应用的迁移。已是最新时为 no-op。
func (m *Migrator) Run(ctx context.Context) error {
	all, err := m.load()
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return errors.New("没有找到任何迁移文件")
	}

	unlock, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	if err := m.ensureLedger(ctx); err != nil {
		return err
	}

	applied, err := m.appliedChecksums(ctx)
	if err != nil {
		return err
	}
	if _, err := planMigrationSets(all, applied); err != nil {
		return err
	}

	pending := 0
	for _, mig := range all {
		_, done := applied[mig.version]
		if done {
			continue
		}
		if err := m.apply(ctx, mig); err != nil {
			return err
		}
		pending++
	}

	if pending == 0 {
		m.logger.Info("数据库结构已是最新", "migrations", len(all))
	} else {
		m.logger.Info("迁移完成", "applied", pending, "total", len(all))
	}
	return nil
}

// Plan validates every already-applied checksum and returns the pending suffix
// without creating the ledger or executing SQL. It is safe to run against the
// live database before an operator schedules an upgrade maintenance window.
func (m *Migrator) Plan(ctx context.Context) (SchemaPlan, error) {
	all, err := m.load()
	if err != nil {
		return SchemaPlan{}, err
	}
	if len(all) == 0 {
		return SchemaPlan{}, errors.New("没有找到任何迁移文件")
	}
	applied, err := m.appliedChecksums(ctx)
	if err != nil {
		return SchemaPlan{}, fmt.Errorf("读取迁移状态: %w", err)
	}
	return planMigrationSets(all, applied)
}

// Check 验证数据库结构与当前二进制完全一致，但不创建表、不执行迁移。
//
// 服务启动与 readiness 必须使用这条只读路径。迁移只能由显式的 -migrate
// 执行；否则滚动发布时，一个普通实例可能在接流量前悄悄改动共享结构。
func (m *Migrator) Check(ctx context.Context) (SchemaStatus, error) {
	all, err := m.load()
	if err != nil {
		return SchemaStatus{}, err
	}
	if len(all) == 0 {
		return SchemaStatus{}, errors.New("没有找到任何迁移文件")
	}

	applied, err := m.appliedChecksums(ctx)
	if err != nil {
		return SchemaStatus{}, fmt.Errorf("读取迁移状态: %w", err)
	}
	return compareMigrationSets(all, applied)
}

func compareMigrationSets(all []migration, applied map[string]string) (SchemaStatus, error) {
	plan, err := planMigrationSets(all, applied)
	status := SchemaStatus{LatestVersion: all[len(all)-1].version, Expected: len(all), Applied: len(applied)}
	if err != nil {
		return status, err
	}
	if plan.Pending > 0 {
		return status, fmt.Errorf("数据库迁移未完成：缺少 %s", plan.NextMigration)
	}
	return status, nil
}

func planMigrationSets(all []migration, applied map[string]string) (SchemaPlan, error) {
	plan := SchemaPlan{TargetVersion: all[len(all)-1].version, Expected: len(all), Applied: len(applied)}
	if err := rejectUnknownMigrationVersions(all, applied); err != nil {
		return plan, err
	}
	seenPending := false
	for _, mig := range all {
		checksum, ok := applied[mig.version]
		if !ok {
			if plan.NextMigration == "" {
				plan.NextMigration = mig.name
			}
			plan.Pending++
			seenPending = true
			continue
		}
		if seenPending {
			return plan, fmt.Errorf("数据库迁移账本不连续：%s 已应用，但更早迁移 %s 缺失；拒绝自动补洞", mig.name, plan.NextMigration)
		}
		if checksum != mig.checksum {
			return plan, fmt.Errorf(
				"数据库迁移不兼容：%s 校验和不一致（库中 %s，二进制 %s）；已发布迁移不可修改",
				mig.name, short(checksum), short(mig.checksum))
		}
		plan.CurrentVersion = mig.version
	}
	return plan, nil
}

func rejectUnknownMigrationVersions(all []migration, applied map[string]string) error {
	expected := make(map[string]struct{}, len(all))
	for _, mig := range all {
		expected[mig.version] = struct{}{}
	}
	for version := range applied {
		if _, ok := expected[version]; !ok {
			return fmt.Errorf("数据库迁移版本 %s 不在当前二进制中；不能用旧二进制执行迁移或接流量", version)
		}
	}
	return nil
}

func (m *Migrator) load() ([]migration, error) {
	entries, err := fs.ReadDir(m.fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("读取迁移目录: %w", err)
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		match := reMigrationName.FindStringSubmatch(e.Name())
		if match == nil {
			return nil, fmt.Errorf("迁移文件命名不符合规范: %s（应为 NNNN_描述.sql）", e.Name())
		}
		body, err := fs.ReadFile(m.fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("读取 %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version:  match[1],
			name:     e.Name(),
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// lock 获取跨实例的迁移互斥锁。返回的函数负责释放。
func (m *Migrator) lock(ctx context.Context) (func(), error) {
	var got sql.NullInt64
	// GET_LOCK 超时 30s：正常迁移远快于此，等不到说明另一实例正在迁移
	if err := m.db.QueryRowContext(ctx, "SELECT GET_LOCK(?, 30)", migrateLockName).Scan(&got); err != nil {
		return nil, fmt.Errorf("获取迁移锁失败: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		return nil, errors.New("获取迁移锁超时：可能有另一个实例正在迁移")
	}
	return func() {
		// 用独立 context：调用方的 ctx 可能已因失败而取消，锁仍必须释放
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := m.db.ExecContext(relCtx, "SELECT RELEASE_LOCK(?)", migrateLockName); err != nil {
			m.logger.Warn("释放迁移锁失败（连接断开时会自动释放）", "error", err)
		}
	}, nil
}

// ensureLedger 建记账表。它自身遵守与业务表相同的规则：
// DATETIME(3) 存 UTC、ID 类列 ascii_bin、无触发器。
func (m *Migrator) ensureLedger(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version     CHAR(4)      CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  name        VARCHAR(128) NOT NULL,
  checksum    CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  duration_ms INT          NOT NULL,
  created_at  DATETIME(3)  NOT NULL,
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	if _, err := m.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("创建 schema_migrations: %w", err)
	}
	return nil
}

func (m *Migrator) appliedChecksums(ctx context.Context) (map[string]string, error) {
	rows, err := m.db.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("查询已应用迁移: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, err
		}
		out[version] = checksum
	}
	return out, rows.Err()
}

func (m *Migrator) apply(ctx context.Context, mig migration) error {
	stmts := splitStatements(mig.body)
	if len(stmts) == 0 {
		return fmt.Errorf("迁移 %s 不含任何语句", mig.name)
	}

	m.logger.Info("应用迁移", "file", mig.name, "statements", len(stmts))
	start := time.Now()

	for i, stmt := range stmts {
		if _, err := m.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf(
				"迁移 %s 第 %d/%d 条语句失败: %w\n  语句: %s\n"+
					"  注意: MySQL 的 DDL 隐式提交，本文件在此之前的语句已生效且未记账。"+
					"修复后需人工确认库状态再重跑",
				mig.name, i+1, len(stmts), err, firstLine(stmt))
		}
	}

	elapsed := time.Since(start)
	_, err := m.db.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, checksum, duration_ms, created_at) VALUES (?, ?, ?, ?, UTC_TIMESTAMP(3))",
		mig.version, mig.name, mig.checksum, elapsed.Milliseconds())
	if err != nil {
		return fmt.Errorf("迁移 %s 已执行但记账失败: %w（重跑前需人工清理）", mig.name, err)
	}

	m.logger.Info("迁移已应用", "file", mig.name, "duration", elapsed)
	return nil
}

// splitStatements 按分号拆分 SQL，跳过注释与字符串字面量中的分号。
//
// 不用 strings.Split(";")：迁移里出现带分号的字符串字面量或注释是迟早的事，
// 那时的报错会是「语法错误」而非「拆分错了」，排查代价极高。
func splitStatements(sql string) []string {
	var (
		out     []string
		cur     strings.Builder
		quote   byte // 当前所处的引号类型，0 表示不在字符串内
		inLine  bool // -- 或 # 行注释
		inBlock bool // /* */ 块注释
	)

	for i := 0; i < len(sql); i++ {
		c := sql[i]

		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				cur.WriteByte(c)
			}
			continue

		case inBlock:
			if c == '*' && i+1 < len(sql) && sql[i+1] == '/' {
				inBlock = false
				i++
			}
			continue

		case quote != 0:
			cur.WriteByte(c)
			if c == '\\' && i+1 < len(sql) && quote != '`' {
				// 反斜杠转义（MySQL 默认开启，反引号内不适用）
				i++
				cur.WriteByte(sql[i])
				continue
			}
			if c == quote {
				// '' 是引号内表示引号自身的写法，不算闭合
				if i+1 < len(sql) && sql[i+1] == quote {
					i++
					cur.WriteByte(sql[i])
					continue
				}
				quote = 0
			}
			continue
		}

		switch {
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			inLine = true
			i++
		case c == '#':
			inLine = true
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			inBlock = true
			i++
		case c == '\'' || c == '"' || c == '`':
			quote = c
			cur.WriteByte(c)
		case c == ';':
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}

	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx]) + " …"
	}
	return strings.TrimSpace(s)
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}
