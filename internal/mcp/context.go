package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/domain/continuity"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

const (
	gatewayAutoContextTimeout = 15 * time.Second
	contextQualityWindow      = 30 * 24 * time.Hour
	contextPrimaryTextRunes   = 2000
	contextSecondaryTextRunes = 1200
)

type gatewayAutoContextInput struct {
	Prompt string `json:"prompt,omitempty" jsonschema:"当前用户消息或完整工作情境。未指定 lineId 时必传，Memory 与 Card 各自经过保守门禁并竞争唯一上下文位。"`
	LineID string `json:"lineId,omitempty" jsonschema:"调用方明确知道当前 Shared Line 时传入。精确 line 优先，系统不会从多条 line 中猜测。"`
}

type gatewayContextFeedbackInput struct {
	Domain    string `json:"domain,omitempty" jsonschema:"knowledge_card | memory | shared_line。兼容旧调用时可省略并传 cardId。"`
	ID        string `json:"id,omitempty" jsonschema:"自动上下文块的实体 ID"`
	CardID    string `json:"cardId,omitempty" jsonschema:"兼容旧调用的知识卡 ID；新调用使用 domain + id"`
	ContextID string `json:"contextId,omitempty" jsonschema:"gateway_auto_context 交付时返回的匿名关联 ID。旧客户端可省略；省略后反馈仍记录，但不计入选择覆盖。"`
	Outcome   string `json:"outcome" jsonschema:"delivered | used | ignored | wrong | corrected"`
}
type gatewayContextFeedbackOut struct {
	Recorded  bool   `json:"recorded"`
	Domain    string `json:"domain"`
	ID        string `json:"id"`
	CardID    string `json:"cardId,omitempty"`
	ContextID string `json:"contextId,omitempty"`
	Outcome   string `json:"outcome"`
}

type contextQualityStatusInput struct{}

type contextQualityStatusOut struct {
	WindowDays     int                      `json:"windowDays"`
	Since          string                   `json:"since"`
	Submitted      int                      `json:"submitted"`
	Selected       int                      `json:"selected"`
	Abstained      int                      `json:"abstained"`
	AbstainReasons contextAbstainReasonsOut `json:"abstainReasons"`
	Covered        int                      `json:"feedbackCoveredSelections"`
	Delivered      int                      `json:"delivered"`
	Used           int                      `json:"used"`
	Ignored        int                      `json:"ignored"`
	Wrong          int                      `json:"wrong"`
	Corrected      int                      `json:"corrected"`
	Caveat         string                   `json:"caveat"`
}

type contextAbstainReasonsOut struct {
	NoRelevantEvidence  int `json:"noRelevantEvidence"`
	Ambiguous           int `json:"ambiguous"`
	SemanticUnavailable int `json:"semanticUnavailable"`
	RetrievalTimeout    int `json:"retrievalTimeout"`
	SharedLineNotReady  int `json:"sharedLineNotReady"`
	Other               int `json:"other"`
}

func (h *Host) handleContextQualityStatus(ctx context.Context, _ *sdk.CallToolRequest, _ contextQualityStatusInput) (*sdk.CallToolResult, contextQualityStatusOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, contextQualityStatusOut{}, toolError(h.logger, "context_quality_status", err)
	}
	quality, err := h.activity.ContextFeedbackQuality(ctx, r.identity.TenantID, contextQualityWindow)
	if err != nil {
		return nil, contextQualityStatusOut{}, toolError(h.logger, "context_quality_status", err)
	}
	return nil, contextQualityStatusOut{
		WindowDays: 30,
		Since:      quality.Since.Format(time.RFC3339),
		Submitted:  quality.Total(),
		Selected:   quality.Selected,
		Abstained:  quality.Abstained,
		AbstainReasons: contextAbstainReasonsOut{
			NoRelevantEvidence:  quality.AbstainReasons.NoRelevantEvidence,
			Ambiguous:           quality.AbstainReasons.Ambiguous,
			SemanticUnavailable: quality.AbstainReasons.SemanticUnavailable,
			RetrievalTimeout:    quality.AbstainReasons.RetrievalTimeout,
			SharedLineNotReady:  quality.AbstainReasons.SharedLineNotReady,
			Other:               quality.AbstainReasons.Other,
		},
		Covered:   quality.FeedbackCoveredSelections,
		Delivered: quality.Delivered,
		Used:      quality.Used,
		Ignored:   quality.Ignored,
		Wrong:     quality.Wrong,
		Corrected: quality.Corrected,
		Caveat:    "abstainReasons 是固定的运行与语料治理分类；它、feedbackCoveredSelections 与主动提交的 outcome 都不是自动上下文准确率。覆盖只计与本窗口真实选择关联的去重 contextId，旧客户端反馈不计覆盖。",
	}, nil
}

