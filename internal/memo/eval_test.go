package memo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"claracore-server/internal/embedding"
)

// TestRecallEval 用真实条目和真实向量模型跑一组查询，打印每条查询的排名并统计首位命中。
// 默认跳过：条目可能含内部信息，不进仓库，由环境变量指向本地文件。
//
//	CLARACORE_EVAL_FIXTURE=/path/entries.json   条目：[{title, body, tags, kind}]
//	CLARACORE_EVAL_QUERIES=/path/queries.json   查询：[{query, want}]，want 是期望首位条目标题里的一段
//	CLARACORE_EVAL_OLLAMA=http://127.0.0.1:11434 可选，不给则只跑文本一路
//	CLARACORE_EVAL_MODEL=bge-m3:latest           可选
func TestRecallEval(t *testing.T) {
	fixture, queries := os.Getenv("CLARACORE_EVAL_FIXTURE"), os.Getenv("CLARACORE_EVAL_QUERIES")
	if fixture == "" || queries == "" {
		t.Skip("未设置 CLARACORE_EVAL_FIXTURE / CLARACORE_EVAL_QUERIES")
	}
	var entries []SaveInput
	var qs []struct{ Query, Want string }
	for path, v := range map[string]any{fixture: &entries, queries: &qs} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatal(err)
		}
	}

	s := newTestService(t, nil)
	if url := os.Getenv("CLARACORE_EVAL_OLLAMA"); url != "" {
		model := os.Getenv("CLARACORE_EVAL_MODEL")
		if model == "" {
			model = "bge-m3:latest"
		}
		s.embedder = embedding.NewOllama(url, model, "", 0)
	}
	ctx := context.Background()
	for _, e := range entries {
		e.Shared = true
		mustSave(t, s, "eval", e)
	}

	top1 := 0
	for _, q := range qs {
		res, err := s.Search(ctx, "eval", SearchInput{Query: q.Query, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, h := range res.Items {
			c := ""
			if h.Cosine != nil {
				c = fmt.Sprintf(" %.3f", *h.Cosine)
			}
			title := []rune(h.Title)
			got = append(got, fmt.Sprintf("%s[%s%s]", string(title[:min(14, len(title))]), h.Match, c))
		}
		mark := "MISS"
		if len(res.Items) > 0 && strings.Contains(res.Items[0].Title, q.Want) {
			mark, top1 = "ok", top1+1
		}
		t.Logf("%-4s %s (semantic=%s) -> %s", mark, q.Query, res.Semantic, strings.Join(got, " | "))
	}
	t.Logf("首位命中 %d/%d", top1, len(qs))
}
