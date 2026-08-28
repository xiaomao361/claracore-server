package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/platform"
)

// EntityCardTrigger 是只有 embeddings 表会用到的第三种实体类型。
//
// 它刻意不进 Ref.Validate() 允许的集合：那个方法回答的是「能不能作为
// 标签或关联的目标」，而召回条件不是——给一条触发条件打标签、或者把它
// 关联到另一条记忆，都没有意义。同一个 Ref 类型在两处有不同的合法集合，
// 是因为它们本来就是两个问题。
const EntityCardTrigger = "card_trigger"

// embedding 行的状态。
//
// pending 与 failed 都还在队列里，区别只是 failed 带着上次的原因和重试计数。
// 不合并成一个状态：运维要能一眼看出「还没轮到」和「试过但不行」，
// 后者意味着 embedding 服务或模型有问题，前者只说明队列有积压。
const (
	EmbedPending = "pending"
	EmbedReady   = "ready"
	EmbedFailed  = "failed"
)

const (
	// MaxEmbedTextLen 是送去算向量的文本上限（按 rune）。
	//
	// embedding 模型本身有上下文上限，超出的部分不会被"平均"进向量里，
	// 而是被直接丢弃。与其让模型静默截断、我们连算进去的是哪一段都说不清，
	// 不如在这里显式截断，并把截断后的文本原样存进 embeddings.source_text。
	MaxEmbedTextLen = 2000

	// maxEmbedAttempts 是连续失败多少次之后不再重试。
	//
	// 不重试不等于丢弃：行还在，状态是 failed，last_error 写着原因。
	// 无限重试会让一条注定失败的内容（比如超出模型限制的畸形文本）
	// 永远占着每一批的名额，把整个队列拖死。
	maxEmbedAttempts = 5

	// maxErrorLen 是写进 last_error 的长度上限，与列宽一致。
	maxErrorLen = 512

	// defaultIndexBatch 是一批处理多少条。批太大时一次外部调用失败会
	// 让一大批一起进重试队列，批太小则每条都摊一次 HTTP 往返。
	defaultIndexBatch = 32
	maxIndexBatch     = 256
)

