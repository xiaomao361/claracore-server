package knowledge

import (
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/platform"
)

// Card 的状态（2026-08-07 拍板，三态）。
//
// 原设计是 draft / verifying / confirmed / deprecated 四态，砍掉中间两个的理由：
// 0806 已拍板「能写就能审、无独立审核实体、作者自管」，
// 没有任何机制会推动一张卡从 draft 流转到 verifying 再到 confirmed。
// 留着它们只会得到两个永远查不到的枚举值——而死枚举比没有枚举更糟，
// 它会让后来的人以为存在一套并不存在的审核流程。
const (
	// CardDraft 是摄取产生的默认状态，不进默认检索，需人过一遍。
	CardDraft = "draft"
	// CardActive 是手写创建的默认状态：作者自管，直接可检索。
	CardActive = "active"
	// CardDeprecated 表示已过时。不删除——「这条约定曾经成立过」本身是信息，
	// 检索时降权而非消失，否则团队会重复踩同一个已被否决的方案。
	CardDeprecated = "deprecated"
)

// Card 是知识卡片：本产品的核心载体。
//
// 五个 body_* section 落成独立字段而非一个 map：Understanding 是质量分界线，
// 它必须能被单独查询和统计（docs/02-data-model.md §3.2）。
type Card struct {
	ID            platform.ID
	TenantID      platform.ID
	SpaceID       platform.ID
	AuthorUserID  platform.ID
	AuthorAgentID *platform.ID
	Title         string
	CardType      string
	Status        string
	Source        string

	// SourceKey / SourceHash 只有摄取产物才有（手写卡片为空）。
	//
	// SourceKey 是源的稳定标识，落进 uk_cards_source (space_id, source_key)——
	// 幂等靠数据库保证，不靠应用层先查一次（并发下必然失效）。
	// SourceHash 是源内容的指纹：没变就整张卡都不动，
	// 否则每次重扫都会推高全部 version 并作废全部向量。
	SourceKey  string
	SourceHash string

	Original      string // 作者原意
	Distilled     string // 提炼
	Understanding string // 理解（空 = 只是摘要，非合格卡片）
	WhyKeep       string // 为什么值得保留
	Unresolved    string // 未解决问题

	CopiedFrom *Origin
	Version    int
	CreatedAt  time.Time
	UpdatedAt  time.Time

	RecallTriggers []RecallTrigger
	Labels         []string
}

// RecallTrigger 是作者显式写下的一条「未来什么时候该想起这张卡」。
//
// 独立成行、将来独立 embedding，是本产品相对通用向量检索的核心优势
// （docs/02-data-model.md §3.3）：整卡一个向量会让「准备删除数据时」
// 这个信号淹没在五十行复盘细节里。
type RecallTrigger struct {
	ID        platform.ID
	TenantID  platform.ID
	CardID    platform.ID
	SpaceID   platform.ID // 有意冗余，使 card_recall 粗筛无需 JOIN cards
	Text      string
	SortOrder int
	CreatedAt time.Time
}

// IsQualified 表示这是一张合格的知识卡片，而不只是一段摘要。
//
// 分界线是 Understanding 一栏（docs/02-data-model.md §3.2）：
// 复述别人说过的话谁都会，写下自己的理解才说明真的想过。
//
// **不合格不等于拒绝创建**——摄取进来的存量文档天然没有这一栏，
// 而摄取是第一版的核心能力。这个方法的用途是让「还有多少张卡只是摘要」
// 成为一个可以统计、可以催的数字，不是一道拦在写入路径上的闸。
func (c Card) IsQualified() bool { return strings.TrimSpace(c.Understanding) != "" }

// IsVisible 表示这张卡是否应出现在默认检索结果里。
// draft 不进（摄取产物需人过一遍），deprecated 不进（已过时，但仍可按 ID 取到）。
func (c Card) IsVisible() bool { return c.Status == CardActive }

func validCardStatus(s string) bool {
	switch s {
	case CardDraft, CardActive, CardDeprecated:
		return true
	}
	return false
}

func (c *Card) validate() error {
	if err := requireScope(c.TenantID, c.SpaceID, c.AuthorUserID); err != nil {
		return err
	}
	title, err := requireTitle(c.Title)
	if err != nil {
		return err
	}
	c.Title = title

	c.CardType = strings.TrimSpace(c.CardType)
	if c.CardType == "" {
		return fmt.Errorf("%w: 缺少卡片类型", ErrInvalidArgument)
	}
	if len([]rune(c.CardType)) > MaxCardTypeLen {
		return fmt.Errorf("%w: 卡片类型超过 %d 字", ErrInvalidArgument, MaxCardTypeLen)
	}
	if !validCardStatus(c.Status) {
		return fmt.Errorf("%w: 卡片状态取值非法 %q", ErrInvalidArgument, c.Status)
	}

	// 五个 section 全空的卡片没有任何内容，只是一个标题。
	// 允许它写进去，检索时就会返回一条什么都没有的结果。
	if strings.TrimSpace(c.Original+c.Distilled+c.Understanding+c.WhyKeep+c.Unresolved) == "" {
		return fmt.Errorf("%w: 卡片至少要有一个 section 有内容", ErrInvalidArgument)
	}

	labels, err := normalizeLabels(c.Labels)
	if err != nil {
		return err
	}
	c.Labels = labels
	return nil
}

// normalizeTriggers 清洗触发条件：去空白、去空项、去重，并按传入顺序重编 sort_order。
//
// 去重是必须的：同一张卡上两条一模一样的触发条件会被检索两次、
// 将来也会算两遍 embedding，而它们对召回没有任何额外贡献。
// 顺序保留传入顺序（不排序）——作者写下的先后本身是信息，
// 「最典型的场景」通常排在第一条。
func normalizeTriggers(raw []string) ([]string, error) {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if len([]rune(t)) > MaxTriggerTextLen {
			return nil, fmt.Errorf("%w: 召回条件超过 %d 字（%.20s…）", ErrInvalidArgument, MaxTriggerTextLen, t)
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out, nil
}
