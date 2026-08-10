package knowledge

import (
	"context"
	"slices"
	"strings"
	"time"

	"claracore-server/internal/platform"
)

// 本文件是 fakeStore 的检索与索引队列部分。
//
// 与 store_fake_test.go 同一条纪律：**它是真库行为的基准，不是方便的 map 包装**。
// 检索这一层尤其容易写出一个"什么都对"的 fake——比如不做空间过滤、
// 或者把没有全文命中的候选直接丢掉——那样领域测试会全绿，而线上
// 要么越权要么召不回。所以这里刻意模拟了四件真库才有的事：
//
//   - 空间与租户过滤下推（对应 SQL 的 WHERE tenant_id = ? AND space_id IN (...)）
//   - 只返回默认可见状态的内容
//   - 全文分数为 0 的候选**仍然进候选集**（MATCH 只用于排序，不做 WHERE 过滤）
//   - EnqueueEmbedding 在 content_hash 未变时不重置已算好的向量

// fakeTextScore 是 MySQL ngram MATCH 相关性的替身。
//
// 不追求数值一致（那不可能，也没有意义），只保证两条性质与真库一致：
// 匹配得越多分越高、完全不匹配得 0 分。领域层只拿它排序、从不拿它当阈值，
// 所以这两条性质就是全部需要的。
func fakeTextScore(query, text string) float64 {
	q := []rune(strings.ToLower(strings.TrimSpace(query)))
	if len(q) < 2 {
		return 0
	}
	t := strings.ToLower(text)
	hit := 0
	for i := 0; i+1 < len(q); i++ {
		if strings.Contains(t, string(q[i:i+2])) {
			hit++
		}
	}
	return float64(hit) / float64(len(q)-1)
}

