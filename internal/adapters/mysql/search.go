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

// 本文件是两段式检索的**第一段**：粗筛（docs/02-data-model.md §5.1）。
//
// 一条纪律高于一切：**权限过滤下推到 SQL，绝不后置**。下面每个查询里的
// `space_id IN (...)` 都不是优化，是安全边界——查全量再在应用层筛，
// 那些行已经进过进程内存了，而这个产品的全部价值建立在
// 「个人空间是敢于随手记录的前提」上。
//
// 与 governance.go 同一条纪律的另一半：这里出现的 `IN (...)` 是领域层
// 交下来的确定值，本文件既不知道它们怎么来的，也不得对它们做任何加工。
// 一旦出现 `OR type = 'team'` 这类东西，权限规则就有了第二个副本。
//
// # 已知的规模上限
//
// MATCH 只出现在 SELECT 列表里而不进 WHERE：那样候选集才能同时容纳
// 「字面命中」和「字面没命中但可能语义相关」两类行，截断也才有依据
// （§5.1 要求"按非向量信号排序后截断，而非随机丢弃"）。代价是每次检索
// 要为**空间内的全部可见行**算一次相关性，而不是只扫全文索引的命中集。
//
// 这在一个团队知识库的规模（万级）上完全没问题，但它是这套检索第一个
// 会撞墙的地方。真撞上时该做的是把粗筛拆成「全文索引命中集 ∪ 最近内容」
// 两条索引路径求并，而不是调 candidateCap——MySQL 8.0 没有 ANN 索引，
// 「向量相关但字面不沾边且不新近」的内容进不了候选集是结构性的，
// 不是参数问题。

// searchColumns 拼出「实体列 + 相关性分数」的 SELECT 列表。
//
// query 为空时分数写成常量 0：MATCH 一个空串在 MySQL 里合法但没有意义，
// 而多带一个恒为 0 的全文计算是白花的钱。
func matchExpr(cols, query string) (string, []any) {
	if strings.TrimSpace(query) == "" {
		return "0", nil
	}
	// MATCH 的索引在刚提交后可能短暂不可见。给明确的字面包含一个极小正分，
	// 保住「刚写完立刻搜」；真正的全文相关性一旦可见，量级远大于这个兜底，
	// 不改变正常排序。
	return "MATCH(" + cols + ") AGAINST (? IN NATURAL LANGUAGE MODE) + " +
			"CASE WHEN CONCAT_WS(' ', " + cols + ") LIKE ? ESCAPE '=' THEN 0.000001 ELSE 0 END",
		[]any{query, literalLikePattern(query)}
}

func literalLikePattern(query string) string {
	replacer := strings.NewReplacer("=", "==", "%", "=%", "_", "=_")
	return "%" + replacer.Replace(strings.TrimSpace(query)) + "%"
}

// labelFilter 生成标签**交集**过滤。
//
// HAVING COUNT(*) = n 是交集的关键：不加它就退化成并集（有任意一个标签即命中），
// 而「缩小范围」是这个参数唯一的用途——并集可以靠调用两次得到，交集不行。
// labels 的主键是 (entity_type, entity_id, label)，所以 COUNT(*) 就是不同标签数。
func labelFilter(tenantID platform.ID, entityType string, labels []string) (string, []any) {
	if len(labels) == 0 {
		return "", nil
	}
	args := []any{string(tenantID), entityType}
	for _, l := range labels {
		args = append(args, l)
	}
	args = append(args, len(labels))
	return ` AND id IN (
		SELECT entity_id FROM labels
		WHERE tenant_id = ? AND entity_type = ? AND label IN (` + placeholders(len(labels)) + `)
		GROUP BY entity_id HAVING COUNT(*) = ?)`, args
}

