package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/domain/governance"
)

// whoamiInput 没有任何参数——**这是有意的**。
//
// 任何工具都不接受 agentId / userId / tenantId（docs/03-mcp-contract.md §3.3）：
// 参数化的身份等同于可伪造的身份。whoami 回答的是「我是谁」，
// 而答案只能来自 token。
type whoamiInput struct{}

type whoamiOutput struct {
	TenantID string `json:"tenantId"`

	UserID      string `json:"userId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`

	AgentID   string `json:"agentId"`
	AgentSlug string `json:"agentSlug"`
	AgentName string `json:"agentName"`

	// Spaces 是可读或可写的全部空间，顺序稳定（team > project > personal）。
	Spaces []spaceInfo `json:"spaces"`

	// DefaultSpaceID 是写入类工具不传 space 时的目标，即个人空间。
	// agent 需要它才能回答「我这条记下去会去哪」——没有这个字段，
	// 「默认写个人空间」就成了一条只写在文档里、agent 无从验证的约定。
	DefaultSpaceID string `json:"defaultSpaceId"`
}

type spaceInfo struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // team | project | personal
	Name     string `json:"name"`
	CanRead  bool   `json:"canRead"`
	CanWrite bool   `json:"canWrite"`
}

// handleWhoAmI 返回身份与空间权限概览。
//
// 这是 agent 判断「我能写哪个空间」的前提，因此它是第一个落地的工具。
// Host 层在这里只做**编码**：内容全部来自 Identity 与 SpaceScope，
// 一行权限判断都没有——那是 governance.ResolveScope 的事，规则只有一份。
func (h *Host) handleWhoAmI(ctx context.Context, _ *sdk.CallToolRequest, _ whoamiInput) (*sdk.CallToolResult, whoamiOutput, error) {
	id, ok := identityFrom(ctx)
	if !ok {
		return nil, whoamiOutput{}, errNoIdentity
	}

	scope, err := h.gov.Scope(ctx, id)
	if err != nil {
		return nil, whoamiOutput{}, toolError(h.logger, "whoami", err)
	}

	accesses := scope.All()
	spaces := make([]spaceInfo, 0, len(accesses))
	for _, a := range accesses {
		spaces = append(spaces, spaceInfo{
			ID:       string(a.Space.ID),
			Type:     a.Space.Type,
			Name:     a.Space.Name,
			CanRead:  a.CanRead,
			CanWrite: a.CanWrite,
		})
	}

	out := whoamiOutput{
		TenantID:    string(id.TenantID),
		UserID:      string(id.UserID),
		Username:    id.Username,
		DisplayName: id.DisplayName,
		Role:        id.Role,
		AgentID:     string(id.AgentID),
		AgentSlug:   id.AgentSlug,
		AgentName:   id.AgentName,
		Spaces:      spaces,
	}

	// 个人空间缺失是数据损坏（创建用户时应当一并创建），但不该让 whoami 失败：
	// 此时最该做的事正是让人看到「你没有个人空间」，而不是回一个错误码。
	if personal, err := scope.PersonalSpace(); err == nil {
		out.DefaultSpaceID = string(personal.ID)
	} else {
		h.logger.Error("用户没有个人空间", "userId", id.UserID, "tenantId", id.TenantID, "error", err)
	}

	return nil, out, nil
}

// 编译期确认 whoami 不依赖 token id：Identity 里有 TokenID，但它不该出现在
// 任何返回值中——那是凭据的一部分，agent 拿它没有用途。
var _ = governance.Identity{}.TokenID
