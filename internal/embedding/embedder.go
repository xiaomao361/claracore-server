package embedding

import "context"

// Embedder 把文本变成向量。
//
// 实现必须遵守：返回的向量与 texts 一一对应且同序；一次调用内维度相同；
// 部分失败即整体失败。错误信息不得包含内网地址或凭证——它会写进 entries.embed_error。
type Embedder interface {
	// Model 返回模型标识，原样写进 entries.embed_model。换模型即换这个值，旧向量会被重算。
	Model() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
