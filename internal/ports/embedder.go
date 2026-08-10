// Package ports 放**纯基础设施接口**：签名里只出现标准库类型，
// 不认识任何领域概念（docs/05-code-structure.md §3.1）。
//
// 涉及领域类型的接口（各域的 Store）定义在各自的 domain 包内——
// 放这里会造成 domain → ports → domain 的循环 import，Go 不允许。
// 由 test/arch_test.go 强制：ports/** 不得 import domain/**。
package ports

import "context"

// Embedder 把文本变成向量。
//
// 服务端不做本地推理（docs/01-architecture.md §2.2）：实现一律是对外部
// HTTP 服务（Ollama / OpenAI-compatible）的调用。Desktop 那个
// @xenova/transformers 的本地 ONNX 推理在服务端是负担而非资产。
//
// # 实现必须遵守的约定
//
//   - 返回的向量数量与 texts **一一对应且同序**。少一条、多一条、顺序错了，
//     都会让向量被安到别的内容头上——那是一种不会报错、只会让检索
//     悄悄变差的故障，事后几乎无法定位。
//   - 一次调用内所有向量维度相同。
//   - 部分失败即整体失败，返回 error 而不是残缺的切片。调用方按批重试，
//     没有"这批里有几条是好的"这种中间状态需要处理。
//   - 错误信息不得包含内网地址或凭证：它会被写进 embeddings.last_error 列。
type Embedder interface {
	// Model 返回模型标识，原样写进 embeddings.model 列。
	//
	// 换模型即换这个值：新旧向量在库里共存，可灰度重建
	// （docs/02-data-model.md §5.3）。所以它必须能唯一标识"算出这个向量的东西"，
	// 不能是 "default" 这种会在两个模型之间复用的名字。
	Model() string

	// Embed 批量把文本变成向量。
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