// Embedding 是一条内容在某个模型下的向量，同时也是**索引队列的一行**。
//
// 两个身份合在一张表上是有意的：写入路径不能依赖外部 embedding 服务
// （Ollama 挂了照样要能记东西），所以写入事务里落一行 pending，向量后台补。
// 单独再建一张 jobs 表的话，「这条内容有没有向量」就要查两个地方，
// 而两个地方迟早会不一致。
type Embedding struct {
	ID       platform.ID
	TenantID platform.ID
	Ref      Ref
	Model    string

	// ContentHash 是 SourceText 的 SHA-256。入队时比对它就能回答
	// 「内容变了没有」——没变就不必重算，那是一次外部调用的钱。
	ContentHash string
	SourceText  string

	Vector    []float32
	Status    string
	Attempts  int
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// EmbeddingStats 是治理面使用的队列口径。Exhausted 是 Failed 中已达到
// 自动重试上限的子集，不与 Failed 相加。
type EmbeddingStats struct {
	Pending   int
	Ready     int
	Failed    int
	Exhausted int
}

type EmbeddingHealth struct {
	Enabled  bool
	Model    string
	Stats    EmbeddingStats
	Failures []Embedding
}

type BackfillReport struct {
	Enabled  bool
	Model    string
	Memories int
	Cards    int
	Triggers int
}

func (r BackfillReport) Total() int { return r.Memories + r.Cards + r.Triggers }

// validEmbedTarget 限定哪些实体可以有向量。
//
// 与 Ref.Validate() 不同：那里问的是「能不能被打标签、被关联」，
// 这里问的是「值不值得单独算一个向量」。card_trigger 只在这里合法，
// 而它恰恰是本产品检索精度的来源（docs/02-data-model.md §3.3）。
func validEmbedTarget(typ string) bool {
	switch typ {
	case EntityMemory, EntityCard, EntityCardTrigger:
		return true
	}
	return false
}

// memoryEmbedText 是一条记忆送去算向量的文本。
//
// 标题与正文都进：记忆的标题常常就是那句事实本身，丢了它等于丢了最精炼的一句。
func memoryEmbedText(m Memory) string {
	return joinSections(m.Title, m.Body)
}

// cardEmbedText 是一张卡片送去算向量的文本。
//
// **刻意不含 Original**，与 ft_cards 全文索引的取舍一致（0002 迁移）：
// 原文是未经整理的现场记录，噪音最多，纳进来只会稀释「提炼」和「理解」
// 这两栏的信号。WhyKeep / Unresolved 同理不进——它们回答的是
// 「该不该留着」和「还欠什么」，不是「这张卡在讲什么」。
//
// 注意这只是**卡片正文**的向量。card_recall 匹配的是每条召回条件各自的向量，
// 走的是另一条路（见 triggerEmbedText）。
func cardEmbedText(c Card) string {
	return joinSections(c.Title, c.Distilled, c.Understanding)
}

// triggerEmbedText 是一条召回条件的向量文本。
//
// 单独一条、不拼卡片标题：整卡一个向量会让「准备删除数据时」这个信号
// 淹没在五十行复盘细节里，而这里连卡片标题都不掺，是为了让向量
// 纯粹表达「什么情境下该想起来」——那正是 agent 传进来的东西。
func triggerEmbedText(t RecallTrigger) string {
	return strings.TrimSpace(t.Text)
}

func joinSections(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

// truncateRunes 按字符（非字节）截断。按字节截会把一个汉字切成两半，
// 留下一个无效 UTF-8 序列——它会一路传到 HTTP 请求体里才炸。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func contentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// newPendingEmbedding 构造一行待算的向量。第二个返回值为 false 表示
// 这条内容没有可 embedding 的文本（比如一张只有标题的卡片），不必入队。
func (s *Service) newPendingEmbedding(tenantID platform.ID, ref Ref, text string, at time.Time) (Embedding, bool) {
	text = truncateRunes(strings.TrimSpace(text), MaxEmbedTextLen)
	if text == "" {
		return Embedding{}, false
	}
	return Embedding{
		ID:          s.ids.New(),
		TenantID:    tenantID,
		Ref:         ref,
		Model:       s.embedder.Model(),
		ContentHash: contentHash(text),
		SourceText:  text,
		Status:      EmbedPending,
		CreatedAt:   at,
		UpdatedAt:   at,
	}, true
}

// enqueueEmbedding 把一条内容排进索引队列，**必须在写内容的同一个事务里调用**。
//
// 同事务是这套设计能成立的前提：入队与内容一起提交，就不存在
// 「内容写进去了但没人知道要给它算向量」的窗口。分两步走的话，
// 那个窗口里崩一次，这条内容就永远搜不到，而且没有任何地方会报错。
//
// 未配置 Embedder 时静默跳过：没有模型名就没有可写的 model 列，
// 攒一堆不知道该用哪个模型算的 pending 行只是垃圾。
// 代价是**后来才打开 embedding 的存量内容不会被自动补索引**，
// 见 docs/HANDOFF「这一层缺的东西」。
func (s *Service) enqueueEmbedding(ctx context.Context, st Store, tenantID platform.ID, ref Ref, text string, at time.Time) error {
	if s.embedder == nil {
		return nil
	}
	if !validEmbedTarget(ref.Type) {
		return fmt.Errorf("%w: 不可 embedding 的实体类型 %q", ErrInvalidArgument, ref.Type)
	}
	e, ok := s.newPendingEmbedding(tenantID, ref, text, at)
	if !ok {
		return nil
	}
	return st.EnqueueEmbedding(ctx, e)
}

// enqueueCardTriggers 给一张卡的全部召回条件入队。
func (s *Service) enqueueCardTriggers(ctx context.Context, st Store, c Card, at time.Time) error {
	for _, t := range c.RecallTriggers {
		ref := Ref{Type: EntityCardTrigger, ID: t.ID}
		if err := s.enqueueEmbedding(ctx, st, c.TenantID, ref, triggerEmbedText(t), at); err != nil {
			return err
		}
	}
	return nil
}

// IndexReport 是一批索引的结果，给后台 worker 打日志用。
type IndexReport struct {
	Picked  int // 本批领到多少条
	Indexed int // 成功写回向量多少条
	Failed  int // 失败多少条（已计入 attempts）
}

// IndexPending 处理一批待算的向量。由后台 worker 反复调用。
//
// 返回 error 不代表队列坏了——最常见的情况是 embedding 服务临时不可用，
// 这一批已经被标记成 failed 并累加了 attempts，下一轮会重试。
// 调用方该做的是记日志然后等下一个 tick，不是退出。
func (s *Service) IndexPending(ctx context.Context, batch int) (IndexReport, error) {
	if s.embedder == nil {
		// 降级路径不是异常路径：没有 Embedder 就没有队列，
		// 检索退化成中文全文匹配，系统功能完整（docs/01-architecture.md §6.1）。
		return IndexReport{}, nil
	}
	batch = clampBatch(batch)

	rows, err := s.store.ListPendingEmbeddings(ctx, s.embedder.Model(), maxEmbedAttempts, batch)
	if err != nil {
		return IndexReport{}, err
	}
	if len(rows) == 0 {
		return IndexReport{}, nil
	}
	report := IndexReport{Picked: len(rows)}

	texts := make([]string, 0, len(rows))
	for _, r := range rows {
		texts = append(texts, r.SourceText)
	}

	vectors, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		report.Failed = len(rows)
		return report, s.failBatch(ctx, rows, err.Error(), err)
	}
	// 数量对不上是实现违反了 ports.Embedder 的约定。整批判失败而不是
	// 按下标硬配：错位的向量会被安到别的内容头上，那是一种不报错、
	// 只让检索悄悄变差的故障，事后几乎无法定位。
	if len(vectors) != len(rows) {
		mismatch := fmt.Errorf("embedder 返回 %d 个向量，期望 %d 个", len(vectors), len(rows))
		report.Failed = len(rows)
		return report, s.failBatch(ctx, rows, mismatch.Error(), mismatch)
	}

	now := s.clock.Now()
	var failedIDs []platform.ID
	for i, r := range rows {
		if len(vectors[i]) == 0 {
			failedIDs = append(failedIDs, r.ID)
			continue
		}
		if err := s.store.SaveEmbeddingVector(ctx, r.ID, vectors[i], now); err != nil {
			return report, err
		}
		report.Indexed++
	}
	if len(failedIDs) > 0 {
		report.Failed = len(failedIDs)
		if err := s.store.MarkEmbeddingsFailed(ctx, failedIDs, "embedder 返回了空向量", now); err != nil {
			return report, err
		}
	}
	return report, nil
}

// failBatch 把整批标记为失败并原样带回触发失败的错误。
//
// 标记失败本身再出错时返回后者：那说明数据库也有问题，
// 而"数据库连不上"比"embedding 服务连不上"是更要紧的一条消息。
func (s *Service) failBatch(ctx context.Context, rows []Embedding, reason string, cause error) error {
	ids := make([]platform.ID, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if err := s.store.MarkEmbeddingsFailed(ctx, ids, truncateRunes(reason, maxErrorLen), s.clock.Now()); err != nil {
		return err
	}
	return cause
}

func clampBatch(n int) int {
	switch {
	case n <= 0:
		return defaultIndexBatch
	case n > maxIndexBatch:
		return maxIndexBatch
	}
	return n
}

// BackfillEmbeddings 为“后来才启用模型”或“切换模型”的存量可检索内容补队列。
// 它只补不存在的 (entity, model)，不会重置已经 ready 或已失败的行。
func (s *Service) BackfillEmbeddings(ctx context.Context, tenantID platform.ID, limit int) (BackfillReport, error) {
	if tenantID == "" {
		return BackfillReport{}, fmt.Errorf("%w: 缺少租户", ErrInvalidArgument)
	}
	if s.embedder == nil {
		return BackfillReport{}, nil
	}
	limit = clampBatch(limit)
	report := BackfillReport{Enabled: true, Model: s.embedder.Model()}
	now := s.clock.Now()
	err := s.store.Atomically(ctx, func(st Store) error {
		remaining := limit
		memories, err := st.ListMemoriesMissingEmbedding(ctx, tenantID, report.Model, remaining)
		if err != nil {
			return err
		}
		for _, m := range memories {
			if err := s.enqueueEmbedding(ctx, st, tenantID, Ref{EntityMemory, m.ID}, memoryEmbedText(m), now); err != nil {
				return err
			}
			report.Memories++
		}
		remaining -= len(memories)
		if remaining == 0 {
			return nil
		}
		cards, err := st.ListCardsMissingEmbedding(ctx, tenantID, report.Model, remaining)
		if err != nil {
			return err
		}
		for _, c := range cards {
			if err := s.enqueueEmbedding(ctx, st, tenantID, Ref{EntityCard, c.ID}, cardEmbedText(c), now); err != nil {
				return err
			}
			report.Cards++
		}
		remaining -= len(cards)
		if remaining == 0 {
			return nil
		}
		triggers, err := st.ListTriggersMissingEmbedding(ctx, tenantID, report.Model, remaining)
		if err != nil {
			return err
		}
		for _, trigger := range triggers {
			if err := s.enqueueEmbedding(ctx, st, tenantID, Ref{EntityCardTrigger, trigger.ID}, triggerEmbedText(trigger), now); err != nil {
				return err
			}
			report.Triggers++
		}
		return nil
	})
	return report, err
}

func (s *Service) EmbeddingHealth(ctx context.Context, tenantID platform.ID, failureLimit int) (EmbeddingHealth, error) {
	if tenantID == "" {
		return EmbeddingHealth{}, fmt.Errorf("%w: 缺少租户", ErrInvalidArgument)
	}
	if s.embedder == nil {
		return EmbeddingHealth{}, nil
	}
	model := s.embedder.Model()
	stats, err := s.store.EmbeddingStats(ctx, tenantID, model, maxEmbedAttempts)
	if err != nil {
		return EmbeddingHealth{}, err
	}
	if failureLimit <= 0 || failureLimit > 100 {
		failureLimit = 20
	}
	failures, err := s.store.ListEmbeddingFailures(ctx, tenantID, model, failureLimit)
	if err != nil {
		return EmbeddingHealth{}, err
	}
	return EmbeddingHealth{Enabled: true, Model: model, Stats: stats, Failures: failures}, nil
}

func (s *Service) RetryEmbeddingFailures(ctx context.Context, tenantID platform.ID) (int, error) {
	if tenantID == "" {
		return 0, fmt.Errorf("%w: 缺少租户", ErrInvalidArgument)
	}
	if s.embedder == nil {
		return 0, nil
	}
	return s.store.RetryEmbeddingFailures(ctx, tenantID, s.embedder.Model(), s.clock.Now())
}
