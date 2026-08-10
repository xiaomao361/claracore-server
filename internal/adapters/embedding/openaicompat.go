package embedding

import (
	"context"
	"fmt"
	"time"

	"claracore-server/internal/ports"
)

// OpenAICompat 通过 OpenAI 风格的 /v1/embeddings 接口算向量。
//
// 覆盖 OpenAI 本身以及一大批兼容实现（vLLM、LocalAI、各家云厂商网关）——
// 这是「已知的变化点」，值得留这个第二实现（docs/05-code-structure.md §0.1）。
type OpenAICompat struct {
	c client
}

var _ ports.Embedder = (*OpenAICompat)(nil)

func NewOpenAICompat(baseURL, model, apiKey string, timeout time.Duration) *OpenAICompat {
	return &OpenAICompat{c: newClient("openai-compatible", baseURL, model, apiKey, timeout)}
}

func (o *OpenAICompat) Model() string { return o.c.model }

func (o *OpenAICompat) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	req := struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Model: o.c.model, Input: texts}

	if err := o.c.postJSON(ctx, "/v1/embeddings", req, &out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embedding(%s): 返回 %d 个向量，期望 %d 个", o.c.provider, len(out.Data), len(texts))
	}

	// **按 index 归位，不按数组顺序**。规范允许返回顺序与入参不同，
	// 而这里错一次的后果是向量被安到别的内容头上——一种不报错、
	// 只让检索悄悄变差的故障。多写这几行是为了不必信任对端的实现细节。
	vectors := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embedding(%s): 返回了越界的 index %d", o.c.provider, d.Index)
		}
		if vectors[d.Index] != nil {
			return nil, fmt.Errorf("embedding(%s): index %d 出现两次", o.c.provider, d.Index)
		}
		vectors[d.Index] = d.Embedding
	}
	if err := o.c.checkBatch(vectors, len(texts)); err != nil {
		return nil, err
	}
	return vectors, nil
}
