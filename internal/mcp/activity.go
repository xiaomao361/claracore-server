package mcp

import (
	"context"
	"encoding/json"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// activityService 是 MCP Host 对活动域的全部需求。
//
// 审计发生在 Host 边界：只有这里同时知道「哪个已认证的 actor 调了哪个工具」
// 与「工具最终返回了什么」。领域用例不应知道 MCP 工具名，逐个 handler 埋点又会
// 让新工具有一条安静漏记的路径。
type activityService interface {
	Record(context.Context, activity.RecordInput) (activity.Event, error)
	ContextFeedbackQuality(context.Context, platform.ID, time.Duration) (activity.ContextFeedbackQuality, error)
}

const activityRecordTimeout = 2 * time.Second

// toolActivityExtractor 只从某个工具的强类型输入/输出里摘取允许进入活动流的字段。
//
// 绝不把 input/output 整体序列化：里面有 query、situation、title、body、token 等
// 私密内容。每个工具只能显式返回 ID、计数和语义检索标记这一小撮安全元数据。
type toolActivityExtractor[In, Out any] func(In, Out, error) toolActivityDetails

type toolActivityDetails struct {
	entityType string
	entityID   *platform.ID
	spaceID    *platform.ID
	metadata   map[string]any
}

// addAuditedTool 是本包注册 MCP 工具的唯一入口。
//
// 泛型保住 SDK 的输入 schema 推导与输出校验，同时让每个注册点必须给出明确的
// 保留类型、读写分类和强类型安全提取器。handler 先完整算出原结果，再 best-effort 记活动；
// 审计失败只写服务端日志，绝不把一次已经成功的工具调用改成失败。
func addAuditedTool[In, Out any](
	h *Host,
	s *sdk.Server,
	tool *sdk.Tool,
	kind string,
	access string,
	extract toolActivityExtractor[In, Out],
	handler sdk.ToolHandlerFor[In, Out],
) {
	sdk.AddTool(s, tool, func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
		result, out, callErr := handler(ctx, req, in)
		h.recordToolActivity(ctx, tool.Name, kind, access, extract(in, out, callErr), result, callErr)
		return result, out, callErr
	})
}

func (h *Host) recordToolActivity(
	ctx context.Context,
	action string,
	kind string,
	access string,
	details toolActivityDetails,
	result *sdk.CallToolResult,
	callErr error,
) {
	id, ok := identityFrom(ctx)
	if !ok {
		// 正常 HTTP 链路不可能到这里：认证中间件先把 Identity 放进 context。
		// 不改变 handler 的既有结果，让原来的 errNoIdentity / 协议错误仍是权威输出。
		h.logger.Error("MCP 活动未记录：context 中没有身份", "tool", action)
		return
	}

	outcome := activity.OutcomeSuccess
	if callErr != nil || (result != nil && result.IsError) {
		outcome = activity.OutcomeError
	}

	var agentID *platform.ID
	if id.AgentID != "" {
		v := id.AgentID
		agentID = &v
	}

	metadata := marshalToolMetadata(h, action, details.metadata)
	// 业务 handler 已经完成时，客户端断开不应该抹掉这次调用的记录。
	// 去掉请求的 cancel，但给审计写入一个很短的硬上限，避免它拖住 MCP 响应。
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activityRecordTimeout)
	defer cancel()
	_, err := h.activity.Record(recordCtx, activity.RecordInput{
		TenantID:   id.TenantID,
		UserID:     id.UserID,
		AgentID:    agentID,
		Source:     activity.SourceMCP,
		Kind:       kind,
		Access:     access,
		Outcome:    outcome,
		Action:     action,
		EntityType: details.entityType,
		EntityID:   details.entityID,
		SpaceID:    details.spaceID,
		Metadata:   metadata,
	})
	if err != nil {
		h.logger.Error("记录 MCP 活动失败", "tool", action, "error", err)
	}
}

func marshalToolMetadata(h *Host, action string, values map[string]any) json.RawMessage {
	if len(values) == 0 {
		return nil
	}
	raw, err := json.Marshal(values)
	if err != nil {
		// 提取器只应返回基础 JSON 类型；真走到这里是编程错误。仍然记录主事件，
		// 只是丢弃 metadata，不能因此改变工具结果。
		h.logger.Error("编码 MCP 活动元数据失败", "tool", action, "error", err)
		return nil
	}
	return raw
}

