package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	// 别名导入：本包也叫 mysql。这里只用它的 MySQLError 类型判错误码。
	driver "github.com/go-sql-driver/mysql"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/platform"
)

// erDupEntry 是 MySQL 的唯一键冲突错误码（ER_DUP_ENTRY）。
//
// 用错误码而非匹配 "Duplicate entry" 字符串：后者会随服务端语言设置变化，
// 一台装了中文错误消息的库就能让这个判断静默失效。
const erDupEntry = 1062

// GovernanceStore 是 governance.Store 的 MySQL 实现。
//
// 手写 database/sql，不用 ORM（docs/05-code-structure.md §3.3、§7）。
//
// 本文件的一条纪律：**SQL 只负责取行与写行，不做任何权限或状态判断**。
// 看不到 `WHERE revoked_at IS NULL`、看不到 `AND can_read = 1`——
// 那些判断全在领域层，只此一份。一旦规则同时活在 SQL 和 Go 里，
// 改一处必忘另一处，而权限规则忘改的后果是越权。
type GovernanceStore struct {
	// db 用于开启事务。在事务内派生出的实例中为 nil，
	// 以此区分「能开事务」与「已在事务里」两种状态。
	db *sql.DB
	// ex 是实际执行者：*sql.DB 或 *sql.Tx。
	ex sqlExecutor
}

// sqlExecutor 抹平 *sql.DB 与 *sql.Tx 的差异，让每个方法只写一遍。
type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// 编译期确认实现完整：接口新增方法时在这里报错，而不是在装配处。
var _ governance.Store = (*GovernanceStore)(nil)

func NewGovernanceStore(db *sql.DB) *GovernanceStore {
	return &GovernanceStore{db: db, ex: db}
}

// --- 认证路径 ---

// findAuthContextSQL 一次往返取回 token + user + agent + tenant。
//
// 认证发生在每一次 MCP 调用上：拆成四次 Find 就是四次往返 × 每次工具调用，
// 是这套系统最容易被忽视的延迟来源。
//
// 两个刻意的选择：
//
//	LEFT JOIN —— 库里没有外键（引用完整性由领域层保证）。用 INNER JOIN 时，
//	             一条指向已删除用户的 token 会表现为「token 不存在」，
//	             把一次数据损坏伪装成一次普通的认证失败。
//	ON 只比主键 —— 不在 ON 里加 tenant_id 相等条件。加了就等于让 SQL 悄悄
//	             过滤掉跨租户的错误数据，而 checkAuthConsistency 正是为了
//	             发现这种数据损坏才存在的；SQL 替它把活干了，它就永远不会响。
const findAuthContextSQL = `
SELECT
    t.id, t.tenant_id, t.user_id, t.agent_id, t.name, t.token_prefix, t.token_hash,
    t.expires_at, t.last_used_at, t.revoked_at, t.created_at,
    u.id, u.tenant_id, u.username, u.password_hash, u.display_name, u.role, u.must_change_pw, u.status, u.created_at,
    a.id, a.tenant_id, a.user_id, a.slug, a.display_name, a.created_at,
    n.id, n.name, n.status, n.created_at
FROM tokens t
LEFT JOIN users   u ON u.id = t.user_id
LEFT JOIN agents  a ON a.id = t.agent_id
LEFT JOIN tenants n ON n.id = t.tenant_id
WHERE t.token_hash = ?`

