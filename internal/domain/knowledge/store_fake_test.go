package knowledge

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"claracore-server/internal/platform"
)

// fakeStore 是领域测试用的内存 Store。
//
// 它必须是**真库行为的基准**，不是一个方便的 map 包装。0001 那一轮的教训：
// 早期的 governance fake 在账号重名时直接 map 赋值把原用户顶掉，而真库会拒绝，
// 于是领域测试一直跑在数据库根本不可能达到的状态上。
//
// 因此这里刻意模拟了四件真库才有的事：
//   - memories/cards 表**没有** labels 与 triggers 列，存进来时必须剥掉，
//     取出来时从各自的表填回（否则测不出"忘了写 labels 表"这类 bug）
//   - 乐观锁：version 不匹配即 ErrVersionConflict
//   - links 的唯一键冲突返回 ErrAlreadyExists
//   - Atomically 失败时整体回滚
type fakeStore struct {
	memories map[platform.ID]Memory
	cards    map[platform.ID]Card
	labels   map[string][]string             // key: Ref.String()
	triggers map[platform.ID][]RecallTrigger // key: cardID
	links    map[platform.ID]Link

	// embeddings 的键模拟 uk_emb (entity_type, entity_id, model)。
	embeddings map[string]Embedding

	// failOn 让测试注入一次存储故障，用于验证 Atomically 真的回滚。
	// 值是方法名，如 "ReplaceLabels"。
	failOn string
	inTx   bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		memories:   map[platform.ID]Memory{},
		cards:      map[platform.ID]Card{},
		labels:     map[string][]string{},
		triggers:   map[platform.ID][]RecallTrigger{},
		links:      map[platform.ID]Link{},
		embeddings: map[string]Embedding{},
	}
}

var errInjected = fmt.Errorf("fake: 注入的存储故障")

func (f *fakeStore) check(method string) error {
	if f.failOn == method {
		return errInjected
	}
	return nil
}

// --- Memory ---

func (f *fakeStore) CreateMemory(_ context.Context, m Memory) error {
	if err := f.check("CreateMemory"); err != nil {
		return err
	}
	if _, dup := f.memories[m.ID]; dup {
		return ErrAlreadyExists
	}
	m.Labels = nil // 真库的 memories 表没有这一列
	f.memories[m.ID] = m
	return nil
}

func (f *fakeStore) FindMemory(_ context.Context, tenantID, id platform.ID, spaces []platform.ID) (*Memory, error) {
	if err := f.check("FindMemory"); err != nil {
		return nil, err
	}
	m, ok := f.memories[id]
	// 租户与空间过滤都下推在这里，对应 SQL 的 WHERE tenant_id = ? AND space_id IN (...)
	if !ok || m.TenantID != tenantID || !slices.Contains(spaces, m.SpaceID) {
		return nil, ErrNotFound
	}
	m.Labels = slices.Clone(f.labels[Ref{EntityMemory, id}.String()])
	return &m, nil
}

func (f *fakeStore) UpdateMemory(_ context.Context, m Memory, expectedVersion int) error {
	if err := f.check("UpdateMemory"); err != nil {
		return err
	}
	cur, ok := f.memories[m.ID]
	if !ok || cur.TenantID != m.TenantID {
		return ErrNotFound
	}
	if cur.Version != expectedVersion {
		return ErrVersionConflict
	}
	m.Labels = nil
	f.memories[m.ID] = m
	return nil
}

// --- Card ---

func (f *fakeStore) CreateCard(_ context.Context, c Card) error {
	if err := f.check("CreateCard"); err != nil {
		return err
	}
	if _, dup := f.cards[c.ID]; dup {
		return ErrAlreadyExists
	}
	c.Labels, c.RecallTriggers = nil, nil // 真库的 cards 表没有这两列
	f.cards[c.ID] = c
	return nil
}

func (f *fakeStore) FindCard(_ context.Context, tenantID, id platform.ID, spaces []platform.ID) (*Card, error) {
	if err := f.check("FindCard"); err != nil {
		return nil, err
	}
	c, ok := f.cards[id]
	if !ok || c.TenantID != tenantID || !slices.Contains(spaces, c.SpaceID) {
		return nil, ErrNotFound
	}
	c.Labels = slices.Clone(f.labels[Ref{EntityCard, id}.String()])
	c.RecallTriggers = slices.Clone(f.triggers[id])
	return &c, nil
}

