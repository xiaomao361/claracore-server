// Package test 承载跨包的架构约束与集成测试。
//
// 本文件是 docs/05-code-structure.md §5.1 的可执行形式：
// 目录结构挡不住技术债，能进 CI 的检查才行。
//
// 仅使用标准库（go/parser + go/ast），不依赖任何第三方架构 lint 工具
// ——那类工具停更之日，就是约束失效之时。
package test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "claracore-server"

// rule 描述一条依赖方向约束。
type rule struct {
	name string
	// from 是被检查的包路径前缀（相对模块根，如 "internal/domain"）
	from string
	// forbidden 是禁止出现在 import 中的前缀。
	// 模块内路径写相对形式（"internal/adapters"），标准库/第三方写完整路径（"net/http"）。
	forbidden []string
	// except 是 from 之下豁免本规则的子路径前缀（极少使用，慎加）
	except []string
	reason string
}

var rules = []rule{
	{
		name:      "领域层不得依赖基础设施",
		from:      "internal/domain",
		forbidden: []string{"internal/adapters"},
		reason:    "domain 只依赖自己定义的接口，由 adapters 反向实现（依赖倒置）",
	},
	{
		name:      "领域层不得感知传输与存储技术",
		from:      "internal/domain",
		forbidden: []string{"net/http", "database/sql", "github.com/go-sql-driver", "github.com/redis"},
		reason:    "出现这些类型说明抽象漏了，领域逻辑被具体技术污染",
	},
	{
		name:      "领域子域之间不得互相依赖",
		from:      "internal/domain/knowledge",
		forbidden: []string{"internal/domain/governance", "internal/domain/continuity"},
		reason:    "权限在应用层解析为 SpaceScope 值后传入，knowledge 不认识 user/permission 概念",
	},
	{
		name:      "领域子域之间不得互相依赖（governance）",
		from:      "internal/domain/governance",
		forbidden: []string{"internal/domain/knowledge", "internal/domain/continuity"},
		reason:    "同上，域间通过应用层编排而非直接调用",
	},
	{
		name:      "ports 必须保持纯净",
		from:      "internal/ports",
		forbidden: []string{"internal/domain", "internal/adapters", "internal/mcp", "internal/web"},
		reason:    "ports 只放不涉及领域类型的通用基础设施接口（Cache/Locker/RateLimiter 等）；涉及领域类型的 Store 接口应定义在各自 domain 包内",
	},
	{
		name:      "MCP Host 不得直连基础设施",
		from:      "internal/mcp",
		forbidden: []string{"internal/adapters"},
		reason:    "Host 只做协议解码、身份装配、结果编码，业务与存储一律经 domain",
	},
	{
		name:      "Web Host 不得直连基础设施",
		from:      "internal/web",
		forbidden: []string{"internal/adapters"},
		reason:    "同上",
	},
	{
		name:      "两个 Host 不得互相依赖",
		from:      "internal/mcp",
		forbidden: []string{"internal/web"},
		reason:    "MCP 与 Web 是并列的入口，任何一方持有对方逻辑都是设计事故",
	},
	{
		name:      "两个 Host 不得互相依赖（web）",
		from:      "internal/web",
		forbidden: []string{"internal/mcp"},
		reason:    "同上",
	},
	{
		name:      "领域层不得直接读取环境变量",
		from:      "internal/domain",
		forbidden: []string{"os/exec"},
		reason:    "配置只在 internal/config 加载，见 05-code-structure.md §6.3",
	},
}

func TestArchitectureDependencies(t *testing.T) {
	root := repoRoot(t)

	files := goFiles(t, root, []string{"internal", "cmd"})
	if len(files) == 0 {
		t.Skip("尚无 Go 源文件，骨架阶段跳过")
	}

	fset := token.NewFileSet()
	violations := 0

	for _, file := range files {
		rel := mustRel(t, root, file)
		pkgPath := filepath.ToSlash(filepath.Dir(rel))

		f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析失败 %s: %v", rel, err)
		}

		for _, imp := range f.Imports {
			raw := strings.Trim(imp.Path.Value, `"`)
			// 模块内 import 归一化为相对路径，便于与规则前缀比较
			normalized := strings.TrimPrefix(raw, modulePath+"/")

			for _, r := range rules {
				if !strings.HasPrefix(pkgPath, r.from) {
					continue
				}
				if hasAnyPrefix(pkgPath, r.except) {
					continue
				}
				for _, bad := range r.forbidden {
					if normalized == bad || strings.HasPrefix(normalized, bad+"/") {
						violations++
						t.Errorf(
							"\n架构约束被破坏: %s\n  文件: %s\n  非法 import: %s\n  原因: %s\n  修复方式: 修结构，不加例外（05-code-structure.md §5.4）",
							r.name, rel, raw, r.reason,
						)
					}
				}
			}
		}
	}

	if violations == 0 {
		t.Logf("依赖方向检查通过：%d 个文件，%d 条规则", len(files), len(rules))
	}
}

// TestDomainHasNoPackageLevelVars 禁止领域层出现包级可变状态。
//
// 包级变量让行为依赖调用顺序与进程生命周期，破坏可测试性，
// 并且在多租户服务中极易演变成跨租户的状态泄漏。
//
// 唯一豁免：以 Err 开头的哨兵错误（Go 社区通行惯例）。
func TestDomainHasNoPackageLevelVars(t *testing.T) {
	root := repoRoot(t)
	files := goFiles(t, root, []string{"internal/domain"})
	if len(files) == 0 {
		t.Skip("领域层尚无源文件")
	}

	fset := token.NewFileSet()
	for _, file := range files {
		rel := mustRel(t, root, file)
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("解析失败 %s: %v", rel, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range vs.Names {
					if name.Name == "_" || strings.HasPrefix(name.Name, "Err") {
						continue // 哨兵错误豁免
					}
					t.Errorf(
						"\n领域层出现包级变量: %s\n  文件: %s:%d\n  原因: 包级可变状态破坏可测试性，多租户下易造成跨租户状态泄漏\n  修复: 改为结构体字段并通过构造函数注入；若确为常量请用 const",
						name.Name, rel, fset.Position(name.Pos()).Line,
					)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// 测试运行于 test/ 目录，模块根在上一级
	return filepath.Dir(wd)
}

func goFiles(t *testing.T, root string, dirs []string) []string {
	t.Helper()
	var out []string
	for _, d := range dirs {
		base := filepath.Join(root, d)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				// poc/ 是独立 module 的丢弃型代码，不参与架构约束
				if entry.Name() == "poc" || entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".go") {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func mustRel(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(rel)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
