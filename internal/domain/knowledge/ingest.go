package knowledge

import (
	"context"
	"errors"
	"fmt"

	"claracore-server/internal/platform"
)

// ingestCardType 是摄取产物的卡片类型。
//
// 统一一个值，让「哪些卡是机器扒进来的」在库里一眼可查。
// 人在审的时候可以改成更准确的类型——那正是审的一部分。
const ingestCardType = "摄取原料"

type IngestInput struct {
	TenantID platform.ID
	// SpaceID 已由应用层经 governance.SpaceScope.ResolveWriteTarget 解析。
	SpaceID       platform.ID
	AuthorUserID  platform.ID
	AuthorAgentID *platform.ID

	Documents []Document
}

// IngestReport 是一批摄取的结果。
//
// **保留成功项 + 列失败清单**（docs/04-web-scope.md §6 拍板）：
// 一份畸形文档不该让整批回滚——那会让「修好那一份再重跑」变成
// 「修好那一份，然后祈祷其余几千份这次也别出问题」。
type IngestReport struct {
	Created   int
	Updated   int
	Unchanged int
	Failed    []IngestFailure
}

// IngestFailure 带上源路径与原因，让人能照着去修那一份。
// 只说「失败 3 条」等于没说。
type IngestFailure struct {
	SourcePath string
	Reason     string
}

func (r IngestReport) Total() int { return r.Created + r.Updated + r.Unchanged + len(r.Failed) }

// Ingest 把一批源文档幂等地落成 draft 卡片。
//
// # 幂等的三条分支
//
//	库里没有这个 source_key      → 新建一张 draft
//	有，但 content_hash 变了      → 更新正文，其余保持
//	有，且 content_hash 没变      → **什么都不做**
//
// 第三条是最要紧的。没有它，重扫一个没改过的目录会把每张卡的 version
// 推高一格、把每个向量作废重算——那是真金白银的外部调用，而且
// 「谁在什么时候改了这张卡」的版本历史会被一堆无意义的自增淹掉。
//
// # 更新时保住人的劳动
//
// 源文档变了只覆盖 `Original`（原文那一栏）与标题。**提炼、理解、
// 召回条件、标签一律不动**——那些是 agent 和人后来加上去的，
// 是这张卡真正的价值。源文档改了一个错别字就把它们清空，
// 等于每次上游更新都惩罚一次做过功的人。
//
// 代价是原文与提炼可能不同步。这个取舍是有意的：不同步的提炼
// 还能被人看见并修正，被清空的提炼谁也想不起来它曾经存在。
func (s *Service) Ingest(ctx context.Context, in IngestInput) (IngestReport, error) {
	if err := requireScope(in.TenantID, in.SpaceID, in.AuthorUserID); err != nil {
		return IngestReport{}, err
	}

	var report IngestReport
	seen := make(map[string]bool, len(in.Documents))

	for _, doc := range in.Documents {
		if err := doc.validate(); err != nil {
			report.Failed = append(report.Failed, IngestFailure{
				SourcePath: doc.SourcePath, Reason: err.Error(),
			})
			continue
		}
		// 同一批里两份文档算出同一个 SourceKey，说明源本身有问题
		// （比如两个路径归一化后撞了）。放过去的话，它们会在同一个
		// 唯一键上互相覆盖，而最终留下哪一份取决于遍历顺序——
		// 那种「每次摄取结果都不一样」的现象极难排查。
		if seen[doc.SourceKey] {
			report.Failed = append(report.Failed, IngestFailure{
				SourcePath: doc.SourcePath,
				Reason:     fmt.Sprintf("%v: 与同批另一份文档的源标识重复", ErrAlreadyExists),
			})
			continue
		}
		seen[doc.SourceKey] = true

		outcome, err := s.ingestOne(ctx, in, doc)
		if err != nil {
			report.Failed = append(report.Failed, IngestFailure{
				SourcePath: doc.SourcePath, Reason: err.Error(),
			})
			continue
		}
		switch outcome {
		case ingestCreated:
			report.Created++
		case ingestUpdated:
			report.Updated++
		default:
			report.Unchanged++
		}
	}
	return report, nil
}

type ingestOutcome int

const (
	ingestUnchanged ingestOutcome = iota
	ingestCreated
	ingestUpdated
)

func (s *Service) ingestOne(ctx context.Context, in IngestInput, doc Document) (ingestOutcome, error) {
	existing, err := s.store.FindCardBySourceKey(ctx, in.TenantID, in.SpaceID, doc.SourceKey)
	switch {
	case errors.Is(err, ErrNotFound):
		return ingestCreated, s.createIngestedCard(ctx, in, doc)
	case err != nil:
		return ingestUnchanged, err
	}
	if err := checkTenant(in.TenantID, existing.TenantID); err != nil {
		return ingestUnchanged, err
	}

	// 源没变就一步都不走：不推 version、不重算向量、不碰更新时间
	if existing.SourceHash == doc.ContentHash {
		return ingestUnchanged, nil
	}

	title, body := doc.Title, doc.Body
	next := *existing
	next.Title = title
	next.Original = body
	next.SourceHash = doc.ContentHash
	next.Source = doc.SourcePath
	next.Version = existing.Version + 1
	next.UpdatedAt = s.clock.Now()

	if err := next.validate(); err != nil {
		return ingestUnchanged, err
	}
	err = s.store.Atomically(ctx, func(st Store) error {
		if err := st.UpdateCard(ctx, next, existing.Version); err != nil {
			return err
		}
		// 正文变了，向量要重算。走的是与手写更新完全同一条入队路径——
		// 摄取绝不自己写第二份 embedding 逻辑。
		return s.enqueueEmbedding(ctx, st, next.TenantID, Ref{EntityCard, next.ID}, cardEmbedText(next), next.UpdatedAt)
	})
	if err != nil {
		return ingestUnchanged, err
	}
	return ingestUpdated, nil
}

func (s *Service) createIngestedCard(ctx context.Context, in IngestInput, doc Document) error {
	now := s.clock.Now()
	c := Card{
		ID:            s.ids.New(),
		TenantID:      in.TenantID,
		SpaceID:       in.SpaceID,
		AuthorUserID:  in.AuthorUserID,
		AuthorAgentID: in.AuthorAgentID,
		Title:         doc.Title,
		CardType:      ingestCardType,
		// 摄取产物一律 draft：不进默认检索，需人过一遍
		// （docs/02-data-model.md §3.2）。这不是保守，是因为
		// 一张没有召回条件的卡片进了检索只会稀释结果。
		Status:     CardDraft,
		Source:     doc.SourcePath,
		SourceKey:  doc.SourceKey,
		SourceHash: doc.ContentHash,
		// 原文进 Original，其余 section 留空。提炼是 agent 的活。
		Original:  doc.Body,
		Version:   1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := c.validate(); err != nil {
		return err
	}
	// 没有召回条件——这正是「摄取产物只是原料」的字面含义。
	// persistNewCard 会把正文入队算向量，让 card_search 能找到它；
	// 但 card_recall 一条都命不中，直到有人给它写下触发条件。
	return s.persistNewCard(ctx, c, now)
}