func (s *GovernanceStore) FindAuthContext(ctx context.Context, tokenHash string) (*governance.AuthContext, error) {
	var (
		tokID, tokTenant, tokUser, tokAgent string
		tokName, tokPrefix, tokHash         string
		expiresAt, lastUsedAt, revokedAt    sql.NullTime
		tokCreated                          time.Time

		uID, uTenant, uUsername, uHash, uDisplay, uRole, uStatus sql.NullString
		uMustChange                                              sql.NullBool
		uCreated                                                 sql.NullTime

		aID, aTenant, aUser, aSlug, aDisplay sql.NullString
		aCreated                             sql.NullTime

		nID, nName, nStatus sql.NullString
		nCreated            sql.NullTime
	)

	err := s.ex.QueryRowContext(ctx, findAuthContextSQL, tokenHash).Scan(
		&tokID, &tokTenant, &tokUser, &tokAgent, &tokName, &tokPrefix, &tokHash,
		&expiresAt, &lastUsedAt, &revokedAt, &tokCreated,
		&uID, &uTenant, &uUsername, &uHash, &uDisplay, &uRole, &uMustChange, &uStatus, &uCreated,
		&aID, &aTenant, &aUser, &aSlug, &aDisplay, &aCreated,
		&nID, &nName, &nStatus, &nCreated,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, governance.ErrTokenNotFound
	case err != nil:
		return nil, fmt.Errorf("mysql: 查询认证上下文: %w", err)
	}

	// 三个「行不存在」分支都是数据损坏，不是正常业务状态。
	// Host 层会把它们与 ErrTokenNotFound 映射为同一种响应（区分等于泄露信息），
	// 但服务端日志里能看出到底缺了什么。
	if !uID.Valid {
		return nil, governance.ErrUserNotFound
	}
	if !aID.Valid {
		return nil, governance.ErrAgentNotFound
	}
	if !nID.Valid {
		return nil, governance.ErrTenantInactive
	}

	return &governance.AuthContext{
		Token: governance.Token{
			ID:         platform.ID(tokID),
			TenantID:   platform.ID(tokTenant),
			UserID:     platform.ID(tokUser),
			AgentID:    platform.ID(tokAgent),
			Name:       tokName,
			Prefix:     tokPrefix,
			Hash:       tokHash,
			ExpiresAt:  timePtr(expiresAt),
			LastUsedAt: timePtr(lastUsedAt),
			RevokedAt:  timePtr(revokedAt),
			CreatedAt:  tokCreated.UTC(),
		},
		User: governance.User{
			ID:           platform.ID(uID.String),
			TenantID:     platform.ID(uTenant.String),
			Username:     uUsername.String,
			PasswordHash: uHash.String,
			DisplayName:  uDisplay.String,
			Role:         uRole.String,
			MustChangePw: uMustChange.Bool,
			Status:       uStatus.String,
			CreatedAt:    uCreated.Time.UTC(),
		},
		Agent: governance.Agent{
			ID:          platform.ID(aID.String),
			TenantID:    platform.ID(aTenant.String),
			UserID:      platform.ID(aUser.String),
			Slug:        aSlug.String,
			DisplayName: aDisplay.String,
			CreatedAt:   aCreated.Time.UTC(),
		},
		Tenant: governance.Tenant{
			ID:        platform.ID(nID.String),
			Name:      nName.String,
			Status:    nStatus.String,
			CreatedAt: nCreated.Time.UTC(),
		},
	}, nil
}