func entityDetails(entityType, entityID, spaceID string, metadata map[string]any) toolActivityDetails {
	var id *platform.ID
	if entityType != "" && entityID != "" {
		v := platform.ID(entityID)
		id = &v
	} else {
		// activity 域要求 entity_type 与 entity_id 同时有或同时无。
		entityType = ""
	}
	var space *platform.ID
	if spaceID != "" {
		v := platform.ID(spaceID)
		space = &v
	}
	return toolActivityDetails{
		entityType: entityType,
		entityID:   id,
		spaceID:    space,
		metadata:   metadata,
	}
}

// safeMetadataID is deliberately stricter than "non-empty": metadata can also
// originate in a failed call, before the knowledge domain has validated it.
// Keep only the fixed-width ASCII shape used by persisted platform IDs.
func safeMetadataID(raw string) (string, bool) {
	if len(raw) != 26 {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return "", false
		}
	}
	return raw, true
}

func auditWhoAmI(_ whoamiInput, out whoamiOutput, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	return toolActivityDetails{metadata: map[string]any{"spaceCount": len(out.Spaces)}}
}

func auditServerStatus(_ serverStatusInput, out serverStatusOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	return toolActivityDetails{metadata: map[string]any{"semanticSearch": out.SemanticSearch}}
}

func auditGatewayAutoContext(_ gatewayAutoContextInput, out gatewayAutoContextOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	return toolActivityDetails{metadata: map[string]any{"decision": out.Decision, "semanticAvailable": out.SemanticAvailable, "policyAvailable": out.PolicyAvailable}}
}

func auditGatewayContextFeedback(in gatewayContextFeedbackInput, out gatewayContextFeedbackOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	return entityDetails("card", in.CardID, "", map[string]any{"feedbackOutcome": out.Outcome})
}

func auditContextQualityStatus(_ contextQualityStatusInput, out contextQualityStatusOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	return toolActivityDetails{metadata: map[string]any{"submittedFeedbackCount": out.Submitted}}
}

func auditSharedLineStatus(in sharedLineStatusInput, _ resumePacketOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	return entityDetails("shared_line", in.LineID, "", map[string]any{"status": in.Status})
}

func auditMemoriaCreate(_ memoriaCreateInput, out memoryOut, err error) toolActivityDetails {
	return auditMemoryOutput(out, err)
}

func auditMemoriaGet(_ memoriaGetInput, out memoryOut, err error) toolActivityDetails {
	return auditMemoryOutput(out, err)
}

func auditMemoriaUpdate(_ memoriaUpdateInput, out memoryOut, err error) toolActivityDetails {
	return auditMemoryOutput(out, err)
}

func auditMemoryOutput(out memoryOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	return entityDetails(knowledge.EntityMemory, out.ID, out.Space.ID, map[string]any{
		"labelCount": len(out.Labels),
		"linkCount":  len(out.Links),
	})
}

func auditMemoriaDelete(in memoriaDeleteInput, out deleteOut, err error) toolActivityDetails {
	if err != nil {
		return entityDetails(knowledge.EntityMemory, in.ID, "", nil)
	}
	// DeleteMemory 直接返回完成软删除的 Memory，因此这里能使用领域层
	// 已经按写权限解析过的空间，不需要二次查询或猜测。
	return entityDetails(knowledge.EntityMemory, in.ID, string(out.spaceID), nil)
}

func auditMemoriaSearch(_ memoriaSearchInput, out memoriaSearchOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	ids := make([]string, 0, len(out.Hits))
	semanticHits := 0
	for _, hit := range out.Hits {
		if id, ok := safeMetadataID(hit.ID); ok {
			ids = append(ids, id)
		}
		if hit.Semantic {
			semanticHits++
		}
	}
	return toolActivityDetails{metadata: map[string]any{
		"hitIds":            ids,
		"hitCount":          len(out.Hits),
		"semanticHitCount":  semanticHits,
		"semanticAvailable": out.SemanticAvailable,
		"cardMatches":       out.CardMatches,
	}}
}

func auditMemoriaLink(in memoriaLinkInput, _ linkOut, _ error) toolActivityDetails {
	// 关联没有可稳定复用的单一 entity；只记两端 ID。Note/Kind 不进 metadata，
	// 避免把用户写下的解释或未来新增的自由文本枚举带进活动流。
	metadata := make(map[string]any, 2)
	if id, ok := safeMetadataID(in.FromID); ok {
		metadata["fromId"] = id
	}
	if id, ok := safeMetadataID(in.ToID); ok {
		metadata["toId"] = id
	}
	return toolActivityDetails{metadata: metadata}
}