func (s *KnowledgeStore) SearchMemories(ctx context.Context, q knowledge.CandidateQuery) ([]knowledge.MemoryCandidate, error) {
	if len(q.SpaceIDs) == 0 {
		return nil, nil
	}
	score, args := matchExpr("title, body", q.Query)
	args = append(args, string(q.TenantID))
	args = append(args, idArgs(q.SpaceIDs)...)
	statuses := q.MemoryStatuses
	if len(statuses) == 0 {
		statuses = []string{knowledge.MemoryActive}
	}
	for _, status := range statuses {
		args = append(args, status)
	}

	where, labelArgs := labelFilter(q.TenantID, knowledge.EntityMemory, q.Labels)
	args = append(args, labelArgs...)
	args = append(args, q.Limit)

	rows, err := s.ex.QueryContext(ctx,
		`SELECT `+memoryColumns+`, `+score+` AS score
		 FROM memories
		 WHERE tenant_id = ? AND space_id IN (`+placeholders(len(q.SpaceIDs))+`) AND status IN (`+placeholders(len(statuses))+`)`+where+`
		 ORDER BY score DESC, updated_at DESC, id ASC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 粗筛记忆: %w", err)
	}
	defer rows.Close()

	var out []knowledge.MemoryCandidate
	for rows.Next() {
		var (
			c                  knowledge.MemoryCandidate
			mID, tID, sID, aID string
			agentID            sql.NullString
			fromType, fromID   sql.NullString
			created, updated   time.Time
		)
		if err := rows.Scan(&mID, &tID, &sID, &aID, &agentID, &c.Memory.Title, &c.Memory.Body,
			&c.Memory.Status, &fromType, &fromID, &c.Memory.Version, &created, &updated,
			&c.TextScore); err != nil {
			return nil, fmt.Errorf("mysql: 读取记忆候选行: %w", err)
		}
		m := &c.Memory
		m.ID, m.TenantID, m.SpaceID, m.AuthorUserID = platform.ID(mID), platform.ID(tID), platform.ID(sID), platform.ID(aID)
		m.AuthorAgentID = idFromNull(agentID)
		m.CopiedFrom = originFromNull(fromType, fromID)
		m.CreatedAt, m.UpdatedAt = created.UTC(), updated.UTC()
		// Labels 刻意不填：那要么是 N+1 次查询，要么是一次会让候选集
		// 膨胀成笛卡尔积的 JOIN。要标签就按 ID 取单条。
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历记忆候选行: %w", err)
	}
	return out, nil
}

// cardFullTextColumns 必须与 0002 迁移里 ft_cards 的列**逐字一致**。
//
// MySQL 要求 MATCH 的列集合与某个 FULLTEXT 索引完全对应，对不上会直接报
// "Can't find FULLTEXT index matching the column list"——这是少数会当场报错
// 的偏差，比静默变慢好得多，但仍然值得在这里写清楚它为什么不能随手改。
const cardFullTextColumns = "title, body_distilled, body_understanding"

func (s *KnowledgeStore) SearchCards(ctx context.Context, q knowledge.CandidateQuery) ([]knowledge.CardCandidate, error) {
	if len(q.SpaceIDs) == 0 {
		return nil, nil
	}
	score, args := matchExpr(cardFullTextColumns, q.Query)
	args = append(args, string(q.TenantID))
	args = append(args, idArgs(q.SpaceIDs)...)
	args = append(args, knowledge.CardActive)

	where, labelArgs := labelFilter(q.TenantID, knowledge.EntityCard, q.Labels)
	args = append(args, labelArgs...)
	args = append(args, q.Limit)

	rows, err := s.ex.QueryContext(ctx,
		`SELECT `+cardColumns+`, `+score+` AS score
		 FROM cards
		 WHERE tenant_id = ? AND space_id IN (`+placeholders(len(q.SpaceIDs))+`) AND status = ?`+where+`
		 ORDER BY score DESC, updated_at DESC, id ASC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 粗筛卡片: %w", err)
	}
	defer rows.Close()

	var out []knowledge.CardCandidate
	for rows.Next() {
		c := knowledge.CardCandidate{}
		if err := scanCard(rows, &c.Card, &c.TextScore); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历卡片候选行: %w", err)
	}
	return out, nil
}