// TouchToken 更新 last_used_at。
//
// 写同值时 MySQL 报告的 affected rows 是 0（驱动未开 clientFoundRows），
// 所以 0 行不能直接判定为「token 不存在」——那会让节流窗口边界上的一次
// 重复写变成一个假的 ErrTokenNotFound。多一次存在性查询，只发生在 0 行时。
func (s *GovernanceStore) TouchToken(ctx context.Context, tokenID platform.ID, at time.Time) error {
	res, err := s.ex.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE id = ?`, at.UTC(), string(tokenID))
	if err != nil {
		return fmt.Errorf("mysql: 更新 last_used_at: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}

	var exists int
	err = s.ex.QueryRowContext(ctx, `SELECT 1 FROM tokens WHERE id = ?`, string(tokenID)).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return governance.ErrTokenNotFound
	}
	if err != nil {
		return fmt.Errorf("mysql: 确认 token 是否存在: %w", err)
	}
	return nil
}

// listMembershipsSQL 捞出「与这个用户有关」的空间及其显式授权行。
//
// WHERE 里的三个条件是**相关性**筛选，不是权限判断——分界线在于：
// 这里只看空间的 type / owner 和「有没有授权行」，绝不看 can_read / can_write
// 的取值。是否可读可写由 governance.ResolveScope 决定，规则只有一份。
//
// 三条规则的形态不同（personal 隐含、team 全租户可读、project 完全显式），
// 想在 SQL 里表达它们必然要写成 `OR type = 'team' OR can_read = 1` 这种形状，
// 那一刻权限规则就有了第二个副本。
const listMembershipsSQL = `
SELECT s.id, s.tenant_id, s.type, s.name, s.owner_user_id, s.created_at,
       p.can_read, p.can_write, p.granted_by, p.created_at
FROM spaces s
LEFT JOIN space_permissions p ON p.space_id = s.id AND p.user_id = ?
WHERE s.tenant_id = ?
  AND (s.type = ? OR s.owner_user_id = ? OR p.space_id IS NOT NULL)
ORDER BY s.id`

func (s *GovernanceStore) ListSpaceMemberships(ctx context.Context, tenantID, userID platform.ID) ([]governance.SpaceMembership, error) {
	rows, err := s.ex.QueryContext(ctx, listMembershipsSQL,
		string(userID), string(tenantID), governance.SpaceTeam, string(userID))
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询空间成员关系: %w", err)
	}
	defer rows.Close()

	var out []governance.SpaceMembership
	for rows.Next() {
		var (
			space     governance.Space
			id, tid   string
			owner     sql.NullString
			created   time.Time
			canRead   sql.NullBool
			canWrite  sql.NullBool
			grantedBy sql.NullString
			permAt    sql.NullTime
		)
		if err := rows.Scan(&id, &tid, &space.Type, &space.Name, &owner, &created,
			&canRead, &canWrite, &grantedBy, &permAt); err != nil {
			return nil, fmt.Errorf("mysql: 读取空间行: %w", err)
		}
		space.ID = platform.ID(id)
		space.TenantID = platform.ID(tid)
		space.CreatedAt = created.UTC()
		if owner.Valid {
			o := platform.ID(owner.String)
			space.OwnerUserID = &o
		}

		m := governance.SpaceMembership{Space: space}
		// grantedBy 非 NULL 即代表 LEFT JOIN 命中了授权行。
		// 不用 can_read 判断：它取值为 0 也是一行真实存在的授权。
		if grantedBy.Valid {
			m.Permission = &governance.Permission{
				SpaceID:   space.ID,
				UserID:    userID,
				CanRead:   canRead.Bool,
				CanWrite:  canWrite.Bool,
				GrantedBy: platform.ID(grantedBy.String),
				CreatedAt: permAt.Time.UTC(),
			}
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历空间行: %w", err)
	}
	return out, nil
}

// --- 治理面写入 ---

func (s *GovernanceStore) CreateTenant(ctx context.Context, t governance.Tenant) error {
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO tenants (id, name, status, created_at) VALUES (?, ?, ?, ?)`,
		string(t.ID), t.Name, t.Status, t.CreatedAt.UTC())
	return writeErr("创建租户", err)
}

func (s *GovernanceStore) CreateUser(ctx context.Context, u governance.User) error {
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO users (id, tenant_id, username, password_hash, display_name, role, must_change_pw, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(u.ID), string(u.TenantID), u.Username, u.PasswordHash, u.DisplayName,
		u.Role, u.MustChangePw, u.Status, u.CreatedAt.UTC())
	return writeErr("创建用户", err)
}

func (s *GovernanceStore) CreateAgent(ctx context.Context, a governance.Agent) error {
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO agents (id, tenant_id, user_id, slug, display_name, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		string(a.ID), string(a.TenantID), string(a.UserID), a.Slug, a.DisplayName, a.CreatedAt.UTC())
	return writeErr("创建 agent", err)
}

