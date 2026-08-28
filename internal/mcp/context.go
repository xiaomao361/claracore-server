package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

const (
	gatewayAutoContextTimeout = 15 * time.Second
	contextQualityWindow      = 30 * 24 * time.Hour
)

type gatewayAutoContextInput struct {
	Prompt string `json:"prompt" jsonschema:"当前用户消息或完整工作情境。服务只读判定，最多交付一张知识卡。"`
}

type gatewayContextFeedbackInput struct {
	CardID  string `json:"cardId" jsonschema:"自动上下文选中的知识卡 ID"`
	Outcome string `json:"outcome" jsonschema:"delivered | used | ignored | wrong | corrected"`
}
type gatewayContextFeedbackOut struct {
	Recorded bool   `json:"recorded"`
	CardID   string `json:"cardId"`
	Outcome  string `json:"outcome"`
}

type contextQualityStatusInput struct{}

type contextQualityStatusOut struct {
	WindowDays int    `json:"windowDays"`
	Since      string `json:"since"`
	Submitted  int    `json:"submitted"`
	Delivered  int    `json:"delivered"`
	Used       int    `json:"used"`
	Ignored    int    `json:"ignored"`
	Wrong      int    `json:"wrong"`
	Corrected  int    `json:"corrected"`
	Caveat     string `json:"caveat"`
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
		Delivered:  quality.Delivered,
		Used:       quality.Used,
		Ignored:    quality.Ignored,
		Wrong:      quality.Wrong,
		Corrected:  quality.Corrected,
		Caveat:     "这些是已提交反馈的计数，不是自动上下文准确率；没有反馈的交付不在统计内。",
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
	spaces, err := r.readableIDs(nil)
	if err != nil {
		return nil, gatewayContextFeedbackOut{}, toolError(h.logger, "gateway_context_feedback", err)
	}
	if _, _, err := h.know.GetCard(ctx, r.identity.TenantID, platform.ID(in.CardID), spaces); err != nil {
		return nil, gatewayContextFeedbackOut{}, toolError(h.logger, "gateway_context_feedback", err)
	}
	return nil, gatewayContextFeedbackOut{Recorded: true, CardID: in.CardID, Outcome: outcome}, nil
}

type contextBlock struct {
	Domain string `json:"domain"`
	ID     string `json:"id"`
	Body   string `json:"body"`
}

type gatewayAutoContextOut struct {
	Decision          string        `json:"decision"`
	Reason            string        `json:"reason"`
	SemanticAvailable bool          `json:"semanticAvailable"`
	PolicyAvailable   bool          `json:"policyAvailable"`
	Selected          *cardHitOut   `json:"selected,omitempty"`
	Block             *contextBlock `json:"block,omitempty"`
}

func (h *Host) handleGatewayAutoContext(ctx context.Context, _ *sdk.CallToolRequest, in gatewayAutoContextInput) (*sdk.CallToolResult, gatewayAutoContextOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
	}
	spaces, err := r.readTargets(nil)
	if err != nil {
		return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
	}
	qualityCtx, cancel := context.WithTimeout(ctx, gatewayAutoContextTimeout)
	defer cancel()
	res, err := h.know.RecallCardForContext(qualityCtx, knowledge.RecallCardsInput{TenantID: r.identity.TenantID, Spaces: spaces, Situation: in.Prompt})
	if err != nil {
		if qualityCtx.Err() != nil {
			return nil, gatewayAutoContextOut{Decision: "abstain", Reason: "retrieval_timeout", PolicyAvailable: false}, nil
		}
		return nil, gatewayAutoContextOut{}, toolError(h.logger, "gateway_auto_context", err)
	}
	out := gatewayAutoContextOut{Decision: "abstain", Reason: res.Reason, SemanticAvailable: res.SemanticAvailable, PolicyAvailable: res.PolicyAvailable}
	if res.Hit == nil {
		return nil, out, nil
	}
	encoded := r.encodeCardHits([]knowledge.CardHit{*res.Hit})[0]
	body := fmt.Sprintf("[ClaraCore knowledge card — read-only evidence]\nTitle: %s\nMatched trigger: %s\nDistilled: %s\nUnderstanding: %s\nInstruction: The terminal Agent is the final relevance judge. Use only if relevant; verify against current code, runtime, data, and user statements.", res.Hit.Card.Title, res.Hit.Trigger, res.Hit.Card.Distilled, res.Hit.Card.Understanding)
	out.Decision, out.Reason, out.Selected = "deliver_one", res.Reason, &encoded
	out.Block = &contextBlock{Domain: "knowledge_card", ID: string(res.Hit.Card.ID), Body: body}
	return nil, out, nil
}
