// Package ingest 是外部知识源的实现（docs/01-architecture.md §8.1）。
//
// 它实现 knowledge.DocumentSource，只负责「把源里的东西列出来」——
// **不提炼**。提炼（写出「理解」与召回条件）是 agent 的活，
// 服务端不引入 LLM 依赖：那会带来又一套 baseURL / model / 配额 /
// 超时 / 失败重试，而换来的自动提炼质量不可控，一旦跑偏就是
// 批量产出垃圾召回条件。
//
// 摄取的产物一律是 draft 原料，价值要靠人和 agent 后续加上去。
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"claracore-server/internal/domain/knowledge"
)

// MaxDocumentBytes 是单份文档的大小上限。
//
// 不是为了省内存，是为了拦住误配：把一个装着日志或二进制的目录指过来时，
// 应该在这里得到一条清晰的失败，而不是让一个 200MB 的文件
// 一路走到 MEDIUMTEXT 的写入报错，或者更糟——真的写进去。
const MaxDocumentBytes = 1 << 20 // 1 MiB

// MarkdownDir 把一个本地目录下的 Markdown 文件当作知识源。
//
// 这是第一个源，选它是因为它把「幂等、失败清单、内容指纹」这三件
// 真正难的事暴露得最干净，而源本身完全可控。ClaraCore 自己的
// docs/ 与 knowledge/cards/ 就是现成的真实语料——自己吃自己的狗粮。
type MarkdownDir struct {
	// label 是这个源的**声明式名字**，不是它在磁盘上的位置。
	//
	// SourceKey 由 label + 相对路径算出，所以 label 必须跨机器稳定：
	// 换一台服务器、换一个 checkout 目录，同一份文档还得是同一份。
	// 一开始我把 root 掺进了 SourceKey，测试当场抓住——那意味着
	// 一次目录搬迁会让库里的卡全部重建一遍（2026-08-10）。
	//
	// 它同时解决另一件事：把两个不同目录摄进同一个空间时，
	// 两边的 `docs/x.md` 不会撞成同一张卡。
	label string
	root  string
	// skipDirs 是不下钻的目录名。默认跳过版本控制与依赖目录：
	// 把 .git 里的对象当文档扫进来，产生的是几千张无意义的 draft。
	skipDirs map[string]bool
}

var _ knowledge.DocumentSource = (*MarkdownDir)(nil)

// NewMarkdownDir 建一个目录源。label 是跨机器稳定的源名（如 "clara-docs"），
// root 是它此刻在这台机器上的位置。
func NewMarkdownDir(label, root string) *MarkdownDir {
	if strings.TrimSpace(label) == "" {
		// 空 label 会让两个源的 SourceKey 只差相对路径，撞车概率骤增。
		// 退回目录名而不是空串——不理想，但比静默撞车好。
		label = filepath.Base(root)
	}
	return &MarkdownDir{
		label: label,
		root:  root,
		skipDirs: map[string]bool{
			".git": true, "node_modules": true, "vendor": true,
			".venv": true, "__pycache__": true, "dist": true, "build": true,
		},
	}
}

func (m *MarkdownDir) Name() string { return "markdown:" + m.label }

// Scan 走一遍目录，把每个 .md 变成一份 Document。
//
// **单个文件的失败不中断整次扫描**：一份读不动的文件会以
// 一条错误的形式出现在返回的错误里，但其余文档照常返回。
// 与 IngestReport 的「保留成功项 + 列失败清单」是同一条原则——
// 一份坏文件不该让一次几千份的摄取整体失败。
func (m *MarkdownDir) Scan(ctx context.Context) ([]knowledge.Document, error) {
	info, err := os.Stat(m.root)
	if err != nil {
		return nil, fmt.Errorf("ingest: 源目录不可读: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ingest: %s 不是目录", m.root)
	}

	var (
		docs     []knowledge.Document
		problems []string
	)
	err = filepath.WalkDir(m.root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			return nil // 继续走，别为一个目录放弃整棵树
		}
		if d.IsDir() {
			if m.skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}

		doc, err := m.readDocument(path)
		if err != nil {
			problems = append(problems, err.Error())
			return nil
		}
		docs = append(docs, doc)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		// 有问题也把成功的那批返回去：调用方据此决定是继续摄取还是先修源
		return docs, fmt.Errorf("ingest: %d 份文件读取失败: %s",
			len(problems), strings.Join(problems, "; "))
	}
	return docs, nil
}

func (m *MarkdownDir) readDocument(path string) (knowledge.Document, error) {
	info, err := os.Stat(path)
	if err != nil {
		return knowledge.Document{}, fmt.Errorf("%s: %v", path, err)
	}
	if info.Size() > MaxDocumentBytes {
		return knowledge.Document{}, fmt.Errorf("%s: 超过 %d 字节上限（%d）", path, MaxDocumentBytes, info.Size())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return knowledge.Document{}, fmt.Errorf("%s: %v", path, err)
	}
	rel, err := filepath.Rel(m.root, path)
	if err != nil {
		rel = path
	}
	// 统一成正斜杠：同一个仓库在 Windows 上扫一遍再在 Linux 上扫一遍，
	// 不该产生两套 source_key、把每张卡都变成两张。
	rel = filepath.ToSlash(rel)

	title, body := splitTitle(string(raw), rel)
	return knowledge.Document{
		// SourceKey = 声明式源名 + 相对路径。**绝不掺绝对路径**：
		// 掺了的话，一次目录搬迁会让整个库里的卡全部重建一遍。
		SourceKey:   hashOf("markdown:" + m.label + "\x00" + rel),
		SourcePath:  rel,
		ContentHash: hashOf(string(raw)),
		Title:       title,
		Body:        body,
	}, nil
}

// splitTitle 取第一个一级标题作为卡片标题，其余作为正文。
//
// 取不到就用文件名——**绝不返回空标题**：标题为空的文档会在领域层
// 被判为非法而进失败清单，而「这份文档没有 # 开头的行」
// 不该算作一次失败，那在现实的 md 里太常见了。
func splitTitle(content, fallback string) (title, body string) {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "# ") {
			continue
		}
		title = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		if title == "" {
			continue
		}
		// 正文是**整篇**，含标题行之后的全部内容。不做进一步切分：
		// 按 ## 拆成多张卡是一种提炼判断，而提炼不归摄取管。
		body = strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		if body == "" {
			// 只有一行标题的文档：把标题也当正文，免得领域层判它正文为空。
			// 这类文档确实没什么内容，但它的存在本身是信息。
			body = title
		}
		return title, body
	}

	title = strings.TrimSuffix(filepath.Base(fallback), filepath.Ext(fallback))
	body = strings.TrimSpace(content)
	if body == "" {
		body = title
	}
	return title, body
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