// CreateSpace 插入一个空间。
//
// 「每用户一个 personal / 每租户一个 team」由 uk_spaces_personal 与
// uk_spaces_team 在数据库层拦截，这里不做 SELECT-then-INSERT 预检——
// 预检在并发下必然失效，而唯一键不会。
func (s *GovernanceStore) CreateSpace(ctx context.Context, sp governance.Space) error {
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO spaces (id, tenant_id, type, name, owner_user_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		string(sp.ID), string(sp.TenantID), sp.Type, sp.Name, idPtrArg(sp.OwnerUserID), sp.CreatedAt.UTC())
	return writeErr("创建空间", err)
}

func (s *GovernanceStore) CreateToken(ctx context.Context, t governance.Token) error {
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO tokens (id, tenant_id, user_id, agent_id, name, token_prefix, token_hash,
		                     expires_at, last_used_at, revoked_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(t.ID), string(t.TenantID), string(t.UserID), string(t.AgentID),
		t.Name, t.Prefix, t.Hash,
		timeArg(t.ExpiresAt), timeArg(t.LastUsedAt), timeArg(t.RevokedAt), t.CreatedAt.UTC())
	return writeErr("创建 token", err)
}

// --- 治理面查询 ---

// userColumns 只写一遍。列顺序散在两处 Scan 里，迟早有一处跟不上改动。
const userColumns = `id, tenant_id, username, password_hash, display_name, role, must_change_pw, status, created_at`

// scanRow 抹平 *sql.Row 与 *sql.Rows 的差异，让单行查询与列表查询共用一个 scan。
type scanRow interface{ Scan(dest ...any) error }

func scanUserFrom(row scanRow) (governance.User, error) {
	var (
		u       governance.User
		id, tid string
		created time.Time
	)
	err := row.Scan(&id, &tid, &u.Username, &u.PasswordHash, &u.DisplayName,
		&u.Role, &u.MustChangePw, &u.Status, &created)
	if err != nil {
		return governance.User{}, err
	}
	u.ID, u.TenantID, u.CreatedAt = platform.ID(id), platform.ID(tid), created.UTC()
	return u, nil
}

func scanUser(rows *sql.Rows) (governance.User, error) {
	u, err := scanUserFrom(rows)
	if err != nil {
		return governance.User{}, fmt.Errorf("mysql: 读取用户行: %w", err)
	}
	return u, nil
}

func (s *GovernanceStore) findUser(ctx context.Context, query string, args ...any) (*governance.User, error) {
	u, err := scanUserFrom(s.ex.QueryRowContext(ctx, query, args...))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, governance.ErrUserNotFound
	case err != nil:
		return nil, fmt.Errorf("mysql: 查询用户: %w", err)
	}
	return &u, nil
}

func (s *GovernanceStore) FindUserByUsername(ctx context.Context, tenantID platform.ID, username string) (*governance.User, error) {
	return s.findUser(ctx,
		`SELECT `+userColumns+` FROM users WHERE tenant_id = ? AND username = ?`,
		string(tenantID), username)
}

// FindSpace 按 ID 取空间，带租户过滤。
//
// tenant_id 是隔离条件不是权限判断：跨租户的空间对本租户等同于不存在，
// 所以返回 ErrSpaceNotFound 而不是 ErrSpaceForbidden——后者会泄露
// 「这个 ID 在别的租户下存在」。
func (s *GovernanceStore) FindSpace(ctx context.Context, tenantID, spaceID platform.ID) (*governance.Space, error) {
	var (
		sp      governance.Space
		id, tid string
		owner   sql.NullString
		created time.Time
	)
	err := s.ex.QueryRowContext(ctx,
		`SELECT id, tenant_id, type, name, owner_user_id, created_at
		 FROM spaces WHERE id = ? AND tenant_id = ?`,
		string(spaceID), string(tenantID),
	).Scan(&id, &tid, &sp.Type, &sp.Name, &owner, &created)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, governance.ErrSpaceNotFound
	case err != nil:
		return nil, fmt.Errorf("mysql: 查询空间: %w", err)
	}
	sp.ID = platform.ID(id)
	sp.TenantID = platform.ID(tid)
	sp.CreatedAt = created.UTC()
	if owner.Valid {
		o := platform.ID(owner.String)
		sp.OwnerUserID = &o
	}
	return &sp, nil
}