// hasAllLabels 模拟标签交集过滤：候选必须**同时**具有全部给定标签。
func (f *fakeStore) hasAllLabels(ref Ref, want []string) bool {
	if len(want) == 0 {
		return true
	}
	have := f.labels[ref.String()]
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// sortAndCap 复现粗筛的排序与截断：分数高的在前，同分按时间新近度，
// 最后按 ID 定序保证同样的查询返回同样的顺序。
func sortAndCap[T any](items []T, score func(T) float64, at func(T) time.Time, id func(T) platform.ID, limit int) []T {
	slices.SortFunc(items, func(a, b T) int {
		if c := compareDesc(score(a), score(b)); c != 0 {
			return c
		}
		if !at(a).Equal(at(b)) {
			if at(a).After(at(b)) {
				return -1
			}
			return 1
		}
		return compareID(id(a), id(b))
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (f *fakeStore) SearchMemories(_ context.Context, q CandidateQuery) ([]MemoryCandidate, error) {
	if err := f.check("SearchMemories"); err != nil {
		return nil, err
	}
	var out []MemoryCandidate
	for _, m := range f.memories {
		if m.TenantID != q.TenantID || !slices.Contains(q.SpaceIDs, m.SpaceID) {
			continue
		}
		if !m.IsVisible() {
			continue
		}
		if !f.hasAllLabels(Ref{EntityMemory, m.ID}, q.Labels) {
			continue
		}
		out = append(out, MemoryCandidate{
			Memory:    m,
			TextScore: fakeTextScore(q.Query, m.Title+"\n"+m.Body),
		})
	}
	return sortAndCap(out,
		func(c MemoryCandidate) float64 { return c.TextScore },
		func(c MemoryCandidate) time.Time { return c.Memory.UpdatedAt },
		func(c MemoryCandidate) platform.ID { return c.Memory.ID },
		q.Limit), nil
}

func (f *fakeStore) SearchCards(_ context.Context, q CandidateQuery) ([]CardCandidate, error) {
	if err := f.check("SearchCards"); err != nil {
		return nil, err
	}
	var out []CardCandidate
	for _, c := range f.visibleCards(q) {
		out = append(out, CardCandidate{
			Card:      c,
			TextScore: fakeTextScore(q.Query, c.Title+"\n"+c.Distilled+"\n"+c.Understanding),
		})
	}
	return sortAndCap(out,
		func(c CardCandidate) float64 { return c.TextScore },
		func(c CardCandidate) time.Time { return c.Card.UpdatedAt },
		func(c CardCandidate) platform.ID { return c.Card.ID },
		q.Limit), nil
}

func (f *fakeStore) CountCardMatches(_ context.Context, q CandidateQuery) (int, error) {
	if err := f.check("CountCardMatches"); err != nil {
		return 0, err
	}
	n := 0
	for _, c := range f.visibleCards(q) {
		// 与 SearchCards 用同一份可见性与标签条件，只是这里**要求真的有全文命中**——
		// 那个数字回答的是「另有多少条卡片命中」，把没命中的也数进去就是在骗人。
		if fakeTextScore(q.Query, c.Title+"\n"+c.Distilled+"\n"+c.Understanding) > 0 {
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) visibleCards(q CandidateQuery) []Card {
	var out []Card
	for _, c := range f.cards {
		if c.TenantID != q.TenantID || !slices.Contains(q.SpaceIDs, c.SpaceID) {
			continue
		}
		if !c.IsVisible() {
			continue
		}
		if !f.hasAllLabels(Ref{EntityCard, c.ID}, q.Labels) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (f *fakeStore) SearchRecallTriggers(_ context.Context, q CandidateQuery) ([]TriggerCandidate, error) {
	if err := f.check("SearchRecallTriggers"); err != nil {
		return nil, err
	}
	var out []TriggerCandidate
	for cardID, list := range f.triggers {
		// 对应 SQL 的 JOIN cards：所属卡片不可见时，它的召回条件也不该被召回。
		// 触发条件表上的 space_id 是冗余列，权限仍以 cards 表为准。
		c, ok := f.cards[cardID]
		if !ok || c.TenantID != q.TenantID || !slices.Contains(q.SpaceIDs, c.SpaceID) || !c.IsVisible() {
			continue
		}
		for _, t := range list {
			out = append(out, TriggerCandidate{
				Trigger:   t,
				TextScore: fakeTextScore(q.Query, t.Text),
			})
		}
	}
	return sortAndCap(out,
		func(c TriggerCandidate) float64 { return c.TextScore },
		func(c TriggerCandidate) time.Time { return c.Trigger.CreatedAt },
		func(c TriggerCandidate) platform.ID { return c.Trigger.ID },
		q.Limit), nil
}

func (f *fakeStore) FindCards(_ context.Context, tenantID platform.ID, ids, spaces []platform.ID) ([]Card, error) {
	if err := f.check("FindCards"); err != nil {
		return nil, err
	}
	var out []Card
	for _, id := range ids {
		c, ok := f.cards[id]
		if !ok || c.TenantID != tenantID || !slices.Contains(spaces, c.SpaceID) || !c.IsVisible() {
			continue // 取不到的直接不出现，不报 ErrNotFound
		}
		c.Labels = slices.Clone(f.labels[Ref{EntityCard, id}.String()])
		c.RecallTriggers = slices.Clone(f.triggers[id])
		out = append(out, c)
	}
	return out, nil
}

// --- 索引队列 ---

func embKey(ref Ref, model string) string { return ref.String() + "@" + model }

func (f *fakeStore) LoadVectors(_ context.Context, tenantID platform.ID, model string, refs []Ref) (map[string][]float32, error) {
	if err := f.check("LoadVectors"); err != nil {
		return nil, err
	}
	out := map[string][]float32{}
	for _, ref := range refs {
		e, ok := f.embeddings[embKey(ref, model)]
		if !ok || e.TenantID != tenantID || e.Status != EmbedReady || len(e.Vector) == 0 {
			continue
		}
		out[ref.String()] = slices.Clone(e.Vector)
	}
	return out, nil
}

// EnqueueEmbedding 模拟 upsert：内容未变就只碰时间戳，不动已算好的向量。
//
// 这条行为必须在 fake 里也成立。写成"每次都置回 pending"的话，
// 领域测试里「只改标签不该触发重算」会永远绿——而线上每改一次标签
// 就是一次真金白银的外部调用。
func (f *fakeStore) EnqueueEmbedding(_ context.Context, e Embedding) error {
	if err := f.check("EnqueueEmbedding"); err != nil {
		return err
	}
	key := embKey(e.Ref, e.Model)
	if cur, ok := f.embeddings[key]; ok {
		if cur.ContentHash == e.ContentHash {
			cur.UpdatedAt = e.UpdatedAt
			f.embeddings[key] = cur
			return nil
		}
		cur.ContentHash, cur.SourceText = e.ContentHash, e.SourceText
		cur.Status, cur.Vector, cur.Attempts, cur.LastError = EmbedPending, nil, 0, ""
		cur.UpdatedAt = e.UpdatedAt
		f.embeddings[key] = cur
		return nil
	}
	f.embeddings[key] = e
	return nil
}

func (f *fakeStore) DeleteCardTriggerEmbeddings(_ context.Context, tenantID, cardID platform.ID) error {
	if err := f.check("DeleteCardTriggerEmbeddings"); err != nil {
		return err
	}
	for _, t := range f.triggers[cardID] {
		for key, e := range f.embeddings {
			if e.TenantID == tenantID && e.Ref.Type == EntityCardTrigger && e.Ref.ID == t.ID {
				delete(f.embeddings, key)
			}
		}
	}
	return nil
}

func (f *fakeStore) ListPendingEmbeddings(_ context.Context, model string, maxAttempts, limit int) ([]Embedding, error) {
	if err := f.check("ListPendingEmbeddings"); err != nil {
		return nil, err
	}
	var out []Embedding
	for _, e := range f.embeddings {
		if e.Model != model || e.Status == EmbedReady || e.Attempts >= maxAttempts {
			continue
		}
		out = append(out, e)
	}
	// 最久没动的排前面：失败的行每失败一次就更新时间戳，自然排到队尾
	slices.SortFunc(out, func(a, b Embedding) int {
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			if a.UpdatedAt.Before(b.UpdatedAt) {
				return -1
			}
			return 1
		}
		return compareID(a.ID, b.ID)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) SaveEmbeddingVector(_ context.Context, id platform.ID, vector []float32, at time.Time) error {
	if err := f.check("SaveEmbeddingVector"); err != nil {
		return err
	}
	for key, e := range f.embeddings {
		if e.ID != id {
			continue
		}
		e.Vector = slices.Clone(vector)
		e.Status, e.Attempts, e.LastError = EmbedReady, 0, ""
		e.UpdatedAt = at
		f.embeddings[key] = e
		return nil
	}
	return ErrNotFound
}

func (f *fakeStore) MarkEmbeddingsFailed(_ context.Context, ids []platform.ID, reason string, at time.Time) error {
	if err := f.check("MarkEmbeddingsFailed"); err != nil {
		return err
	}
	for key, e := range f.embeddings {
		if !slices.Contains(ids, e.ID) {
			continue
		}
		e.Status, e.LastError, e.UpdatedAt = EmbedFailed, reason, at
		e.Attempts++
		f.embeddings[key] = e
	}
	return nil
}

// --- 测试用 Embedder ---

// fakeEmbedder 把文本映射成一个确定的向量，让相似度可预期。
//
// 不用随机向量：那样测试只能断言"跑通了"，断言不了"排序是对的"。
// 这里的做法是给每个关键词一个维度，向量就是关键词计数——
// 于是"共享关键词多的两段文本余弦更高"这条性质成立，
// 而那正是检索排序唯一依赖的性质。
type fakeEmbedder struct {
	model string
	words []string

	fail error // 非 nil 时每次调用都失败，用于验证降级
	// short > 0 时只返回这么多个向量，模拟实现违反 ports.Embedder
	// 「返回数量与入参一一对应」的约定。
	short int
	calls int
}

func newFakeEmbedder(words ...string) *fakeEmbedder {
	return &fakeEmbedder{model: "fake-embed-v1", words: words}
}

func (e *fakeEmbedder) Model() string { return e.model }

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	if e.fail != nil {
		return nil, e.fail
	}
	out := make([][]float32, 0, len(texts))
	for _, t := range texts {
		v := make([]float32, len(e.words))
		lower := strings.ToLower(t)
		for i, w := range e.words {
			v[i] = float32(strings.Count(lower, strings.ToLower(w)))
		}
		out = append(out, v)
	}
	if e.short > 0 && e.short < len(out) {
		out = out[:e.short]
	}
	return out, nil
}

// CountSpaceContents 与 MySQL 实现的口径必须一致，尤其是
// UnqualifiedCards——那边是 `body_understanding IS NULL`，这边是
// Card.IsQualified()。两者等价是 0002「空 section 写 NULL 而非空串」
// 换来的，有专门的测试盯着（见 TestSpaceStatsUnqualifiedMatchesIsQualified）。
func (f *fakeStore) CountSpaceContents(_ context.Context, tenantID platform.ID, spaces []platform.ID) ([]SpaceStats, error) {
	if err := f.check("CountSpaceContents"); err != nil {
		return nil, err
	}
	byID := map[platform.ID]*SpaceStats{}
	stat := func(id platform.ID) *SpaceStats {
		if st, ok := byID[id]; ok {
			return st
		}
		st := &SpaceStats{SpaceID: id}
		byID[id] = st
		return st
	}
	for _, m := range f.memories {
		if m.TenantID != tenantID || !slices.Contains(spaces, m.SpaceID) || !m.IsVisible() {
			continue
		}
		stat(m.SpaceID).Memories++
	}
	for _, c := range f.cards {
		if c.TenantID != tenantID || !slices.Contains(spaces, c.SpaceID) {
			continue
		}
		st := stat(c.SpaceID)
		st.Cards++
		if c.Status == CardDraft {
			st.DraftCards++
		}
		if !c.IsQualified() {
			st.UnqualifiedCards++
		}
		st.RecallTriggers += len(f.triggers[c.ID])
	}
	out := make([]SpaceStats, 0, len(byID))
	for _, st := range byID {
		out = append(out, *st)
	}
	return out, nil
}
