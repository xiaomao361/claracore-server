package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// KnowledgeStore 是 knowledge.Store 的 MySQL 实现。
//
// 与 GovernanceStore 同一套纪律：**SQL 只取行写行，不做权限判断**。
// 这里唯一像"过滤"的东西是 `space_id IN (...)`，而那批空间是领域层
// 从 governance.SpaceScope 解析好交下来的确定值——本文件既不知道
// 它们怎么来的，也不得对它们做任何加工。看到 `OR type = 'team'`
// 这类东西出现在本文件里，说明权限规则已经有了第二个副本。
type KnowledgeStore struct {
	// db 用于开启事务。在事务内派生出的实例中为 nil。
	db *sql.DB
	ex sqlExecutor
}

var _ knowledge.Store = (*KnowledgeStore)(nil)

func NewKnowledgeStore(db *sql.DB) *KnowledgeStore {
	return &KnowledgeStore{db: db, ex: db}
}

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

const memoryColumns = `id, tenant_id, space_id, author_user_id, author_agent_id,
	title, body, status, copied_from_type, copied_from_id, version, created_at, updated_at`

func (s *KnowledgeStore) CreateMemory(ctx context.Context, m knowledge.Memory) error {
	fromType, fromID := originArgs(m.CopiedFrom)
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO memories (`+memoryColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(m.ID), string(m.TenantID), string(m.SpaceID), string(m.AuthorUserID), idPtrArg(m.AuthorAgentID),
		m.Title, m.Body, m.Status, fromType, fromID, m.Version, m.CreatedAt.UTC(), m.UpdatedAt.UTC())
	return knowledgeWriteErr("创建记忆", err)
}

func (s *KnowledgeStore) FindMemory(ctx context.Context, tenantID, id platform.ID, spaces []platform.ID) (*knowledge.Memory, error) {
	if len(spaces) == 0 {
		// 空集合会让 `IN ()` 变成语法错误。领域层已经拦过一道，
		// 这里再兜一次：Store 是公开接口，将来的调用方未必走 Service。
		return nil, knowledge.ErrNotFound
	}

	args := append([]any{string(id), string(tenantID)}, idArgs(spaces)...)
	var (
		m                  knowledge.Memory
		mID, tID, sID, aID string
		agentID            sql.NullString
		fromType, fromID   sql.NullString
		created, updated   time.Time
	)
	err := s.ex.QueryRowContext(ctx,
		`SELECT `+memoryColumns+` FROM memories
		 WHERE id = ? AND tenant_id = ? AND space_id IN (`+placeholders(len(spaces))+`)`,
		args...,
	).Scan(&mID, &tID, &sID, &aID, &agentID, &m.Title, &m.Body, &m.Status,
		&fromType, &fromID, &m.Version, &created, &updated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, knowledge.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("mysql: 查询记忆: %w", err)
	}

	m.ID, m.TenantID, m.SpaceID, m.AuthorUserID = platform.ID(mID), platform.ID(tID), platform.ID(sID), platform.ID(aID)
	m.AuthorAgentID = idFromNull(agentID)
	m.CopiedFrom = originFromNull(fromType, fromID)
	m.CreatedAt, m.UpdatedAt = created.UTC(), updated.UTC()

	labels, err := s.listLabels(ctx, knowledge.Ref{Type: knowledge.EntityMemory, ID: m.ID})
	if err != nil {
		return nil, err
	}
	m.Labels = labels
	return &m, nil
}

func (s *KnowledgeStore) UpdateMemory(ctx context.Context, m knowledge.Memory, expectedVersion int) error {
	fromType, fromID := originArgs(m.CopiedFrom)
	res, err := s.ex.ExecContext(ctx,
		`UPDATE memories SET space_id = ?, title = ?, body = ?, status = ?,
		        copied_from_type = ?, copied_from_id = ?, version = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ? AND version = ?`,
		string(m.SpaceID), m.Title, m.Body, m.Status, fromType, fromID, m.Version, m.UpdatedAt.UTC(),
		string(m.ID), string(m.TenantID), expectedVersion)
	if err != nil {
		return fmt.Errorf("mysql: 更新记忆: %w", err)
	}
	return s.classifyUpdateResult(ctx, res, "memories", m.ID, m.TenantID, expectedVersion)
}

// ---------------------------------------------------------------------------
// Card
// ---------------------------------------------------------------------------

const cardColumns = `id, tenant_id, space_id, author_user_id, author_agent_id,
	title, card_type, status, source, source_name, source_key, source_hash,
	body_original, body_distilled, body_understanding, body_why_keep, body_unresolved,
	copied_from_type, copied_from_id, version, created_at, updated_at`

func (s *KnowledgeStore) CreateCard(ctx context.Context, c knowledge.Card) error {
	fromType, fromID := originArgs(c.CopiedFrom)
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO cards (`+cardColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(c.ID), string(c.TenantID), string(c.SpaceID), string(c.AuthorUserID), idPtrArg(c.AuthorAgentID),
		c.Title, c.CardType, c.Status, nullIfEmpty(c.Source), nullIfEmpty(c.SourceName),
		// source_key 必须写 NULL 而不是空串：uk_cards_source 允许多行 NULL，
		// 而空串会让所有手写卡片在同一个空间里互相冲突。
		nullIfEmpty(c.SourceKey), nullIfEmpty(c.SourceHash),
		nullIfEmpty(c.Original), nullIfEmpty(c.Distilled), nullIfEmpty(c.Understanding),
		nullIfEmpty(c.WhyKeep), nullIfEmpty(c.Unresolved),
		fromType, fromID, c.Version, c.CreatedAt.UTC(), c.UpdatedAt.UTC())
	return knowledgeWriteErr("创建卡片", err)
}