func auditCardCreate(_ cardCreateInput, out cardOut, err error) toolActivityDetails {
	return auditCardOutput(out, err)
}

func auditCardGet(_ cardGetInput, out cardOut, err error) toolActivityDetails {
	return auditCardOutput(out, err)
}

func auditCardUpdate(_ cardUpdateInput, out cardOut, err error) toolActivityDetails {
	return auditCardOutput(out, err)
}

func auditCardOutput(out cardOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	return entityDetails(knowledge.EntityCard, out.ID, out.Space.ID, map[string]any{
		"labelCount":         len(out.Labels),
		"linkCount":          len(out.Links),
		"recallTriggerCount": len(out.RecallTriggers),
	})
}

func auditCardSearch(_ cardSearchInput, out cardSearchOut, err error) toolActivityDetails {
	return auditCardHits(out, err)
}

func auditCardRecall(_ cardRecallInput, out cardSearchOut, err error) toolActivityDetails {
	return auditCardHits(out, err)
}

func auditCardHits(out cardSearchOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	ids := make([]string, 0, len(out.Hits))
	semanticHits := 0
	for _, hit := range out.Hits {
		if id, ok := safeMetadataID(hit.ID); ok {
			ids = append(ids, id)
		}
		if hit.Semantic {
			semanticHits++
		}
	}
	return toolActivityDetails{metadata: map[string]any{
		"hitIds":            ids,
		"hitCount":          len(out.Hits),
		"semanticHitCount":  semanticHits,
		"semanticAvailable": out.SemanticAvailable,
	}}
}

func auditSpaceList(_ spaceListInput, out spaceListOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	ids := make([]string, 0, len(out.Spaces))
	for _, space := range out.Spaces {
		if id, ok := safeMetadataID(space.ID); ok {
			ids = append(ids, id)
		}
	}
	return toolActivityDetails{metadata: map[string]any{
		"spaceIds":   ids,
		"spaceCount": len(out.Spaces),
	}}
}

func auditPromote(in promoteInput, out promoteOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	metadata := make(map[string]any, 1)
	if id, ok := safeMetadataID(in.EntityID); ok {
		metadata["sourceEntityId"] = id
	}
	return entityDetails(out.Type, out.ID, out.Space.ID, metadata)
}

func auditSpaceStats(_ spaceStatsInput, out spaceStatsOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	ids := make([]string, 0, len(out.Spaces))
	for _, stat := range out.Spaces {
		if id, ok := safeMetadataID(stat.Space.ID); ok {
			ids = append(ids, id)
		}
	}
	return toolActivityDetails{metadata: map[string]any{
		"spaceIds":   ids,
		"spaceCount": len(out.Spaces),
	}}
}

func auditSharedLineList(_ sharedLineListInput, out sharedLineListOut, err error) toolActivityDetails {
	if err != nil {
		return toolActivityDetails{}
	}
	ids := make([]string, 0, len(out.Lines))
	for _, line := range out.Lines {
		if id, ok := safeMetadataID(line.ID); ok {
			ids = append(ids, id)
		}
	}
	return toolActivityDetails{metadata: map[string]any{"lineIds": ids, "lineCount": len(out.Lines)}}
}

func auditSharedLineGet(in sharedLineGetInput, out resumePacketOut, err error) toolActivityDetails {
	return auditSharedLinePacket(in.LineID, out, err)
}

func auditSharedLineUpdate(in sharedLineUpdateInput, out resumePacketOut, err error) toolActivityDetails {
	return auditSharedLinePacket(in.LineID, out, err)
}

func auditSharedLineHandoff(in sharedLineHandoffInput, out resumePacketOut, err error) toolActivityDetails {
	return auditSharedLinePacket(in.LineID, out, err)
}

func auditSharedLinePacket(requestedID string, out resumePacketOut, err error) toolActivityDetails {
	id := out.Line.ID
	if err != nil {
		id = requestedID
	}
	metadata := map[string]any{}
	if err == nil && out.Line.Position != nil {
		metadata["positionVersion"] = out.Line.Position.Version
		metadata["factCount"] = len(out.Line.Position.FactsUsed)
	}
	if err == nil {
		metadata["hasHandoff"] = out.RecentHandoff != nil
	}
	return entityDetails("shared_line", id, "", metadata)
}