func (h *Host) handleGatewayContextFeedback(ctx context.Context, _ *sdk.CallToolRequest, in gatewayContextFeedbackInput) (*sdk.CallToolResult, gatewayContextFeedbackOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, gatewayContextFeedbackOut{}, toolError(h.logger, "gateway_context_feedback", err)
	}
	outcome := strings.TrimSpace(in.Outcome)
	switch outcome {
	case "delivered", "used", "ignored", "wrong", "corrected":
	default:
		return nil, gatewayContextFeedbackOut{}, toolError(h.logger, "gateway_context_feedback", fmt.Errorf("%w: outcome 非法", knowledge.ErrInvalidArgument))
	}
	contextID, err := normalizeContextID(in.ContextID)
	if err != nil {
		return nil, gatewayContextFeedbackOut{}, toolError(h.logger, "gateway_context_feedback", err)
	}
	domain, entityID := strings.TrimSpace(in.Domain), strings.TrimSpace(in.ID)
	if domain == "" && strings.TrimSpace(in.CardID) != "" {
		domain, entityID = "knowledge_card", strings.TrimSpace(in.CardID)
	}
	if entityID == "" && domain == "knowledge_card" {
		entityID = strings.TrimSpace(in.CardID)
	}
	if entityID == "" {
		return nil, gatewayContextFeedbackOut{}, toolError(h.logger, "gateway_context_feedback", fmt.Errorf("%w: 缺少自动上下文实体 ID", knowledge.ErrInvalidArgument))
	}
	switch domain {
	case "knowledge_card", "memory":
		spaces, err := r.readableIDs(nil)
		if err != nil {
			return nil, gatewayContextFeedbackOut{}, toolError(h.logger, "gateway_context_feedback", err)
		}
		if domain == "knowledge_card" {
			_, _, err = h.know.GetCard(ctx, r.identity.TenantID, platform.ID(entityID), spaces)
		} else {
			_, _, err = h.know.GetMemory(ctx, r.identity.TenantID, platform.ID(entityID), spaces)
		}
		if err != nil {
			return nil, gatewayContextFeedbackOut{}, toolError(h.logger, "gateway_context_feedback", err)
		}
	case "shared_line":
		if _, err := h.continuity.Get(ctx, continuity.GetInput{TenantID: r.identity.TenantID, AgentID: r.identity.AgentID, LineID: platform.ID(entityID)}); err != nil {
			return nil, gatewayContextFeedbackOut{}, toolError(h.logger, "gateway_context_feedback", err)
		}
	default:
		return nil, gatewayContextFeedbackOut{}, toolError(h.logger, "gateway_context_feedback", fmt.Errorf("%w: domain 非法", knowledge.ErrInvalidArgument))
	}
	out := gatewayContextFeedbackOut{Recorded: true, Domain: domain, ID: entityID, ContextID: contextID, Outcome: outcome}
	if domain == "knowledge_card" {
		out.CardID = entityID
	}
	return nil, out, nil
}

type contextBlock struct {
	Domain string `json:"domain"`
	ID     string `json:"id"`
	Body   string `json:"body"`
}

type automaticMemorySelectionOut struct {
	ID       string   `json:"id"`
	Space    spaceRef `json:"space"`
	Title    string   `json:"title"`
	Score    float64  `json:"score"`
	Semantic bool     `json:"semantic"`
}

type gatewayAutoContextOut struct {
	Decision          string                       `json:"decision"`
	Reason            string                       `json:"reason"`
	ContextID         string                       `json:"contextId,omitempty"`
	SemanticAvailable bool                         `json:"semanticAvailable"`
	PolicyAvailable   bool                         `json:"policyAvailable"`
	Selected          *cardHitOut                  `json:"selected,omitempty"`
	SelectedMemory    *automaticMemorySelectionOut `json:"selectedMemory,omitempty"`
	Block             *contextBlock                `json:"block,omitempty"`
}