func (s *KnowledgeStore) FindCard(ctx context.Context, tenantID, id platform.ID, spaces []platform.ID) (*knowledge.Card, error) {
	if len(spaces) == 0 {
		return nil, knowledge.ErrNotFound
	}

	args := append([]any{string(id), string(tenantID)}, idArgs(spaces)...)
	c, err := s.scanCardRow(s.ex.QueryRowContext(ctx,
		`SELECT `+cardColumns+` FROM cards
		 WHERE id = ? AND tenant_id = ? AND space_id IN (`+placeholders(len(spaces))+`)`,
		args...))
	if err != nil {
		return nil, err
	}

	labels, err := s.listLabels(ctx, knowledge.Ref{Type: knowledge.EntityCard, ID: c.ID})
	if err != nil {
		return nil, err
	}
	c.Labels = labels

	triggers, err := s.listRecallTriggers(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	c.RecallTriggers = triggers
	return c, nil
}

// FindCardBySourceKey 是幂等摄取的读侧。
//
// **不带状态过滤**：一张被人改成 deprecated 的摄取卡，下次重扫仍要
// 认出它是同一份源——否则会新建一张，而那个「已经判过这份文档没用」
// 的决定就被悄悄推翻了。走 uk_cards_source (space_id, source_key)。
func (s *KnowledgeStore) FindCardBySourceKey(ctx context.Context, tenantID, spaceID platform.ID, sourceKey string) (*knowledge.Card, error) {
	if sourceKey == "" {
		// 空 source_key 在库里是 NULL，`= ''` 永远匹配不上任何行。
		// 明确报 ErrNotFound 而不是让一次拼错的调用静默返回空。
		return nil, knowledge.ErrNotFound
	}
	c, err := s.scanCardRow(s.ex.QueryRowContext(ctx,
		`SELECT `+cardColumns+` FROM cards
		 WHERE tenant_id = ? AND space_id = ? AND source_key = ?`,
		string(tenantID), string(spaceID), sourceKey))
	if err != nil {
		return nil, err
	}
	// 摄取的更新路径只碰标题与原文，不需要标签与召回条件——
	// 那些是人后来加的，正是要保住不动的东西。
	return c, nil
}

func (s *KnowledgeStore) ListCardsBySource(ctx context.Context, tenantID, spaceID platform.ID, sourceName string) ([]knowledge.Card, error) {
	if sourceName == "" {
		return nil, nil
	}
	rows, err := s.ex.QueryContext(ctx,
		`SELECT id, tenant_id, space_id, source, source_name, source_key, status
		 FROM cards
		 WHERE tenant_id = ? AND space_id = ? AND source_name = ?
		 ORDER BY source, id`, string(tenantID), string(spaceID), sourceName)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询摄取来源卡片: %w", err)
	}
	defer rows.Close()

	var out []knowledge.Card
	for rows.Next() {
		var c knowledge.Card
		var id, tid, sid string
		var source, name, key sql.NullString
		if err := rows.Scan(&id, &tid, &sid, &source, &name, &key, &c.Status); err != nil {
			return nil, fmt.Errorf("mysql: 读取摄取来源卡片: %w", err)
		}
		c.ID, c.TenantID, c.SpaceID = platform.ID(id), platform.ID(tid), platform.ID(sid)
		c.Source, c.SourceName, c.SourceKey = source.String, name.String, key.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历摄取来源卡片: %w", err)
	}
	return out, nil
}

