package knowledge

import (
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/platform"
)

// 关联类型。取值与 Desktop 的 memoria_link 保持一致
// （docs/03-mcp-contract.md §2：共享契约不共享代码）——
// 同一个 agent 在两套系统之间切换时，语义必须是同一套。
const (
	LinkRelated     = "related"
	LinkCauses      = "causes"
	LinkEvolvedFrom = "evolved-from"
	LinkContradicts = "contradicts"
	LinkPartOf      = "part-of"
)

// Link 是一条有向关联。
//
// 方向有意义：causes / evolved-from / part-of 反过来就是另一回事。
// related 与 contradicts 语义上对称，但仍只存一行——
// 存两行意味着删除时要记得删两次，而漏删会留下一条单向的假关联。
// 需要「谁指向了我」时走 idx_links_to 反查。
type Link struct {
	ID        platform.ID
	TenantID  platform.ID
	From      Ref
	To        Ref
	Kind      string
	Note      string
	CreatedAt time.Time
}

func validLinkKind(k string) bool {
	switch k {
	case LinkRelated, LinkCauses, LinkEvolvedFrom, LinkContradicts, LinkPartOf:
		return true
	}
	return false
}

func (l *Link) validate() error {
	if l.TenantID == "" {
		return fmt.Errorf("%w: 缺少租户", ErrInvalidArgument)
	}
	if err := l.From.Validate(); err != nil {
		return err
	}
	if err := l.To.Validate(); err != nil {
		return err
	}
	// 自指关联没有任何含义，但会污染每一次「取出关联」的结果，
	// 且在图遍历里制造自环。挡在入口比在每个消费点绕开它便宜。
	if l.From == l.To {
		return fmt.Errorf("%w: 不能把内容关联到它自己", ErrInvalidArgument)
	}
	l.Kind = strings.TrimSpace(l.Kind)
	if !validLinkKind(l.Kind) {
		return fmt.Errorf("%w: 关联类型取值非法 %q（可选 related/causes/evolved-from/contradicts/part-of）",
			ErrInvalidArgument, l.Kind)
	}
	return nil
}