func (h *Host) handleGatewayAutoContext(ctx context.Context, _ *sdk.CallToolRequest, in gatewayAutoContextInput) (*sdk.CallToolResult, gatewayAutoContextOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
	}
	qualityCtx, cancel := context.WithTimeout(ctx, gatewayAutoContextTimeout)
	defer cancel()

	if lineID := strings.TrimSpace(in.LineID); lineID != "" {
		packet, err := h.continuity.Get(qualityCtx, continuity.GetInput{TenantID: r.identity.TenantID, AgentID: r.identity.AgentID, LineID: platform.ID(lineID)})
		if err != nil {
			return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
		}
		out := gatewayAutoContextOut{Decision: "abstain", Reason: "shared_line_not_ready", PolicyAvailable: true}
		if packet.Line.Status != continuity.LineActive || packet.Line.Position == nil {
			return nil, out, nil
		}
		position := packet.Line.Position
		body := fmt.Sprintf("[ClaraCore Shared Line — read-only current position]\nTitle: %s\nInterpretation status: %s\nSummary: %s\nNext step: %s\nInstruction: This line was explicitly scoped by the caller. Verify its position against current code, runtime, data, and user statements before acting.", packet.Line.Title, position.InterpretationStatus, boundedContextText(position.Summary, contextPrimaryTextRunes), boundedContextText(position.NextStep, contextSecondaryTextRunes))
		out.Decision, out.Reason = "deliver_one", "explicit_shared_line"
		out.Block = &contextBlock{Domain: "shared_line", ID: string(packet.Line.ID), Body: body}
		if err := assignContextID(&out); err != nil {
			return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
		}
		return nil, out, nil
	}

	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return nil, gatewayAutoContextOut{Decision: "abstain", Reason: "empty_prompt"}, nil
	}
	spaces, err := r.readTargets(nil)
	if err != nil {
		return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
	}
	memoryRes, err := h.know.RecallMemoryForContext(qualityCtx, knowledge.SearchMemoriesInput{TenantID: r.identity.TenantID, Spaces: spaces, Query: prompt})
	if err != nil {
		if qualityCtx.Err() != nil {
			return nil, gatewayAutoContextOut{Decision: "abstain", Reason: "retrieval_timeout", PolicyAvailable: false}, nil
		}
		return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
	}
	cardRes, err := h.know.RecallCardForContext(qualityCtx, knowledge.RecallCardsInput{TenantID: r.identity.TenantID, Spaces: spaces, Situation: prompt})
	if err != nil {
		if qualityCtx.Err() != nil {
			return nil, gatewayAutoContextOut{Decision: "abstain", Reason: "retrieval_timeout", PolicyAvailable: false}, nil
		}
		return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
	}
	out := gatewayAutoContextOut{
		Decision: "abstain", SemanticAvailable: memoryRes.SemanticAvailable && cardRes.SemanticAvailable,
		PolicyAvailable: memoryRes.PolicyAvailable && cardRes.PolicyAvailable,
	}
	if !out.SemanticAvailable || !out.PolicyAvailable {
		out.Reason = "semantic_unavailable"
		return nil, out, nil
	}
	if memoryRes.Hit != nil && cardRes.Hit != nil {
		out.Reason = "ambiguous_domains"
		return nil, out, nil
	}
	if cardRes.Hit != nil {
		encoded := r.encodeCardHits([]knowledge.CardHit{*cardRes.Hit})[0]
		body := fmt.Sprintf("[ClaraCore knowledge card — read-only evidence]\nTitle: %s\nMatched trigger: %s\nDistilled: %s\nUnderstanding: %s\nInstruction: The terminal Agent is the final relevance judge. Use only if relevant; verify against current code, runtime, data, and user statements.", cardRes.Hit.Card.Title, cardRes.Hit.Trigger, boundedContextText(cardRes.Hit.Card.Distilled, contextSecondaryTextRunes), boundedContextText(cardRes.Hit.Card.Understanding, contextSecondaryTextRunes))
		out.Decision, out.Reason, out.Selected = "deliver_one", cardRes.Reason, &encoded
		out.Block = &contextBlock{Domain: "knowledge_card", ID: string(cardRes.Hit.Card.ID), Body: body}
		if err := assignContextID(&out); err != nil {
			return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
		}
		return nil, out, nil
	}
	if memoryRes.Hit != nil {
		encoded := automaticMemorySelectionOut{
			ID: string(memoryRes.Hit.Memory.ID), Space: r.spaceRefOf(memoryRes.Hit.Memory.SpaceID), Title: memoryRes.Hit.Memory.Title,
			Score: memoryRes.Hit.Score, Semantic: memoryRes.Hit.Semantic,
		}
		body := fmt.Sprintf("[ClaraCore Memory — read-only evidence]\nTitle: %s\nBody: %s\nInstruction: The terminal Agent is the final relevance judge. Treat this as recalled evidence, not current truth; verify against current code, runtime, data, and user statements.", memoryRes.Hit.Memory.Title, boundedContextText(memoryRes.Hit.Memory.Body, contextPrimaryTextRunes))
		out.Decision, out.Reason, out.SelectedMemory = "deliver_one", memoryRes.Reason, &encoded
		out.Block = &contextBlock{Domain: "memory", ID: string(memoryRes.Hit.Memory.ID), Body: body}
		if err := assignContextID(&out); err != nil {
			return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
		}
		return nil, out, nil
	}
	if memoryRes.Reason == cardRes.Reason {
		out.Reason = memoryRes.Reason
	} else {
		out.Reason = "no_high_confidence_evidence"
	}
	return nil, out, nil
}

func assignContextID(out *gatewayAutoContextOut) error {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("生成自动上下文关联 ID: %w", err)
	}
	out.ContextID = hex.EncodeToString(raw[:])
	return nil
}

func normalizeContextID(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if len(value) != 32 {
		return "", fmt.Errorf("%w: contextId 非法", knowledge.ErrInvalidArgument)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("%w: contextId 非法", knowledge.ErrInvalidArgument)
	}
	return strings.ToLower(value), nil
}

func boundedContextText(raw string, maxRunes int) string {
	text := strings.TrimSpace(raw)
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "… [truncated]"
}