// CountCardMatches 数「另有 N 条卡片命中」。
//
// 这里 MATCH **进 WHERE**（与 SearchCards 相反）：那个数字回答的是
// 「另有多少条卡片命中」，把没命中的也数进去就是在骗人。
// 走 ngram 索引的一次 COUNT，代价与返回内容完全不同。
//
// 这里使用 BOOLEAN MODE，不使用检索排序的 NATURAL LANGUAGE MODE：后者的
// 相关性统计会把高频词判成 0 分。在小团队或刚冷启动的库里，一条术语只要
// 出现在少量卡片中的一半，就可能出现「正文明确包含，命中数却是 0」的静默
// 假阴性。这个查询只回答有没有字面命中，不承担相关性排序，布尔模式才是
// 与产品语义一致的选择。
//
// 仍保留一个转义后的字面包含兜底。InnoDB FULLTEXT 在刚提交新卡后存在很短的
// 可见性窗口：行已经能 SELECT 到，MATCH 仍可能返回 0；团队冷启动时用户最
// 容易在写完卡后立刻搜索，不能让这个窗口把真实命中说成 0。LIKE 只在这次
// 低频提示 COUNT 中运行，且租户、空间、状态过滤仍先下推。
func (s *KnowledgeStore) CountCardMatches(ctx context.Context, q knowledge.CandidateQuery) (int, error) {
	if len(q.SpaceIDs) == 0 || strings.TrimSpace(q.Query) == "" {
		return 0, nil
	}
	args := []any{string(q.TenantID)}
	args = append(args, idArgs(q.SpaceIDs)...)
	args = append(args, knowledge.CardActive)

	where, labelArgs := labelFilter(q.TenantID, knowledge.EntityCard, q.Labels)
	args = append(args, labelArgs...)
	args = append(args, q.Query, literalLikePattern(q.Query))

	var n int
	err := s.ex.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cards
		 WHERE tenant_id = ? AND space_id IN (`+placeholders(len(q.SpaceIDs))+`) AND status = ?`+where+`
		   AND (
		     MATCH(`+cardFullTextColumns+`) AGAINST (? IN BOOLEAN MODE)
		     OR CONCAT_WS(' ', title, body_distilled, body_understanding) LIKE ? ESCAPE '='
		   )`, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("mysql: 统计卡片命中: %w", err)
	}
	return n, nil
}

// SearchRecallTriggers 是 card_recall 的第一段粗筛。
//
// **必须 JOIN cards**，两件事都躲不掉：
//
//   - 状态过滤。card_recall_triggers 上没有 status 列，而 draft（摄取未审）
//     与 deprecated（已过时）的卡片不该被召回。
//   - 权限。触发条件表上那个 space_id 是**冗余列**，拿它当权限依据
//     等于信任一份可能与 cards 表不同步的副本；提升、迁移、补数据
//     任何一次没同步它，就是一次静默越权。
//
// 0002 迁移里「冗余 space_id 使粗筛无需 JOIN cards」那句话只对纯权限过滤成立，
// 当时没想到状态过滤（实现检索时发现，2026-08-10）。冗余列没有白留——
// 它仍然是「哪些触发条件属于我能看到的空间」最快的一条索引路径，
// 只是不能单独成为唯一依据。
func (s *KnowledgeStore) SearchRecallTriggers(ctx context.Context, q knowledge.CandidateQuery) ([]knowledge.TriggerCandidate, error) {
	if len(q.SpaceIDs) == 0 {
		return nil, nil
	}
	score, args := matchExpr("t.trigger_text", q.Query)
	args = append(args, string(q.TenantID), string(q.TenantID))
	args = append(args, idArgs(q.SpaceIDs)...)
	args = append(args, knowledge.CardActive, q.Limit)

	rows, err := s.ex.QueryContext(ctx,
		`SELECT t.id, t.tenant_id, t.card_id, t.space_id, t.trigger_text, t.sort_order, t.created_at,
		        `+score+` AS score
		 FROM card_recall_triggers t
		 JOIN cards c ON c.id = t.card_id
		 WHERE t.tenant_id = ? AND c.tenant_id = ?
		   AND c.space_id IN (`+placeholders(len(q.SpaceIDs))+`)
		   AND c.status = ?
		 ORDER BY score DESC, t.created_at DESC, t.id ASC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 粗筛召回条件: %w", err)
	}
	defer rows.Close()

	var out []knowledge.TriggerCandidate
	for rows.Next() {
		var (
			c                 knowledge.TriggerCandidate
			id, tid, cid, sid string
			created           time.Time
		)
		if err := rows.Scan(&id, &tid, &cid, &sid, &c.Trigger.Text, &c.Trigger.SortOrder,
			&created, &c.TextScore); err != nil {
			return nil, fmt.Errorf("mysql: 读取召回条件候选行: %w", err)
		}
		t := &c.Trigger
		t.ID, t.TenantID, t.CardID, t.SpaceID = platform.ID(id), platform.ID(tid), platform.ID(cid), platform.ID(sid)
		t.CreatedAt = created.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历召回条件候选行: %w", err)
	}
	return out, nil
}

