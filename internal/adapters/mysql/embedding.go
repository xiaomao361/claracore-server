package mysql

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// 本文件是 embeddings 表的读写：既是向量存储，也是索引队列。
//
// 队列查询本身不持有跨请求行锁。没有用 `SELECT ... FOR UPDATE SKIP LOCKED`
// 领活，是因为
// 那要求持有事务直到向量算完，而中间夹着一次外部 HTTP 调用——
// 一个跨越外部服务超时的长事务，比重复算一次向量危险得多。
//
// 多实例由 worker 外层的 Redis 短租约协调；Redis 未配置或暂时故障时
// fail-open，SaveEmbeddingVector 仍按 ID 幂等覆盖。这样不把可选 Redis
// 变成索引可用性的硬依赖，最坏代价仍只是一次重复外部调用。

// encodeVector 把 float32 切片编成小端紧凑字节。
//
// 不存 JSON：一个 1024 维向量的 JSON 大约 12KB，二进制是 4KB，而检索的
// 第二段要一次拉几百条——那三倍差价全部落在网络与解码上。
func encodeVector(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[4*i:], math.Float32bits(f))
	}
	return buf
}

// decodeVector 是 encodeVector 的逆。长度不是 4 的倍数说明这一行坏了
// （半截写入、或者被别的编码写过），当作没有向量处理而不是返回半个向量——
// 半个向量算出来的余弦是一个看起来很正常的数字，那比报错难查得多。
func decodeVector(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

// EnqueueEmbedding 入队，内容未变时**不动已算好的向量**。
//
// 这段 upsert 有一个真实存在的坑：MySQL 的 ON DUPLICATE KEY UPDATE
// 里，等号右边引用的是**赋值发生那一刻**的值，而赋值是从左到右依次进行的。
// 所以 content_hash 必须放在最后一行——它一旦先被覆盖，前面几行用来
// 判断「内容变了没有」的比较就全部退化成恒等式，每次入队都会作废向量。
//
// 用行别名 `AS v` 而不是已废弃的 VALUES()（MySQL 8.0.20+，库是 8.0.36）。
func (s *KnowledgeStore) EnqueueEmbedding(ctx context.Context, e knowledge.Embedding) error {
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO embeddings
		   (id, tenant_id, entity_type, entity_id, model, content_hash, source_text,
		    vector, dim, status, attempts, last_error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, 0, ?, 0, NULL, ?, ?) AS v
		 ON DUPLICATE KEY UPDATE
		   status     = IF(embeddings.content_hash = v.content_hash, embeddings.status, v.status),
		   vector     = IF(embeddings.content_hash = v.content_hash, embeddings.vector, NULL),
		   dim        = IF(embeddings.content_hash = v.content_hash, embeddings.dim, 0),
		   attempts   = IF(embeddings.content_hash = v.content_hash, embeddings.attempts, 0),
		   last_error = IF(embeddings.content_hash = v.content_hash, embeddings.last_error, NULL),
		   source_text  = v.source_text,
		   updated_at   = v.updated_at,
		   content_hash = v.content_hash`,
		string(e.ID), string(e.TenantID), e.Ref.Type, string(e.Ref.ID), e.Model,
		e.ContentHash, e.SourceText, e.Status, e.CreatedAt.UTC(), e.UpdatedAt.UTC())
	return knowledgeWriteErr("入队 embedding", err)
}

// DeleteCardTriggerEmbeddings 删掉一张卡全部召回条件的向量行。
//
// 必须在 ReplaceRecallTriggers 之前调用：那个方法先删后插，旧触发条件的行
// 一旦没了，这个 JOIN 就再也定位不到它们留下的向量，那些行会永远躺在库里。
func (s *KnowledgeStore) DeleteCardTriggerEmbeddings(ctx context.Context, tenantID, cardID platform.ID) error {
	_, err := s.ex.ExecContext(ctx,
		`DELETE e FROM embeddings e
		 JOIN card_recall_triggers t ON t.id = e.entity_id
		 WHERE e.tenant_id = ? AND e.entity_type = ? AND t.tenant_id = ? AND t.card_id = ?`,
		string(tenantID), knowledge.EntityCardTrigger, string(tenantID), string(cardID))
	if err != nil {
		return fmt.Errorf("mysql: 清除召回条件 embedding: %w", err)
	}
	return nil
}

// ListPendingEmbeddings 领一批待算的向量。
//
// status <> ready 一并覆盖 pending 与 failed：两者都还在队列里，
// 区别只是后者带着上次的原因和重试计数。走 idx_emb_queue (model, status, updated_at)。
func (s *KnowledgeStore) ListPendingEmbeddings(ctx context.Context, model string, maxAttempts, limit int) ([]knowledge.Embedding, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT id, tenant_id, entity_type, entity_id, model, content_hash, source_text,
		        status, attempts, created_at, updated_at
		 FROM embeddings
		 WHERE model = ? AND status <> ? AND attempts < ?
		 ORDER BY updated_at ASC, id ASC
		 LIMIT ?`,
		model, knowledge.EmbedReady, maxAttempts, limit)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询待算 embedding: %w", err)
	}
	defer rows.Close()

	var out []knowledge.Embedding
	for rows.Next() {
		var (
			e                knowledge.Embedding
			id, tid, eid     string
			created, updated time.Time
		)
		if err := rows.Scan(&id, &tid, &e.Ref.Type, &eid, &e.Model, &e.ContentHash,
			&e.SourceText, &e.Status, &e.Attempts, &created, &updated); err != nil {
			return nil, fmt.Errorf("mysql: 读取待算 embedding 行: %w", err)
		}
		e.ID, e.TenantID, e.Ref.ID = platform.ID(id), platform.ID(tid), platform.ID(eid)
		e.CreatedAt, e.UpdatedAt = created.UTC(), updated.UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历待算 embedding 行: %w", err)
	}
	return out, nil
}