func (f *fakeStore) UpdateCard(_ context.Context, c Card, expectedVersion int) error {
	if err := f.check("UpdateCard"); err != nil {
		return err
	}
	cur, ok := f.cards[c.ID]
	if !ok || cur.TenantID != c.TenantID {
		return ErrNotFound
	}
	if cur.Version != expectedVersion {
		return ErrVersionConflict
	}
	c.Labels, c.RecallTriggers = nil, nil
	f.cards[c.ID] = c
	return nil
}

// --- 多态附属物 ---

func (f *fakeStore) ReplaceLabels(_ context.Context, _ platform.ID, ref Ref, labels []string, _ time.Time) error {
	if err := f.check("ReplaceLabels"); err != nil {
		return err
	}
	if len(labels) == 0 {
		delete(f.labels, ref.String())
		return nil
	}
	f.labels[ref.String()] = slices.Clone(labels)
	return nil
}

func (f *fakeStore) ReplaceRecallTriggers(_ context.Context, _, cardID platform.ID, triggers []RecallTrigger) error {
	if err := f.check("ReplaceRecallTriggers"); err != nil {
		return err
	}
	if len(triggers) == 0 {
		delete(f.triggers, cardID)
		return nil
	}
	f.triggers[cardID] = slices.Clone(triggers)
	return nil
}

// --- Link ---

func linkKey(from, to Ref, kind string) string {
	return from.String() + "->" + to.String() + "#" + kind
}

func (f *fakeStore) CreateLink(_ context.Context, l Link) error {
	if err := f.check("CreateLink"); err != nil {
		return err
	}
	// uk_links (from_type, from_id, to_type, to_id, kind)
	for _, existing := range f.links {
		if linkKey(existing.From, existing.To, existing.Kind) == linkKey(l.From, l.To, l.Kind) {
			return ErrAlreadyExists
		}
	}
	f.links[l.ID] = l
	return nil
}

func (f *fakeStore) DeleteLink(_ context.Context, tenantID platform.ID, from, to Ref, kind string) error {
	if err := f.check("DeleteLink"); err != nil {
		return err
	}
	for id, l := range f.links {
		if l.TenantID == tenantID && linkKey(l.From, l.To, l.Kind) == linkKey(from, to, kind) {
			delete(f.links, id)
		}
	}
	return nil // 幂等：本就不存在也返回 nil
}

func (f *fakeStore) ListLinks(_ context.Context, tenantID platform.ID, ref Ref) ([]Link, error) {
	if err := f.check("ListLinks"); err != nil {
		return nil, err
	}
	var out []Link
	for _, l := range f.links {
		if l.TenantID == tenantID && (l.From == ref || l.To == ref) {
			out = append(out, l)
		}
	}
	slices.SortFunc(out, func(a, b Link) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out, nil
}

// --- 事务 ---

func (f *fakeStore) Atomically(ctx context.Context, fn func(Store) error) error {
	if f.inTx {
		// 已在事务中则复用，与 MySQL 实现一致（无真嵌套事务）
		return fn(f)
	}
	snapshot := f.clone()
	f.inTx = true
	defer func() { f.inTx = false }()

	if err := fn(f); err != nil {
		f.restore(snapshot)
		return err
	}
	return nil
}

func (f *fakeStore) clone() *fakeStore {
	c := newFakeStore()
	maps.Copy(c.memories, f.memories)
	maps.Copy(c.cards, f.cards)
	maps.Copy(c.links, f.links)
	maps.Copy(c.embeddings, f.embeddings)
	// slice 值必须逐个 Clone：直接 maps.Copy 会让快照与原件共享底层数组，
	// 回滚就成了空操作——而这个 fake 存在的意义正是让回滚能被测到。
	for k, v := range f.labels {
		c.labels[k] = slices.Clone(v)
	}
	for k, v := range f.triggers {
		c.triggers[k] = slices.Clone(v)
	}
	return c
}

func (f *fakeStore) restore(s *fakeStore) {
	f.memories, f.cards, f.labels, f.triggers, f.links = s.memories, s.cards, s.labels, s.triggers, s.links
	f.embeddings = s.embeddings
}
