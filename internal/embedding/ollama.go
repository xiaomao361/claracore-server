package embedding

import (
	"context"
	"fmt"
	"time"
)

// Ollama 通过 Ollama 的 /api/embed 接口算向量，**一次请求只算一段**。
//
// /api/embed 虽然接受一批 input，但返回里没有 index 可以校对，而顺序并不可靠：
// 2026-10-16 在 Local200 的 Ollama 0.3.3 + bge-m3 上实测，同一批 8 条查询
// 三次里有两次返回乱序（批内第 0 条拿到的是第 2 条的向量）。向量安错对象
// 不会报错，只会让检索悄悄变差，所以宁可多几次 HTTP 往返，用请求本身绑定对应关系。
// 这个服务的量级（写入时一条，后台每轮最多 32 条）不在乎这点吞吐。
type Ollama struct {
	c client
}

var _ Embedder = (*Ollama)(nil)

func NewOllama(baseURL, model, apiKey string, timeout time.Duration) *Ollama {
	return &Ollama{c: newClient("ollama", baseURL, model, apiKey, timeout)}
}

func (o *Ollama) Model() string { return o.c.model }

func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		var out struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		req := struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}{Model: o.c.model, Input: []string{text}}
		if err := o.c.postJSON(ctx, "/api/embed", req, &out); err != nil {
			return nil, err
		}
		if len(out.Embeddings) != 1 {
			return nil, fmt.Errorf("embedding(%s): 一段文本返回了 %d 个向量", o.c.provider, len(out.Embeddings))
		}
		vectors[i] = out.Embeddings[0]
	}
	if err := o.c.checkBatch(vectors, len(texts)); err != nil {
		return nil, err
	}
	return vectors, nil
}