// scanCardRow 读一行卡片。FindCard 与 FindCardBySourceKey 共用，
// 列顺序只写一遍——两份 Scan 迟早有一份忘了跟着 cardColumns 改。
func (s *KnowledgeStore) scanCardRow(row *sql.Row) (*knowledge.Card, error) {
	var (
		c                                      knowledge.Card
		cID, tID, sID, aID                     string
		agentID                                sql.NullString
		source, sourceName, srcKey, srcHash    sql.NullString
		orig, dist, under, whyKeep, unresolved sql.NullString
		fromType, fromID                       sql.NullString
		created, updated                       time.Time
	)
	err := row.Scan(&cID, &tID, &sID, &aID, &agentID, &c.Title, &c.CardType, &c.Status,
		&source, &sourceName, &srcKey, &srcHash,
		&orig, &dist, &under, &whyKeep, &unresolved,
		&fromType, &fromID, &c.Version, &created, &updated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, knowledge.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("mysql: 查询卡片: %w", err)
	}

	c.ID, c.TenantID, c.SpaceID, c.AuthorUserID = platform.ID(cID), platform.ID(tID), platform.ID(sID), platform.ID(aID)
	c.AuthorAgentID = idFromNull(agentID)
	c.Source, c.SourceName, c.SourceKey, c.SourceHash = source.String, sourceName.String, srcKey.String, srcHash.String
	c.Original, c.Distilled = orig.String, dist.String
	c.Understanding, c.WhyKeep, c.Unresolved = under.String, whyKeep.String, unresolved.String
	c.CopiedFrom = originFromNull(fromType, fromID)
	c.CreatedAt, c.UpdatedAt = created.UTC(), updated.UTC()
	return &c, nil
}

func (s *KnowledgeStore) UpdateCard(ctx context.Context, c knowledge.Card, expectedVersion int) error {
	fromType, fromID := originArgs(c.CopiedFrom)
	res, err := s.ex.ExecContext(ctx,
		`UPDATE cards SET space_id = ?, title = ?, card_type = ?, status = ?, source = ?, source_name = ?,
		        source_key = ?, source_hash = ?,
		        body_original = ?, body_distilled = ?, body_understanding = ?,
		        body_why_keep = ?, body_unresolved = ?,
		        copied_from_type = ?, copied_from_id = ?, version = ?, updated_at = ?
		 WHERE id = ? AND tenant_id = ? AND version = ?`,
		string(c.SpaceID), c.Title, c.CardType, c.Status, nullIfEmpty(c.Source), nullIfEmpty(c.SourceName),
		nullIfEmpty(c.SourceKey), nullIfEmpty(c.SourceHash),
		nullIfEmpty(c.Original), nullIfEmpty(c.Distilled), nullIfEmpty(c.Understanding),
		nullIfEmpty(c.WhyKeep), nullIfEmpty(c.Unresolved),
		fromType, fromID, c.Version, c.UpdatedAt.UTC(),
		string(c.ID), string(c.TenantID), expectedVersion)
	if err != nil {
		return fmt.Errorf("mysql: 更新卡片: %w", err)
	}
	return s.classifyUpdateResult(ctx, res, "cards", c.ID, c.TenantID, expectedVersion)
}

