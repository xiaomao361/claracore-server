//go:build integration

package test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// TestAutomaticContextQualityGate 是真实 embedding + 确定性 Stage B 的
// 共享语料质量门禁。终端 Agent 的模型负责最终语境判断，因此服务端这里只
// 证明强相关样本能在候选竞争中进入一个有界槽位、弱相关样本会被安全放弃。
func TestAutomaticContextQualityGate(t *testing.T) {
	semantic, _, _ := newRecallEnv(t)
	automatic := semantic.WithAutomaticRecallPolicy(knowledge.DefaultAutomaticRecallPolicy())
	raw, err := os.ReadFile("testdata/retrieval_eval.json")
	if err != nil {
		t.Fatal(err)
	}
	var set retrievalEvalSet
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	if set.Version != 1 || len(set.Cases) == 0 {
		t.Fatalf("评估集版本或内容非法: version=%d cases=%d", set.Version, len(set.Cases))
	}

	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	tenant, space, user := ids.New(), ids.New(), ids.New()
	t.Cleanup(func() { mysqlCleanup(t)(tenant) })
	expectedIDs := make(map[string]platform.ID, len(set.Cases))
	for _, evalCase := range set.Cases {
		item := evalCase.Expected
		card, err := automatic.CreateCard(ctx, knowledge.CreateCardInput{
			TenantID: tenant, SpaceID: space, AuthorUserID: user, Title: item.Title,
			CardType: "自动上下文评估", Distilled: item.Distilled,
			Understanding: item.Understanding, RecallTriggers: []string{item.Trigger},
		})
		if err != nil {
			t.Fatalf("创建评估卡 %s: %v", item.Title, err)
		}
		expectedIDs[evalCase.ID] = card.ID
	}
	indexAll(t, automatic, 64)
	spaces := []knowledge.SearchSpace{{ID: space}}
	for _, evalCase := range set.Cases {
		t.Run(evalCase.ID, func(t *testing.T) {
			logCardCandidates(t, automatic, knowledge.RecallCardsInput{
				TenantID: tenant, Spaces: spaces, Situation: evalCase.Situation,
			})
			assertAutomaticDecision(t, automatic, knowledge.RecallCardsInput{
				TenantID: tenant, Spaces: spaces, Situation: evalCase.Situation,
			}, expectedIDs[evalCase.ID], true, "high_confidence_top1")
		})
	}
	assertAutomaticDecision(t, automatic, knowledge.RecallCardsInput{
		TenantID: tenant, Spaces: spaces, Situation: "帮我设计一道适合六个人的晚餐菜单",
	}, "", false, "low_relevance")
}