// FindCards 批量回表取整卡（card_recall 的第二步）。
//
// 取不到的 ID 直接不出现，不报 ErrNotFound：召回本来就是「有什么给什么」，
// 一条悬空的触发条件（多态引用没有外键，02 §3.1 的明确权衡）
// 不该让整次召回失败。
func (s *KnowledgeStore) FindCards(ctx context.Context, tenantID platform.ID, ids, spaces []platform.ID) ([]knowledge.Card, error) {
	if len(ids) == 0 || len(spaces) == 0 {
		return nil, nil
	}
	args := []any{string(tenantID)}
	args = append(args, idArgs(ids)...)
	args = append(args, idArgs(spaces)...)
	args = append(args, knowledge.CardActive)

	rows, err := s.ex.QueryContext(ctx,
		`SELECT `+cardColumns+`, 0 AS score FROM cards
		 WHERE tenant_id = ? AND id IN (`+placeholders(len(ids))+`)
		   AND space_id IN (`+placeholders(len(spaces))+`) AND status = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 批量查询卡片: %w", err)
	}
	defer rows.Close()

	var (
		out     []knowledge.Card
		cardIDs []platform.ID
	)
	for rows.Next() {
		var (
			c     knowledge.Card
			score float64
		)
		if err := scanCard(rows, &c, &score); err != nil {
			return nil, err
		}
		out = append(out, c)
		cardIDs = append(cardIDs, c.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历卡片行: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}

	// 标签与召回条件各批量取一次，不做 N+1。召回结果要带着这两样：
	// 卡片的价值一半在「何时该想起它」，返回一张没有触发条件的卡
	// 等于让调用方看不出它为什么会出现。
	labels, err := s.listLabelsBatch(ctx, knowledge.EntityCard, cardIDs)
	if err != nil {
		return nil, err
	}
	triggers, err := s.listRecallTriggersBatch(ctx, cardIDs)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Labels = labels[out[i].ID]
		out[i].RecallTriggers = triggers[out[i].ID]
	}
	return out, nil
}

// scanCard 读一行卡片 + 相关性分数。SearchCards 与 FindCards 共用，
// 列顺序只写一遍——两份 Scan 迟早会有一份忘了跟着 cardColumns 改。
func scanCard(rows *sql.Rows, c *knowledge.Card, score *float64) error {
	var (
		cID, tID, sID, aID                     string
		agentID                                sql.NullString
		source, sourceName, srcKey, srcHash    sql.NullString
		orig, dist, under, whyKeep, unresolved sql.NullString
		fromType, fromID                       sql.NullString
		created, updated                       time.Time
	)
	if err := rows.Scan(&cID, &tID, &sID, &aID, &agentID, &c.Title, &c.CardType, &c.Status,
		&source, &sourceName, &srcKey, &srcHash,
		&orig, &dist, &under, &whyKeep, &unresolved,
		&fromType, &fromID, &c.Version, &created, &updated, score); err != nil {
		return fmt.Errorf("mysql: 读取卡片行: %w", err)
	}
	c.ID, c.TenantID, c.SpaceID, c.AuthorUserID = platform.ID(cID), platform.ID(tID), platform.ID(sID), platform.ID(aID)
	c.AuthorAgentID = idFromNull(agentID)
	c.Source, c.SourceName, c.SourceKey, c.SourceHash = source.String, sourceName.String, srcKey.String, srcHash.String
	c.Original, c.Distilled = orig.String, dist.String
	c.Understanding, c.WhyKeep, c.Unresolved = under.String, whyKeep.String, unresolved.String
	c.CopiedFrom = originFromNull(fromType, fromID)
	c.CreatedAt, c.UpdatedAt = created.UTC(), updated.UTC()
	return nil
}

func (s *KnowledgeStore) listLabelsBatch(ctx context.Context, entityType string, ids []platform.ID) (map[platform.ID][]string, error) {
	args := append([]any{entityType}, idArgs(ids)...)
	rows, err := s.ex.QueryContext(ctx,
		`SELECT entity_id, label FROM labels
		 WHERE entity_type = ? AND entity_id IN (`+placeholders(len(ids))+`)
		 ORDER BY entity_id, label`, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 批量查询标签: %w", err)
	}
	defer rows.Close()

	out := map[platform.ID][]string{}
	for rows.Next() {
		var id, label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, fmt.Errorf("mysql: 读取标签行: %w", err)
		}
		out[platform.ID(id)] = append(out[platform.ID(id)], label)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历标签行: %w", err)
	}
	return out, nil
}

func (s *KnowledgeStore) listRecallTriggersBatch(ctx context.Context, cardIDs []platform.ID) (map[platform.ID][]knowledge.RecallTrigger, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT id, tenant_id, card_id, space_id, trigger_text, sort_order, created_at
		 FROM card_recall_triggers
		 WHERE card_id IN (`+placeholders(len(cardIDs))+`)
		 ORDER BY card_id, sort_order`, idArgs(cardIDs)...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 批量查询召回条件: %w", err)
	}
	defer rows.Close()

	out := map[platform.ID][]knowledge.RecallTrigger{}
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
		out[t.CardID] = append(out[t.CardID], t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历召回条件行: %w", err)
	}
	return out, nil
}