// classifyUpdateResult 把「影响 0 行」翻译成正确的领域错误。
//
// 乐观锁的 UPDATE 必然改变 version 列（expected → expected+1），
// 所以这里不会遇到 GovernanceStore.TouchToken 那种「写同值也算 0 行」的情况：
// 0 行只可能是 WHERE 没匹配上。但"没匹配上"仍有两种原因，
// 而它们给调用方的下一步完全不同——冲突该重试，不存在该放弃。
// 这次查询顺带取回当前版本号：契约要求冲突响应告诉调用方现在是第几版
// （docs/03-mcp-contract.md §3.4），否则 agent 只知道撞车了，还得再发一次
// get 才能重试——而那次 get 之后可能再撞一次，重试就不收敛。
// 反正这一行本来就要查，多带一列是白给的。
func (s *KnowledgeStore) classifyUpdateResult(ctx context.Context, res sql.Result, table string, id, tenantID platform.ID, expectedVersion int) error {
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}
	var current int
	// 表名来自本文件的字面量，不是外部输入
	err := s.ex.QueryRowContext(ctx,
		`SELECT version FROM `+table+` WHERE id = ? AND tenant_id = ?`,
		string(id), string(tenantID)).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return knowledge.ErrNotFound
	case err != nil:
		return fmt.Errorf("mysql: 确认 %s 是否存在: %w", table, err)
	}
	return &knowledge.VersionConflictError{Expected: expectedVersion, Current: current}
}

// ---------------------------------------------------------------------------
// 多态附属物
// ---------------------------------------------------------------------------

func (s *KnowledgeStore) listLabels(ctx context.Context, ref knowledge.Ref) ([]string, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT label FROM labels WHERE entity_type = ? AND entity_id = ? ORDER BY label`,
		ref.Type, string(ref.ID))
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询标签: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("mysql: 读取标签行: %w", err)
		}
		out = append(out, label)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历标签行: %w", err)
	}
	return out, nil
}

// ReplaceLabels 整体替换某实体的标签：先删后插。
//
// 不用 upsert + 删差集：那要先查现状再算差集，而 DELETE 走的是主键前缀
// (entity_type, entity_id)，代价与实体的标签数同阶——一个实体不会有几千个标签。
func (s *KnowledgeStore) ReplaceLabels(ctx context.Context, tenantID platform.ID, ref knowledge.Ref, labels []string, at time.Time) error {
	if _, err := s.ex.ExecContext(ctx,
		`DELETE FROM labels WHERE entity_type = ? AND entity_id = ?`,
		ref.Type, string(ref.ID)); err != nil {
		return fmt.Errorf("mysql: 清除标签: %w", err)
	}
	if len(labels) == 0 {
		return nil
	}

	values := make([]string, 0, len(labels))
	args := make([]any, 0, len(labels)*5)
	for _, label := range labels {
		values = append(values, "(?, ?, ?, ?, ?)")
		args = append(args, string(tenantID), ref.Type, string(ref.ID), label, at.UTC())
	}
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO labels (tenant_id, entity_type, entity_id, label, created_at) VALUES `+
			strings.Join(values, ", "), args...)
	return knowledgeWriteErr("写入标签", err)
}

