package knowledge

import (
	"context"
	"fmt"
	"strings"
)

// Document 是一份等待被摄取的源文档。
//
// 它刻意**只有原文，没有提炼**：标题 + 正文 + 来源，仅此而已。
// 摄取器不产出「理解」「召回条件」这些栏——那些是 Card 的价值所在
// （docs/01-architecture.md §8.1），而现成文档恰恰没有它们。
// 整篇导入的结果是一堆检索不准的长文本，`card_recall` 什么都命不中，
// 因为没有触发条件。
//
// 所以摄取的产物一律是 `draft`：它是**原料**，不是卡片。
// 变成卡片要靠 agent 读它、写下提炼与召回条件，再由人确认。
type Document struct {
	// SourceKey 是这份文档的**稳定标识**，幂等全靠它。
	//
	// 必须满足：同一份源文档，无论扫多少次、内容怎么改，SourceKey 不变。
	// 用路径而不是内容算——内容变了还是同一份文档，那正是要更新而不是新建的情况。
	SourceKey string

	// SourcePath 是给人看的来源说明，落进 cards.source。
	SourcePath string

	// ContentHash 是正文的指纹。没变就整张卡都不动——
	// 不推 version、不作废向量。重扫一个没改过的目录应该是零成本的。
	ContentHash string

	Title string
	Body  string
}

func (d Document) validate() error {
	if strings.TrimSpace(d.SourceKey) == "" {
		return fmt.Errorf("%w: 源标识为空", ErrInvalidArgument)
	}
	if strings.TrimSpace(d.ContentHash) == "" {
		return fmt.Errorf("%w: 内容指纹为空", ErrInvalidArgument)
	}
	if strings.TrimSpace(d.Title) == "" {
		return fmt.Errorf("%w: 文档标题为空（%s）", ErrInvalidArgument, d.SourcePath)
	}
	if strings.TrimSpace(d.Body) == "" {
		return fmt.Errorf("%w: 文档正文为空（%s）", ErrInvalidArgument, d.SourcePath)
	}
	return nil
}

// DocumentSource 是一个可枚举的外部知识源。
//
// 接口定义在 domain 内而不是 ports/：签名涉及领域类型 Document
// （docs/05-code-structure.md §3.1）。实现落在 internal/ingest/——
// 那里可以读文件系统、发 HTTP，领域层不必知道。
//
// # 「可枚举」是前提，不是巧合
//
// 01-architecture.md §8.1.3 拍板过：外部源都是自有服务，缺接口可以补。
// 一个只能搜不能列的源没法做幂等——你无从知道「上次摄进来的那份
// 这次还在不在」，于是删掉的源文档会永远留在库里。
type DocumentSource interface {
	// Name 用于日志与报告，让人知道这批是从哪来的。
	Name() string

	// Scan 列出这个源当前的全部文档。
	//
	// 返回**全量**而不是增量：增量要求源能可靠地告诉你「哪些变了」，
	// 而文件系统和多数外部服务都做不到这件事。全量 + 内容指纹比对，
	// 是唯一不会漏更新的做法，代价是每次都要读一遍——对本地目录
	// 和自有服务来说完全可以接受。
	Scan(ctx context.Context) ([]Document, error)
}
