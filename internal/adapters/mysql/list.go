package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// 本文件是治理面的列表查询（浏览页与审核队列）。
//
// 与 search.go 的分工要说清楚：那边是**检索**（MATCH 打分、候选截断、
// 只返回可见状态），这边是**列表**（确定的时间序、能翻到底、
// 看得到 draft 与 deprecated）。把两者合成一个带开关的查询，
// 会同时毁掉两边——检索多一个能把未审草稿混进 agent 上下文的开关，
// 列表还是翻不到底。

// listSQL 拼出列表与计数共用的 WHERE。
//
// 共用是硬要求：「共 128 条待审」和用户真翻出来的必须是同一批，
// 两份 WHERE 迟早会分叉，而那时那个数字就成了误导。
func listSQL(q knowledge.ListQuery) (table, where string, args []any) {
	switch q.Kind {
	case knowledge.EntityMemory:
		table = "memories"
	default:
		table = "cards"
	}

	var b strings.Builder
	b.WriteString(" WHERE tenant_id = ? AND space_id IN (")
	b.WriteString(placeholders(len(q.SpaceIDs)))
	b.WriteString(")")
	args = append(args, string(q.TenantID))
	args = append(args, idArgs(q.SpaceIDs)...)

	if len(q.Statuses) > 0 {
		b.WriteString(" AND status IN (")
		b.WriteString(placeholders(len(q.Statuses)))
		b.WriteString(")")
		for _, s := range q.Statuses {
			args = append(args, s)
		}
	}
	return table, b.String(), args
}

// ListContents 翻一页。
//
// # keyset 分页，不用 OFFSET
//
// `(updated_at, id) < (?, ?)` 这个复合比较是关键。用 OFFSET 的话，
// 翻页期间有内容被更新（updated_at 变了、排到了前面）会让某些行
// 被跳过、某些行出现两次——而审核队列恰恰是一边翻一边改的场景。
//
// 写成 `updated_at < ? OR (updated_at = ? AND id < ?)` 而不是
// MySQL 的行构造器 `(a,b) < (?,?)`：后者更短，但它对索引的利用
// 在不同版本上表现不一，而这是要翻几千条的路径。
func (s *KnowledgeStore) ListContents(ctx context.Context, q knowledge.ListQuery) ([]knowledge.ContentItem, error) {
	if len(q.SpaceIDs) == 0 {
		return nil, nil
	}
	table, where, args := listSQL(q)

	if !q.AfterUpdatedAt.IsZero() {
		where += " AND (updated_at < ? OR (updated_at = ? AND id < ?))"
		args = append(args, q.AfterUpdatedAt.UTC(), q.AfterUpdatedAt.UTC(), string(q.AfterID))
	}
	args = append(args, q.Limit)

	cols := `id, tenant_id, space_id, author_user_id, author_agent_id,
	         title, status, copied_from_type, copied_from_id, created_at, updated_at`
	if table == "cards" {
		// 卡片多取三列：类型、来源路径、以及「理解栏是否为空」。
		// 最后那个直接在 SQL 里算成布尔，与 space_stats 的
		// `body_understanding IS NULL` 是同一份口径——两处必须一致。
		cols += `, card_type, source, (body_understanding IS NOT NULL) AS qualified`
	}

	rows, err := s.ex.QueryContext(ctx,
		`SELECT `+cols+` FROM `+table+where+`
		 ORDER BY updated_at DESC, id DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 列表查询: %w", err)
	}
	defer rows.Close()

	var out []knowledge.ContentItem
	for rows.Next() {
		item, err := scanContentItem(rows, q.Kind)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历列表行: %w", err)
	}
	return out, nil
}

func scanContentItem(rows *sql.Rows, kind string) (knowledge.ContentItem, error) {
	var (
		it                knowledge.ContentItem
		id, tid, sid, aid string
		agentID           sql.NullString
		fromType, fromID  sql.NullString
		cardType, source  sql.NullString
		qualified         sql.NullBool
		created, updated  time.Time
	)
	dest := []any{&id, &tid, &sid, &aid, &agentID, &it.Title, &it.Status,
		&fromType, &fromID, &created, &updated}
	if kind == knowledge.EntityCard {
		dest = append(dest, &cardType, &source, &qualified)
	}
	if err := rows.Scan(dest...); err != nil {
		return knowledge.ContentItem{}, fmt.Errorf("mysql: 读取列表行: %w", err)
	}

	it.Ref = knowledge.Ref{Type: kind, ID: platform.ID(id)}
	it.SpaceID, it.AuthorUserID = platform.ID(sid), platform.ID(aid)
	it.AuthorAgentID = idFromNull(agentID)
	it.CopiedFrom = originFromNull(fromType, fromID)
	it.CardType, it.SourcePath = cardType.String, source.String
	it.Qualified = qualified.Bool
	it.CreatedAt, it.UpdatedAt = created.UTC(), updated.UTC()
	_ = tid // 已由 WHERE 限定，不必回填
	return it, nil
}

// CountContents 数总数，**忽略游标与 Limit**。
//
// 与 ListContents 共用 listSQL：两份 WHERE 会分叉，而那时
// 「共 128 条」和用户真翻出来的条数对不上，比不给这个数字更糟。
func (s *KnowledgeStore) CountContents(ctx context.Context, q knowledge.ListQuery) (int, error) {
	if len(q.SpaceIDs) == 0 {
		return 0, nil
	}
	table, where, args := listSQL(q)

	var n int
	if err := s.ex.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("mysql: 统计列表总数: %w", err)
	}
	return n, nil
}