// ListTokens 返回该用户的全部 token，含已吊销的。
//
// 不过滤 revoked_at：这个列表是审计视图，「什么时候被吊销的」正是要看的内容。
// 按 id 排序即按签发时间排序（ULID 时间有序）。
func (s *GovernanceStore) ListTokens(ctx context.Context, tenantID, userID platform.ID) ([]governance.Token, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT id, tenant_id, user_id, agent_id, name, token_prefix, token_hash,
		        expires_at, last_used_at, revoked_at, created_at
		 FROM tokens WHERE tenant_id = ? AND user_id = ? ORDER BY id`,
		string(tenantID), string(userID))
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询 token 列表: %w", err)
	}
	defer rows.Close()

	var out []governance.Token
	for rows.Next() {
		var (
			t                                governance.Token
			id, tid, uid, aid                string
			expiresAt, lastUsedAt, revokedAt sql.NullTime
			created                          time.Time
		)
		if err := rows.Scan(&id, &tid, &uid, &aid, &t.Name, &t.Prefix, &t.Hash,
			&expiresAt, &lastUsedAt, &revokedAt, &created); err != nil {
			return nil, fmt.Errorf("mysql: 读取 token 行: %w", err)
		}
		t.ID, t.TenantID, t.UserID, t.AgentID = platform.ID(id), platform.ID(tid), platform.ID(uid), platform.ID(aid)
		t.ExpiresAt, t.LastUsedAt, t.RevokedAt = timePtr(expiresAt), timePtr(lastUsedAt), timePtr(revokedAt)
		t.CreatedAt = created.UTC()
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历 token 行: %w", err)
	}
	return out, nil
}

// RevokeToken 幂等吊销。
//
// `AND revoked_at IS NULL` 在这里是幂等机制而非状态判断：它保证重复吊销
// 不会覆盖第一次的时间戳——那个时间戳是审计证据。
// 0 行有两种含义（不存在 / 已吊销），必须区分，所以再查一次。
func (s *GovernanceStore) RevokeToken(ctx context.Context, tenantID, tokenID platform.ID, at time.Time) error {
	res, err := s.ex.ExecContext(ctx,
		`UPDATE tokens SET revoked_at = ? WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL`,
		at.UTC(), string(tokenID), string(tenantID))
	if err != nil {
		return fmt.Errorf("mysql: 吊销 token: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}

	var exists int
	err = s.ex.QueryRowContext(ctx,
		`SELECT 1 FROM tokens WHERE id = ? AND tenant_id = ?`,
		string(tokenID), string(tenantID)).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return governance.ErrTokenNotFound
	}
	if err != nil {
		return fmt.Errorf("mysql: 确认 token 是否存在: %w", err)
	}
	return nil // 已经吊销过，幂等返回
}

// GrantPermission 插入或覆盖一行授权。
//
// 用 upsert 而非 SELECT-then-INSERT/UPDATE：后者在两个管理员同时改同一人的
// 权限时会一个报主键冲突。created_at 一并覆盖——它记的是「这次授权是什么时候
// 生效的」，重新授权后旧时间已无意义。
func (s *GovernanceStore) GrantPermission(ctx context.Context, p governance.Permission) error {
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO space_permissions (space_id, user_id, can_read, can_write, granted_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?) AS v
		 ON DUPLICATE KEY UPDATE
		   can_read = v.can_read, can_write = v.can_write,
		   granted_by = v.granted_by, created_at = v.created_at`,
		string(p.SpaceID), string(p.UserID), p.CanRead, p.CanWrite, string(p.GrantedBy), p.CreatedAt.UTC())
	return writeErr("授予空间权限", err)
}

// RevokePermission 删除授权行。不存在不是错误——撤销一个本就没有的权限，
// 结果与调用方期望的一致。
func (s *GovernanceStore) RevokePermission(ctx context.Context, spaceID, userID platform.ID) error {
	_, err := s.ex.ExecContext(ctx,
		`DELETE FROM space_permissions WHERE space_id = ? AND user_id = ?`,
		string(spaceID), string(userID))
	if err != nil {
		return fmt.Errorf("mysql: 撤销空间权限: %w", err)
	}
	return nil
}