// SaveEmbeddingVector 写回算好的向量。
//
// 幂等的按 ID 覆盖写：多实例重复算出同一个向量时，写两遍结果一致
// （见本文件头部关于领活的说明）。attempts 归零、last_error 清空——
// 「以前失败过」在成功之后不再是有用信息，留着只会让运维查错方向。
func (s *KnowledgeStore) SaveEmbeddingVector(ctx context.Context, id platform.ID, vector []float32, at time.Time) error {
	res, err := s.ex.ExecContext(ctx,
		`UPDATE embeddings
		 SET vector = ?, dim = ?, status = ?, attempts = 0, last_error = NULL, updated_at = ?
		 WHERE id = ?`,
		encodeVector(vector), len(vector), knowledge.EmbedReady, at.UTC(), string(id))
	if err != nil {
		return fmt.Errorf("mysql: 写回向量: %w", err)
	}
	// 这条 UPDATE 必然改变 status 或 vector，所以 0 行只可能是行不存在——
	// 不会遇到 TouchToken 那种「写同值也算 0 行」的情况。
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return knowledge.ErrNotFound
	}
	return nil
}

// MarkEmbeddingsFailed 批量记失败：累加 attempts、留下原因、更新时间戳。
//
// 更新时间戳不是顺手：ORDER BY updated_at ASC 领活，于是失败的行自然
// 排到队尾，不会霸着队头把整批活反复卡死。
func (s *KnowledgeStore) MarkEmbeddingsFailed(ctx context.Context, ids []platform.ID, reason string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{knowledge.EmbedFailed, reason, at.UTC()}
	args = append(args, idArgs(ids)...)
	_, err := s.ex.ExecContext(ctx,
		`UPDATE embeddings
		 SET status = ?, attempts = attempts + 1, last_error = ?, updated_at = ?
		 WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return fmt.Errorf("mysql: 标记 embedding 失败: %w", err)
	}
	return nil
}

func (s *KnowledgeStore) ListMemoriesMissingEmbedding(ctx context.Context, tenantID platform.ID, model string, limit int) ([]knowledge.Memory, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT m.id, m.tenant_id, m.title, m.body, m.status
		 FROM memories m
		 LEFT JOIN embeddings e
		   ON e.tenant_id = m.tenant_id AND e.entity_type = ? AND e.entity_id = m.id AND e.model = ?
		 WHERE m.tenant_id = ? AND m.status = ? AND e.id IS NULL
		 ORDER BY m.id ASC LIMIT ?`,
		knowledge.EntityMemory, model, string(tenantID), knowledge.MemoryActive, limit)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询未索引记忆: %w", err)
	}
	defer rows.Close()
	var out []knowledge.Memory
	for rows.Next() {
		var m knowledge.Memory
		var id, tid string
		if err := rows.Scan(&id, &tid, &m.Title, &m.Body, &m.Status); err != nil {
			return nil, fmt.Errorf("mysql: 读取未索引记忆: %w", err)
		}
		m.ID, m.TenantID = platform.ID(id), platform.ID(tid)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *KnowledgeStore) ListCardsMissingEmbedding(ctx context.Context, tenantID platform.ID, model string, limit int) ([]knowledge.Card, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT c.id, c.tenant_id, c.title, c.body_distilled, c.body_understanding, c.status
		 FROM cards c
		 LEFT JOIN embeddings e
		   ON e.tenant_id = c.tenant_id AND e.entity_type = ? AND e.entity_id = c.id AND e.model = ?
		 WHERE c.tenant_id = ? AND c.status = ? AND e.id IS NULL
		 ORDER BY c.id ASC LIMIT ?`,
		knowledge.EntityCard, model, string(tenantID), knowledge.CardActive, limit)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询未索引卡片: %w", err)
	}
	defer rows.Close()
	var out []knowledge.Card
	for rows.Next() {
		var c knowledge.Card
		var id, tid string
		var distilled, understanding sql.NullString
		if err := rows.Scan(&id, &tid, &c.Title, &distilled, &understanding, &c.Status); err != nil {
			return nil, fmt.Errorf("mysql: 读取未索引卡片: %w", err)
		}
		c.ID, c.TenantID = platform.ID(id), platform.ID(tid)
		c.Distilled, c.Understanding = distilled.String, understanding.String
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *KnowledgeStore) ListTriggersMissingEmbedding(ctx context.Context, tenantID platform.ID, model string, limit int) ([]knowledge.RecallTrigger, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT t.id, t.tenant_id, t.card_id, t.space_id, t.trigger_text, t.sort_order, t.created_at
		 FROM card_recall_triggers t
		 JOIN cards c ON c.tenant_id = t.tenant_id AND c.id = t.card_id
		 LEFT JOIN embeddings e
		   ON e.tenant_id = t.tenant_id AND e.entity_type = ? AND e.entity_id = t.id AND e.model = ?
		 WHERE t.tenant_id = ? AND c.status = ? AND e.id IS NULL
		 ORDER BY t.id ASC LIMIT ?`,
		knowledge.EntityCardTrigger, model, string(tenantID), knowledge.CardActive, limit)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询未索引召回条件: %w", err)
	}
	defer rows.Close()
	var out []knowledge.RecallTrigger
	for rows.Next() {
		var trigger knowledge.RecallTrigger
		var id, tid, cardID, spaceID string
		var created time.Time
		if err := rows.Scan(&id, &tid, &cardID, &spaceID, &trigger.Text, &trigger.SortOrder, &created); err != nil {
			return nil, fmt.Errorf("mysql: 读取未索引召回条件: %w", err)
		}
		trigger.ID, trigger.TenantID = platform.ID(id), platform.ID(tid)
		trigger.CardID, trigger.SpaceID, trigger.CreatedAt = platform.ID(cardID), platform.ID(spaceID), created.UTC()
		out = append(out, trigger)
	}
	return out, rows.Err()
}