// 完全重复的召回条件代表重复导入或尚未治理的近重复卡。即使它们都很相关，
// 自动路径也不能任意挑一张；相同向量确保这条歧义门禁不依赖模型偶然分差。
func TestAutomaticContextRejectsDuplicateCards(t *testing.T) {
	semantic, _, _ := newRecallEnv(t)
	automatic := semantic.WithAutomaticRecallPolicy(knowledge.DefaultAutomaticRecallPolicy())
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	tenant, space, user := ids.New(), ids.New(), ids.New()
	t.Cleanup(func() { mysqlCleanup(t)(tenant) })
	for _, title := range []string{"发布完成判定 A", "发布完成判定 B"} {
		if _, err := automatic.CreateCard(ctx, knowledge.CreateCardInput{
			TenantID: tenant, SpaceID: space, AuthorUserID: user, Title: title,
			CardType: "自动上下文评估", Distilled: "健康探针成功不等于业务验收完成。",
			Understanding:  "发布结论必须分开验证运行时健康与真实业务结果。",
			RecallTriggers: []string{"准备宣布服务发布已经完成时"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	indexAll(t, automatic, 16)
	assertAutomaticDecision(t, automatic, knowledge.RecallCardsInput{
		TenantID: tenant, Spaces: []knowledge.SearchSpace{{ID: space}},
		Situation: "健康探针已经通过，我准备宣布服务发布完成",
	}, "", false, "ambiguous_top_results")
}

func TestAutomaticMemoryContextQualityGate(t *testing.T) {
	semantic, _, _ := newRecallEnv(t)
	automatic := semantic.WithAutomaticRecallPolicy(knowledge.DefaultAutomaticRecallPolicy())
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	tenant, space, user := ids.New(), ids.New(), ids.New()
	t.Cleanup(func() { mysqlCleanup(t)(tenant) })
	memory, err := automatic.CreateMemory(ctx, knowledge.CreateMemoryInput{
		TenantID: tenant, SpaceID: space, AuthorUserID: user,
		Title: "发布验证要分层",
		Body:  "宣布服务发布完成前，必须分别验证源码、构建、部署、运行时健康和业务验收。",
	})
	if err != nil {
		t.Fatal(err)
	}
	indexAll(t, automatic, 8)
	spaces := []knowledge.SearchSpace{{ID: space}}
	assertAutomaticMemoryDecision(t, automatic, knowledge.SearchMemoriesInput{
		TenantID: tenant, Spaces: spaces, Query: "ClaraCore 已部署，我准备宣布发布完成，应该如何分层验收？",
	}, memory.ID, true, "high_confidence_top1")
	assertAutomaticMemoryDecision(t, automatic, knowledge.SearchMemoriesInput{
		TenantID: tenant, Spaces: spaces, Query: "帮我设计一道晚餐菜单",
	}, memory.ID, false, "low_relevance")
}

func TestAutomaticMemoryContextRejectsDuplicates(t *testing.T) {
	semantic, _, _ := newRecallEnv(t)
	automatic := semantic.WithAutomaticRecallPolicy(knowledge.DefaultAutomaticRecallPolicy())
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	tenant, space, user := ids.New(), ids.New(), ids.New()
	t.Cleanup(func() { mysqlCleanup(t)(tenant) })
	for _, title := range []string{"连接池结论 A", "连接池结论 B"} {
		if _, err := automatic.CreateMemory(ctx, knowledge.CreateMemoryInput{
			TenantID: tenant, SpaceID: space, AuthorUserID: user, Title: title,
			Body: "MySQL 最大连接数从两百降到四十后，连接超时消失。",
		}); err != nil {
			t.Fatal(err)
		}
	}
	indexAll(t, automatic, 16)
	assertAutomaticMemoryDecision(t, automatic, knowledge.SearchMemoriesInput{
		TenantID: tenant, Spaces: []knowledge.SearchSpace{{ID: space}},
		Query: "MySQL 连接超时应该如何调整最大连接数？",
	}, "", false, "ambiguous_top_results")
}

func logCardCandidates(t *testing.T, svc *knowledge.Service, in knowledge.RecallCardsInput) {
	t.Helper()
	in.Limit = 3
	result, err := svc.RecallCards(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for i, hit := range result.Hits {
		t.Logf("candidate=%d title=%q score=%.4f trigger=%q", i+1, hit.Card.Title, hit.Score, hit.Trigger)
	}
}

func indexAll(t *testing.T, svc *knowledge.Service, batch int) {
	t.Helper()
	for {
		report, err := svc.IndexPending(context.Background(), batch)
		if err != nil {
			t.Fatal(err)
		}
		if report.Picked == 0 {
			return
		}
	}
}

func assertAutomaticDecision(t *testing.T, svc *knowledge.Service, in knowledge.RecallCardsInput, cardID platform.ID, wantHit bool, wantReason string) {
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
	if result.Reason != wantReason {
		t.Fatalf("决策原因错误: want=%s got=%+v", wantReason, result)
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

func assertAutomaticMemoryDecision(t *testing.T, svc *knowledge.Service, in knowledge.SearchMemoriesInput, memoryID platform.ID, wantHit bool, wantReason string) {
	t.Helper()
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := svc.RecallMemoryForContext(ctx, in)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Memory 自动上下文失败: %v", err)
	}
	if elapsed >= 15*time.Second {
		t.Fatalf("Memory 自动上下文超过总预算: %s", elapsed)
	}
	if !result.SemanticAvailable || !result.PolicyAvailable {
		t.Fatalf("Memory 质量链路不可用: %+v", result)
	}
	if result.Reason != wantReason {
		t.Fatalf("Memory 决策原因错误: want=%s got=%+v", wantReason, result)
	}
	if wantHit {
		if result.Hit == nil || result.Hit.Memory.ID != memoryID {
			t.Fatalf("正样本未交付期望 Memory: %+v", result)
		}
	} else if result.Hit != nil {
		t.Fatalf("负样本不应交付 Memory: %+v", result)
	}
	t.Logf("memory wantHit=%v elapsed=%s", wantHit, elapsed)
}