// Atomically 在一个 SQL 事务内执行 fn。
//
// 已在事务中时直接复用当前事务（MySQL 不支持真正的嵌套事务，
// 用 SAVEPOINT 模拟会让「回滚」在内外层含义不同）。领域层要表达的是
// 「原子地做这几件事」，嵌套调用时这个语义仍然成立。
func (s *GovernanceStore) Atomically(ctx context.Context, fn func(governance.Store) error) error {
	if s.db == nil {
		return fn(s)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysql: 开启事务: %w", err)
	}
	// panic 也要回滚，否则连接会带着未提交的事务被放回池里
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := fn(&GovernanceStore{ex: tx}); err != nil {
		return err // 原样返回，让领域层的哨兵错误穿透
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysql: 提交事务: %w", err)
	}
	committed = true
	return nil
}

// writeErr 把一次写入的失败翻译成上层能判断的错误。
//
// 唯一键冲突映射为 governance.ErrAlreadyExists：Host 层要区分「这个账号已经
// 有人用了」和「库炸了」，而它不能 import 数据库驱动去看错误码——那会撞上
// 「Host 不得直连基础设施」的架构约束（test/arch_test.go）。翻译发生在这里，
// 是适配器的本职：把存储技术的方言换成领域的语言。
func writeErr(what string, err error) error {
	if err == nil {
		return nil
	}
	var me *driver.MySQLError
	if errors.As(err, &me) && me.Number == erDupEntry {
		// 保留原始错误：日志里需要看到是哪个唯一键冲突的
		return fmt.Errorf("%w: %s: %v", governance.ErrAlreadyExists, what, err)
	}
	return fmt.Errorf("mysql: %s: %w", what, err)
}

// --- 类型转换 ---

// timeArg 把可空时间转成 driver 参数：nil 指针写 NULL。
func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func timePtr(n sql.NullTime) *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time.UTC()
	return &t
}

func idPtrArg(id *platform.ID) any {
	if id == nil {
		return nil
	}
	return string(*id)
}

// ---------------------------------------------------------------------------
// 治理面：账号管理（Web 用，低频）
// ---------------------------------------------------------------------------

func (s *GovernanceStore) FindUserByID(ctx context.Context, tenantID, userID platform.ID) (*governance.User, error) {
	return s.findUser(ctx,
		`SELECT `+userColumns+` FROM users WHERE tenant_id = ? AND id = ?`,
		string(tenantID), string(userID))
}

// ListTenants 返回全部租户。
//
// 不加 `WHERE status = 'active'`：领域层要能看见「库里有两个活跃租户」
// 这件事并报错。在 SQL 里过滤掉就等于让一次运维事故被悄悄消化掉，
// 而它的后果是某个人登进了别人的团队。
func (s *GovernanceStore) ListTenants(ctx context.Context) ([]governance.Tenant, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT id, name, status, created_at FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询租户: %w", err)
	}
	defer rows.Close()

	var out []governance.Tenant
	for rows.Next() {
		var (
			t       governance.Tenant
			id      string
			created time.Time
		)
		if err := rows.Scan(&id, &t.Name, &t.Status, &created); err != nil {
			return nil, fmt.Errorf("mysql: 读取租户行: %w", err)
		}
		t.ID, t.CreatedAt = platform.ID(id), created.UTC()
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历租户行: %w", err)
	}
	return out, nil
}

