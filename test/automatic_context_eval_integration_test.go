//go:build integration

package test

import (
	"context"
	"testing"
	"time"

	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// TestAutomaticContextQualityGate 是真实 embedding + 确定性 Stage B 的
// 正负与延迟门禁。终端 Agent 的模型负责最终语境判断，因此服务端这里只
// 证明强相关样本可进入一个有界候选槽、弱相关样本会被安全放弃。
func TestAutomaticContextQualityGate(t *testing.T) {
	semantic, _, _ := newRecallEnv(t)
	automatic := semantic.WithAutomaticRecallPolicy(knowledge.DefaultAutomaticRecallPolicy())

	tests := []struct {
		name     string
		card     retrievalEvalCard
		positive string
		negative string
	}{
		{
			name: "release-validation",
			card: retrievalEvalCard{
				Title: "发布验证要分层", Distilled: "源码、构建、部署、运行时和业务验收分别留证。",
				Understanding: "上游成功不能替代下游环境与业务结果。", Trigger: "准备宣布一次服务发布已经完成时",
			},
			positive: "ClaraCore 已经部署完成，我准备宣布成功，应该如何做验收？",
			negative: "今天天气怎么样？",
		},
		{
			name: "irreversible-delete",
			card: retrievalEvalCard{
				Title: "备份成功不等于数据可恢复", Distilled: "删除前验证备份包含目标文件并做隔离恢复。",
				Understanding: "归档成功只证明传输完整，不证明内容可恢复。", Trigger: "准备执行不可逆的数据清除动作之前",
			},
			positive: "准备删除唯一一份本地历史资料",
			negative: "帮我设计一道晚餐菜单",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ids := platform.NewIDGenerator(platform.SystemClock{})
			tenant, space, user := ids.New(), ids.New(), ids.New()
			t.Cleanup(func() { mysqlCleanup(t)(tenant) })
			card, err := automatic.CreateCard(ctx, knowledge.CreateCardInput{
				TenantID: tenant, SpaceID: space, AuthorUserID: user, Title: tc.card.Title,
				CardType: "自动上下文评估", Distilled: tc.card.Distilled,
				Understanding: tc.card.Understanding, RecallTriggers: []string{tc.card.Trigger},
			})
			if err != nil {
				t.Fatal(err)
			}
			for {
				report, err := automatic.IndexPending(ctx, 8)
				if err != nil {
					t.Fatal(err)
				}
				if report.Picked == 0 {
					break
				}
			}
			spaces := []knowledge.SearchSpace{{ID: space}}
			assertAutomaticDecision(t, automatic, knowledge.RecallCardsInput{TenantID: tenant, Spaces: spaces, Situation: tc.positive}, card.ID, true)
			assertAutomaticDecision(t, automatic, knowledge.RecallCardsInput{TenantID: tenant, Spaces: spaces, Situation: tc.negative}, card.ID, false)
		})
	}
}

func assertAutomaticDecision(t *testing.T, svc *knowledge.Service, in knowledge.RecallCardsInput, cardID platform.ID, wantHit bool) {
	t.Helper()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := svc.RecallCardForContext(ctx, in)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("自动上下文失败: %v", err)
	}
	if elapsed >= 15*time.Second {
		t.Fatalf("自动上下文超过总预算: %s", elapsed)
	}
	if !result.SemanticAvailable || !result.PolicyAvailable {
		t.Fatalf("质量链路不可用: %+v", result)
	}
	if wantHit {
		if result.Hit == nil || result.Hit.Card.ID != cardID {
			t.Fatalf("正样本未交付期望卡: %+v", result)
		}
	} else if result.Hit != nil {
		t.Fatalf("负样本不应交付: %+v", result)
	}
	t.Logf("wantHit=%v elapsed=%s", wantHit, elapsed)
}
