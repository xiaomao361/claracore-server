package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestScanReadsMarkdownOnly(t *testing.T) {
	root := writeTree(t, map[string]string{
		"a.md":              "# 标题 A\n\n正文 A。",
		"sub/b.markdown":    "不该被扫到（扩展名不是 .md）",
		"sub/c.md":          "# 标题 C\n\n正文 C。",
		"notes.txt":         "不是 markdown",
		".git/objects/x":    "版本控制内部文件",
		"node_modules/d.md": "依赖目录里的文档",
	})

	docs, err := NewMarkdownDir("test-source", root).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		var got []string
		for _, d := range docs {
			got = append(got, d.SourcePath)
		}
		t.Fatalf("应只扫到 2 份 .md，实际 %d: %v", len(docs), got)
	}
	// .git 与 node_modules 必须跳过：把它们扫进来产生的是几千张无意义的 draft
	for _, d := range docs {
		if strings.HasPrefix(d.SourcePath, ".git") || strings.HasPrefix(d.SourcePath, "node_modules") {
			t.Fatalf("不该扫到 %s", d.SourcePath)
		}
	}
}

func TestSourceKeyIsStableAcrossRoots(t *testing.T) {
	// 同一份文档换一个 checkout 目录，SourceKey 必须不变——
	// 否则一次目录搬迁会让库里的卡全部重建一遍
	content := "# 同一份文档\n\n内容没变。"
	a := writeTree(t, map[string]string{"docs/x.md": content})
	b := writeTree(t, map[string]string{"docs/x.md": content})

	da, err := NewMarkdownDir("test-source", a).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	db, err := NewMarkdownDir("test-source", b).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if da[0].SourceKey != db[0].SourceKey {
		t.Fatal("SourceKey 必须只依赖「源名 + 相对路径」，不能掺进绝对路径——" +
			"掺了的话一次目录搬迁会让库里的卡全部重建一遍")
	}
	if da[0].ContentHash != db[0].ContentHash {
		t.Fatal("内容相同则指纹必须相同")
	}
}

func TestContentHashTracksContent(t *testing.T) {
	root := writeTree(t, map[string]string{"x.md": "# 标题\n\n第一版。"})
	first, err := NewMarkdownDir("test-source", root).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "x.md"), []byte("# 标题\n\n第二版。"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := NewMarkdownDir("test-source", root).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// 同一份文档：身份不变，指纹要变——这正是「更新而不是新建」的依据
	if first[0].SourceKey != second[0].SourceKey {
		t.Fatal("内容变了但还是同一份文档，SourceKey 不该变")
	}
	if first[0].ContentHash == second[0].ContentHash {
		t.Fatal("内容变了指纹必须变，否则重扫会跳过这次更新")
	}
}

func TestTitleFallsBackToFilename(t *testing.T) {
	root := writeTree(t, map[string]string{
		"有标题.md":  "# 真标题\n\n正文。",
		"没标题.md":  "这份文档没有 # 开头的行。",
		"只有标题.md": "# 光杆标题",
	})
	docs, err := NewMarkdownDir("test-source", root).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]string{}
	bodies := map[string]string{}
	for _, d := range docs {
		byPath[d.SourcePath] = d.Title
		bodies[d.SourcePath] = d.Body
	}

	if byPath["有标题.md"] != "真标题" {
		t.Fatalf("应取第一个一级标题，实际 %q", byPath["有标题.md"])
	}
	// 「没有 # 开头的行」在现实的 md 里太常见，不该算失败
	if byPath["没标题.md"] != "没标题" {
		t.Fatalf("取不到标题应退回文件名，实际 %q", byPath["没标题.md"])
	}
	// 领域层会把空正文判为非法，所以这里绝不能产出空正文
	for path, body := range bodies {
		if strings.TrimSpace(body) == "" {
			t.Fatalf("%s 的正文为空——它会在领域层被判非法而进失败清单", path)
		}
	}
}

func TestOversizedFileIsReportedNotIngested(t *testing.T) {
	root := writeTree(t, map[string]string{
		"ok.md":  "# 正常\n\n内容。",
		"big.md": "# 巨大\n\n" + strings.Repeat("x", MaxDocumentBytes+1),
	})

	docs, err := NewMarkdownDir("test-source", root).Scan(context.Background())
	// 超限的那份进错误，正常的那份照常返回——一份坏文件不该让整次扫描失败
	if err == nil {
		t.Fatal("超限文件应当被报出来")
	}
	if !strings.Contains(err.Error(), "big.md") {
		t.Fatalf("错误里应指出是哪一份，实际: %v", err)
	}
	if len(docs) != 1 || docs[0].SourcePath != "ok.md" {
		t.Fatalf("正常文件应照常返回，实际 %+v", docs)
	}
}

func TestScanRejectsNonDirectory(t *testing.T) {
	root := writeTree(t, map[string]string{"x.md": "# x\n\ny"})

	if _, err := NewMarkdownDir("test-source", filepath.Join(root, "x.md")).Scan(context.Background()); err == nil {
		t.Fatal("指向一个文件时应当报错")
	}
	if _, err := NewMarkdownDir("test-source", filepath.Join(root, "不存在")).Scan(context.Background()); err == nil {
		t.Fatal("目录不存在时应当报错")
	}
}

// TestDifferentSourcesDoNotCollide 是 label 存在的第二个理由。
//
// 把两个不同的目录摄进同一个空间时，两边的 docs/x.md
// 不该撞成同一张卡——那会让它们在 uk_cards_source 上互相覆盖。
func TestDifferentSourcesDoNotCollide(t *testing.T) {
	content := "# 同名文件\n\n但来自不同的源。"
	a := writeTree(t, map[string]string{"docs/x.md": content})
	b := writeTree(t, map[string]string{"docs/x.md": content})

	da, err := NewMarkdownDir("源甲", a).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	db, err := NewMarkdownDir("源乙", b).Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if da[0].SourceKey == db[0].SourceKey {
		t.Fatal("不同源下的同名文件必须有不同的 SourceKey")
	}
}
