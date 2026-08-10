package embedding

import (
	"context"
	"time"

	"claracore-server/internal/ports"
)

// Ollama 通过 Ollama 的 /api/embed 接口算向量。
//
// 用 /api/embed 而不是文档里最初写的 /api/embeddings：后者一次只能算一段
// （字段是 prompt 不是 input），一批 32 条就是 32 次 HTTP 往返。
// 代价是要求 Ollama 版本足够新——接口不存在时 postJSON 会明确报 404
// 并提示去看版本与 BaseURL，而不是留下一句看不懂的解码错误。
type Ollama struct {
	c client
}

var _ ports.Embedder = (*Ollama)(nil)

func NewOllama(baseURL, model, apiKey string, timeout time.Duration) *Ollama {
	return &Ollama{c: newClient("ollama", baseURL, model, apiKey, timeout)}
}

func (o *Ollama) Model() string { return o.c.model }

func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	req := struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Model: o.c.model, Input: texts}

	if err := o.c.postJSON(ctx, "/api/embed", req, &out); err != nil {
		return nil, err
	}
	// Ollama 按入参顺序返回，没有 index 字段可以校对——所以这里
	// 只能校数量与维度，顺序只能信它。这是选它的代价之一。
	if err := o.c.checkBatch(out.Embeddings, len(texts)); err != nil {
		return nil, err
	}
	return out.Embeddings, nil
}
