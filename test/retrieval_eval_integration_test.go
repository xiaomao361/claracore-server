//go:build integration

package test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

type retrievalEvalCard struct {
	Title         string `json:"title"`
	Distilled     string `json:"distilled"`
	Understanding string `json:"understanding"`
	Trigger       string `json:"trigger"`
}

type retrievalEvalCase struct {
	ID        string            `json:"id"`
	Situation string            `json:"situation"`
	Expected  retrievalEvalCard `json:"expected"`
	Decoy     retrievalEvalCard `json:"decoy"`
}

type retrievalEvalSet struct {
	Version int                 `json:"version"`
	Cases   []retrievalEvalCase `json:"cases"`
}

// TestRetrievalEvaluationBaseline 是可复跑的质量门槛，不只证明接口跑通。
// 数据集刻意给每个正确答案配一个字面更接近的干扰项；指标看 Hit@1、
// Hit@3 与 MRR，模型或排序策略变化时能给出可比较的回执。
func TestRetrievalEvaluationBaseline(t *testing.T) {
	semantic, _, _ := newRecallEnv(t)
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
	expectedIDs := make(map[string]platform.ID, len(set.Cases))
	for _, evalCase := range set.Cases {
		for i, item := range []retrievalEvalCard{evalCase.Expected, evalCase.Decoy} {
			card, err := semantic.CreateCard(ctx, knowledge.CreateCardInput{
				TenantID: tenant, SpaceID: space, AuthorUserID: user,
				Title: item.Title, CardType: "检索评估",
				Distilled: item.Distilled, Understanding: item.Understanding,
				RecallTriggers: []string{item.Trigger},
			})
			if err != nil {
				t.Fatalf("创建评估卡 %s: %v", item.Title, err)
			}
			if i == 0 {
				expectedIDs[evalCase.ID] = card.ID
			}
		}
	}
	t.Cleanup(func() { mysqlCleanup(t)(tenant) })
	for {
		report, err := semantic.IndexPending(ctx, 64)
		if err != nil {
			t.Fatalf("索引评估集: %v", err)
		}
		if report.Picked == 0 {
			break
		}
	}

	hit1, hit3, reciprocalRank := 0, 0, 0.0
	spaces := []knowledge.SearchSpace{{ID: space, Rank: 0}}
	for _, evalCase := range set.Cases {
		result, err := semantic.RecallCards(ctx, knowledge.RecallCardsInput{
			TenantID: tenant, Spaces: spaces, Situation: evalCase.Situation, Limit: 10,
		})
		if err != nil {
			t.Fatalf("评估 %s: %v", evalCase.ID, err)
		}
		if !result.SemanticAvailable {
			t.Fatalf("评估 %s 没有使用语义检索", evalCase.ID)
		}
		rank := 0
		for i, hit := range result.Hits {
			if hit.Card.ID == expectedIDs[evalCase.ID] {
				rank = i + 1
				break
			}
		}
		if rank == 1 {
			hit1++
		}
		if rank > 0 && rank <= 3 {
			hit3++
		}
		if rank > 0 {
			reciprocalRank += 1 / float64(rank)
		}
		t.Logf("case=%s rank=%d top=%s", evalCase.ID, rank, firstTitle(result))
	}

	n := len(set.Cases)
	mrr := reciprocalRank / float64(n)
	t.Logf("retrieval baseline: cases=%d hit@1=%d/%d hit@3=%d/%d mrr=%.3f", n, hit1, n, hit3, n, mrr)
	if hit3 != n {
		t.Fatalf("检索基线退化：Hit@3=%d/%d，要求全部进入前三", hit3, n)
	}
	if hit1*2 < n || mrr < 0.70 {
		t.Fatalf("检索基线退化：Hit@1=%d/%d MRR=%.3f，要求 Hit@1>=50%% 且 MRR>=0.70", hit1, n, mrr)
	}
}