func (s *KnowledgeStore) listRecallTriggers(ctx context.Context, cardID platform.ID) ([]knowledge.RecallTrigger, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT id, tenant_id, card_id, space_id, trigger_text, sort_order, created_at
		 FROM card_recall_triggers WHERE card_id = ? ORDER BY sort_order`,
		string(cardID))
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询召回条件: %w", err)
	}
	defer rows.Close()

	var out []knowledge.RecallTrigger
	for rows.Next() {
		var (
			t                 knowledge.RecallTrigger
			id, tid, cid, sid string
			created           time.Time
		)
		if err := rows.Scan(&id, &tid, &cid, &sid, &t.Text, &t.SortOrder, &created); err != nil {
			return nil, fmt.Errorf("mysql: 读取召回条件行: %w", err)
		}
		t.ID, t.TenantID, t.CardID, t.SpaceID = platform.ID(id), platform.ID(tid), platform.ID(cid), platform.ID(sid)
		t.CreatedAt = created.UTC()
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历召回条件行: %w", err)
	}
	return out, nil
}

// ReplaceRecallTriggers 整体替换一张卡的召回条件。
//
// 先删后插而非增量更新：召回条件是有序列表，增量更新还要维护 sort_order 的
// 连续性——整体替换让「作者重排了顺序」和「作者删了中间一条」变成同一件事。
func (s *KnowledgeStore) ReplaceRecallTriggers(ctx context.Context, tenantID, cardID platform.ID, triggers []knowledge.RecallTrigger) error {
	if _, err := s.ex.ExecContext(ctx,
		`DELETE FROM card_recall_triggers WHERE card_id = ? AND tenant_id = ?`,
		string(cardID), string(tenantID)); err != nil {
		return fmt.Errorf("mysql: 清除召回条件: %w", err)
	}
	if len(triggers) == 0 {
		return nil
	}

	values := make([]string, 0, len(triggers))
	args := make([]any, 0, len(triggers)*7)
	for _, t := range triggers {
		values = append(values, "(?, ?, ?, ?, ?, ?, ?)")
		args = append(args, string(t.ID), string(t.TenantID), string(t.CardID), string(t.SpaceID),
			t.Text, t.SortOrder, t.CreatedAt.UTC())
	}
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO card_recall_triggers
		   (id, tenant_id, card_id, space_id, trigger_text, sort_order, created_at) VALUES `+
			strings.Join(values, ", "), args...)
	return knowledgeWriteErr("写入召回条件", err)
}

// ---------------------------------------------------------------------------
// Link
// ---------------------------------------------------------------------------

func (s *KnowledgeStore) CreateLink(ctx context.Context, l knowledge.Link) error {
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO links (id, tenant_id, from_type, from_id, to_type, to_id, kind, note, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(l.ID), string(l.TenantID), l.From.Type, string(l.From.ID),
		l.To.Type, string(l.To.ID), l.Kind, nullIfEmpty(l.Note), l.CreatedAt.UTC())
	return knowledgeWriteErr("创建关联", err)
}

// DeleteLink 幂等：不存在时返回 nil。调用方的意图是「让这条关联不存在」，
// 那个状态已经达成了。
func (s *KnowledgeStore) DeleteLink(ctx context.Context, tenantID platform.ID, from, to knowledge.Ref, kind string) error {
	_, err := s.ex.ExecContext(ctx,
		`DELETE FROM links
		 WHERE tenant_id = ? AND from_type = ? AND from_id = ? AND to_type = ? AND to_id = ? AND kind = ?`,
		string(tenantID), from.Type, string(from.ID), to.Type, string(to.ID), kind)
	if err != nil {
		return fmt.Errorf("mysql: 删除关联: %w", err)
	}
	return nil
}