func (s *GovernanceStore) ListUsers(ctx context.Context, tenantID platform.ID) ([]governance.User, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE tenant_id = ? ORDER BY created_at`,
		string(tenantID))
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询用户列表: %w", err)
	}
	defer rows.Close()

	var out []governance.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历用户行: %w", err)
	}
	return out, nil
}

func (s *GovernanceStore) ListSpaces(ctx context.Context, tenantID platform.ID) ([]governance.Space, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT id, tenant_id, type, name, owner_user_id, created_at
		 FROM spaces WHERE tenant_id = ? ORDER BY type, id`, string(tenantID))
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询空间列表: %w", err)
	}
	defer rows.Close()

	var out []governance.Space
	for rows.Next() {
		var sp governance.Space
		var id, tid string
		var owner sql.NullString
		var created time.Time
		if err := rows.Scan(&id, &tid, &sp.Type, &sp.Name, &owner, &created); err != nil {
			return nil, fmt.Errorf("mysql: 读取空间列表: %w", err)
		}
		sp.ID, sp.TenantID, sp.CreatedAt = platform.ID(id), platform.ID(tid), created.UTC()
		if owner.Valid {
			o := platform.ID(owner.String)
			sp.OwnerUserID = &o
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历空间列表: %w", err)
	}
	return out, nil
}

func (s *GovernanceStore) ListPermissions(ctx context.Context, tenantID platform.ID) ([]governance.Permission, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT p.space_id, p.user_id, p.can_read, p.can_write, p.granted_by, p.created_at
		 FROM space_permissions p
		 JOIN spaces s ON s.id = p.space_id
		 JOIN users u ON u.id = p.user_id
		 WHERE s.tenant_id = ? AND u.tenant_id = ?
		 ORDER BY p.space_id, p.user_id`, string(tenantID), string(tenantID))
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询空间授权: %w", err)
	}
	defer rows.Close()

	var out []governance.Permission
	for rows.Next() {
		var p governance.Permission
		var spaceID, userID, grantedBy string
		var created time.Time
		if err := rows.Scan(&spaceID, &userID, &p.CanRead, &p.CanWrite, &grantedBy, &created); err != nil {
			return nil, fmt.Errorf("mysql: 读取空间授权: %w", err)
		}
		p.SpaceID, p.UserID = platform.ID(spaceID), platform.ID(userID)
		p.GrantedBy, p.CreatedAt = platform.ID(grantedBy), created.UTC()
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历空间授权: %w", err)
	}
	return out, nil
}

func (s *GovernanceStore) UpdateUserPassword(ctx context.Context, tenantID, userID platform.ID, hash string, mustChange bool, _ time.Time) error {
	res, err := s.ex.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, must_change_pw = ? WHERE tenant_id = ? AND id = ?`,
		hash, mustChange, string(tenantID), string(userID))
	if err != nil {
		return fmt.Errorf("mysql: 更新密码: %w", err)
	}
	return s.confirmUserTouched(ctx, res, tenantID, userID)
}

func (s *GovernanceStore) UpdateUserStatus(ctx context.Context, tenantID, userID platform.ID, status string, _ time.Time) error {
	res, err := s.ex.ExecContext(ctx,
		`UPDATE users SET status = ? WHERE tenant_id = ? AND id = ?`,
		status, string(tenantID), string(userID))
	if err != nil {
		return fmt.Errorf("mysql: 更新账号状态: %w", err)
	}
	return s.confirmUserTouched(ctx, res, tenantID, userID)
}

// confirmUserTouched 把「影响 0 行」翻译成正确的领域错误。
//
// 驱动未开 clientFoundRows，**写同值时 affected rows 是 0**——
// 把一个已停用的账号再停用一次，或者重置成同一个密码（哈希带盐所以
// 这不会发生，但状态更新会），都会走到这里。直接判成「不存在」
// 会造出一个假的 ErrUserNotFound（0001 那一轮在 TouchToken 上踩过同样的坑）。
func (s *GovernanceStore) confirmUserTouched(ctx context.Context, res sql.Result, tenantID, userID platform.ID) error {
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}
	var exists int
	err := s.ex.QueryRowContext(ctx,
		`SELECT 1 FROM users WHERE tenant_id = ? AND id = ?`,
		string(tenantID), string(userID)).Scan(&exists)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return governance.ErrUserNotFound
	case err != nil:
		return fmt.Errorf("mysql: 确认用户是否存在: %w", err)
	}
	return nil
}