func (s *KnowledgeStore) EmbeddingStats(ctx context.Context, tenantID platform.ID, model string, maxAttempts int) (knowledge.EmbeddingStats, error) {
	var stats knowledge.EmbeddingStats
	err := s.ex.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(status = ?), 0),
		   COALESCE(SUM(status = ?), 0),
		   COALESCE(SUM(status = ?), 0),
		   COALESCE(SUM(status = ? AND attempts >= ?), 0)
		 FROM embeddings WHERE tenant_id = ? AND model = ?`,
		knowledge.EmbedPending, knowledge.EmbedReady, knowledge.EmbedFailed,
		knowledge.EmbedFailed, maxAttempts, string(tenantID), model,
	).Scan(&stats.Pending, &stats.Ready, &stats.Failed, &stats.Exhausted)
	if err != nil {
		return knowledge.EmbeddingStats{}, fmt.Errorf("mysql: 统计 embedding: %w", err)
	}
	return stats, nil
}

func (s *KnowledgeStore) ListEmbeddingFailures(ctx context.Context, tenantID platform.ID, model string, limit int) ([]knowledge.Embedding, error) {
	rows, err := s.ex.QueryContext(ctx,
		`SELECT id, entity_type, entity_id, attempts, last_error, updated_at
		 FROM embeddings
		 WHERE tenant_id = ? AND model = ? AND status = ?
		 ORDER BY updated_at DESC, id DESC LIMIT ?`,
		string(tenantID), model, knowledge.EmbedFailed, limit)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询 embedding 失败项: %w", err)
	}
	defer rows.Close()
	var out []knowledge.Embedding
	for rows.Next() {
		var e knowledge.Embedding
		var id, entityID string
		var lastError sql.NullString
		var updated time.Time
		if err := rows.Scan(&id, &e.Ref.Type, &entityID, &e.Attempts, &lastError, &updated); err != nil {
			return nil, fmt.Errorf("mysql: 读取 embedding 失败项: %w", err)
		}
		e.ID, e.TenantID, e.Ref.ID, e.Model = platform.ID(id), tenantID, platform.ID(entityID), model
		e.Status, e.LastError, e.UpdatedAt = knowledge.EmbedFailed, lastError.String, updated.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *KnowledgeStore) RetryEmbeddingFailures(ctx context.Context, tenantID platform.ID, model string, at time.Time) (int, error) {
	res, err := s.ex.ExecContext(ctx,
		`UPDATE embeddings
		 SET status = ?, attempts = 0, last_error = NULL, updated_at = ?
		 WHERE tenant_id = ? AND model = ? AND status = ?`,
		knowledge.EmbedPending, at.UTC(), string(tenantID), model, knowledge.EmbedFailed)
	if err != nil {
		return 0, fmt.Errorf("mysql: 重试 embedding 失败项: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mysql: 读取 embedding 重试数量: %w", err)
	}
	return int(n), nil
}

// LoadVectors 批量取已算好的向量（两段式检索的第二段）。
//
// 按 entity_type 分组查询，让每条 SQL 都能走 uk_emb (entity_type, entity_id, model)
// 的索引前缀。写成 `(entity_type, entity_id) IN ((?,?),...)` 的行构造器形式
// 也能跑，但 MySQL 对它的索引利用没有保证——一次检索的热路径不值得赌这个。
//
// 不带空间过滤：refs 全部来自第一段粗筛，已经过过权限了。在这里再滤一次
// 意味着权限规则有了第二个可以走的入口，而它迟早会和第一个不一致。
func (s *KnowledgeStore) LoadVectors(ctx context.Context, tenantID platform.ID, model string, refs []knowledge.Ref) (map[string][]float32, error) {
	byType := map[string][]platform.ID{}
	for _, r := range refs {
		byType[r.Type] = append(byType[r.Type], r.ID)
	}

	out := map[string][]float32{}
	for entityType, ids := range byType {
		args := []any{string(tenantID), model, entityType, knowledge.EmbedReady}
		args = append(args, idArgs(ids)...)

		rows, err := s.ex.QueryContext(ctx,
			`SELECT entity_id, vector FROM embeddings
			 WHERE tenant_id = ? AND model = ? AND entity_type = ? AND status = ?
			   AND entity_id IN (`+placeholders(len(ids))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("mysql: 查询向量: %w", err)
		}
		if err := scanVectors(rows, entityType, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanVectors 单独一个函数，是为了让 rows.Close 在每一轮循环结束时
// 确定发生——在 for 循环体里写 defer 会把它们全部堆到函数返回时。
func scanVectors(rows *sql.Rows, entityType string, out map[string][]float32) error {
	defer rows.Close()
	for rows.Next() {
		var (
			id  string
			raw []byte
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return fmt.Errorf("mysql: 读取向量行: %w", err)
		}
		if v := decodeVector(raw); v != nil {
			out[knowledge.Ref{Type: entityType, ID: platform.ID(id)}.String()] = v
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("mysql: 遍历向量行: %w", err)
	}
	return nil
}