// ListLinks 取该实体作为起点或终点的全部关联。
//
// 不带 space_id 过滤：关联的对端可能落在调用方无权访问的空间里，
// 但这里返回的只是 ID 和 kind，不含任何内容。是否把对端的标题展示出来，
// 由应用层用可读空间集合再判一次。
func (s *KnowledgeStore) ListLinks(ctx context.Context, tenantID platform.ID, ref knowledge.Ref) ([]knowledge.Link, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT id, tenant_id, from_type, from_id, to_type, to_id, kind, note, created_at
		 FROM links
		 WHERE tenant_id = ?
		   AND ((from_type = ? AND from_id = ?) OR (to_type = ? AND to_id = ?))
		 ORDER BY id`,
		string(tenantID), ref.Type, string(ref.ID), ref.Type, string(ref.ID))
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询关联: %w", err)
	}
	defer rows.Close()

	var out []knowledge.Link
	for rows.Next() {
		var (
			l                     knowledge.Link
			id, tid, fromID, toID string
			note                  sql.NullString
			created               time.Time
		)
		if err := rows.Scan(&id, &tid, &l.From.Type, &fromID, &l.To.Type, &toID,
			&l.Kind, &note, &created); err != nil {
			return nil, fmt.Errorf("mysql: 读取关联行: %w", err)
		}
		l.ID, l.TenantID = platform.ID(id), platform.ID(tid)
		l.From.ID, l.To.ID = platform.ID(fromID), platform.ID(toID)
		l.Note = note.String
		l.CreatedAt = created.UTC()
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历关联行: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 事务
// ---------------------------------------------------------------------------

// Atomically 与 GovernanceStore.Atomically 同构：已在事务中则复用当前事务
// （MySQL 无真嵌套事务，SAVEPOINT 会让「回滚」在内外层含义不同），
// fn 的错误原样返回以便领域层用 errors.Is 认哨兵。
func (s *KnowledgeStore) Atomically(ctx context.Context, fn func(knowledge.Store) error) error {
	if s.db == nil {
		return fn(s)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysql: 开启事务: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := fn(&KnowledgeStore{ex: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysql: 提交事务: %w", err)
	}
	committed = true
	return nil
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// knowledgeWriteErr 把唯一键冲突翻译成 knowledge.ErrAlreadyExists。
//
// 与 governance 的 writeErr 分开而不是合成一个带哨兵参数的函数：
// 两个域的哨兵不同，合并之后每个调用点都要多传一个参数，
// 而传错了不会有任何编译错误——只会在某天返回一个属于另一个域的错误。
func knowledgeWriteErr(what string, err error) error {
	if err == nil {
		return nil
	}
	var me *driver.MySQLError
	if errors.As(err, &me) && me.Number == erDupEntry {
		return fmt.Errorf("%w: %s: %v", knowledge.ErrAlreadyExists, what, err)
	}
	return fmt.Errorf("mysql: %s: %w", what, err)
}

// placeholders 生成 n 个逗号分隔的 `?`，用于 IN 子句。
//
// 调用方必须先确认 n > 0：`IN ()` 是语法错误。
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

func idArgs(ids []platform.ID) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

func idFromNull(n sql.NullString) *platform.ID {
	if !n.Valid {
		return nil
	}
	id := platform.ID(n.String)
	return &id
}

// originArgs 把提升来源拆成两个可空列参数。两列同时为 NULL 或同时有值。
func originArgs(o *knowledge.Origin) (any, any) {
	if o == nil {
		return nil, nil
	}
	return o.Type, string(o.ID)
}

// originFromNull 只在两列都有值时还原来源。
//
// 只有一列有值属于数据损坏，这里选择当作"没有来源"而不是报错：
// 提升来源是可追溯性信息，不是授权依据，让它拖垮一次正常读取不划算。
func originFromNull(typ, id sql.NullString) *knowledge.Origin {
	if !typ.Valid || !id.Valid {
		return nil
	}
	return &knowledge.Origin{Type: typ.String, ID: platform.ID(id.String)}
}

// nullIfEmpty 让空字符串写成 NULL。
//
// 这些列在设计上是"可空的可选内容"，空串和 NULL 都表示"没写"。
// 统一成 NULL，是为了让 `WHERE body_understanding IS NOT NULL`
// 这类统计查询有一个确定的口径——否则"有多少张卡还只是摘要"
// 会因为写入路径的不同而给出两个数字。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