// CountSpaceContents 统计每个空间的内容量（space_stats）。
//
// 三条独立的 GROUP BY 而不是一条大 JOIN：JOIN 三张一对多的表会产生笛卡尔积，
// 「一张有 3 条召回条件的卡」会把卡片数数成 3。要正确就得套子查询，
// 而三条各走各索引的简单查询比一条难读的查询更便宜也更好改。
//
// `body_understanding IS NULL` 是 Card.IsQualified() 在 SQL 里的第二份表达。
// 两者必须始终等价——0002 迁移「空 section 写 NULL 而非空串」正是为这个
// 查询留的口径，写入侧的 nullIfEmpty 保证不会产生空串。领域层有测试
// 盯着这对等价关系。
func (s *KnowledgeStore) CountSpaceContents(ctx context.Context, tenantID platform.ID, spaces []platform.ID) ([]knowledge.SpaceStats, error) {
	if len(spaces) == 0 {
		return nil, nil
	}
	byID := make(map[platform.ID]*knowledge.SpaceStats, len(spaces))
	stat := func(id platform.ID) *knowledge.SpaceStats {
		if st, ok := byID[id]; ok {
			return st
		}
		st := &knowledge.SpaceStats{SpaceID: id}
		byID[id] = st
		return st
	}

	args := append([]any{string(tenantID)}, idArgs(spaces)...)
	in := placeholders(len(spaces))

	// 记忆：只数默认可见的（软删除的不算内容量）
	memArgs := append(append([]any{}, args...), knowledge.MemoryActive)
	if err := s.scanCounts(ctx,
		`SELECT space_id, COUNT(*) FROM memories
		 WHERE tenant_id = ? AND space_id IN (`+in+`) AND status = ?
		 GROUP BY space_id`, memArgs,
		func(id platform.ID, n int) { stat(id).Memories = n }); err != nil {
		return nil, err
	}

	// 卡片：一次查出总数、草稿数、只是摘要的数量
	rows, err := s.ex.QueryContext(ctx,
		`SELECT space_id, COUNT(*),
		        SUM(status = ?),
		        SUM(body_understanding IS NULL)
		 FROM cards
		 WHERE tenant_id = ? AND space_id IN (`+in+`)
		 GROUP BY space_id`,
		append([]any{knowledge.CardDraft}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 统计卡片: %w", err)
	}
	if err := func() error {
		defer rows.Close()
		for rows.Next() {
			var (
				id                        string
				total, draft, unqualified int
			)
			if err := rows.Scan(&id, &total, &draft, &unqualified); err != nil {
				return fmt.Errorf("mysql: 读取卡片统计行: %w", err)
			}
			st := stat(platform.ID(id))
			st.Cards, st.DraftCards, st.UnqualifiedCards = total, draft, unqualified
		}
		return rows.Err()
	}(); err != nil {
		return nil, err
	}

	if err := s.scanCounts(ctx,
		`SELECT space_id, COUNT(*) FROM card_recall_triggers
		 WHERE tenant_id = ? AND space_id IN (`+in+`)
		 GROUP BY space_id`, args,
		func(id platform.ID, n int) { stat(id).RecallTriggers = n }); err != nil {
		return nil, err
	}

	out := make([]knowledge.SpaceStats, 0, len(byID))
	for _, st := range byID {
		out = append(out, *st)
	}
	return out, nil
}

func (s *KnowledgeStore) scanCounts(ctx context.Context, query string, args []any, assign func(platform.ID, int)) error {
	rows, err := s.ex.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("mysql: 统计内容量: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id string
			n  int
		)
		if err := rows.Scan(&id, &n); err != nil {
			return fmt.Errorf("mysql: 读取统计行: %w", err)
		}
		assign(platform.ID(id), n)
	}
	return rows.Err()
}
