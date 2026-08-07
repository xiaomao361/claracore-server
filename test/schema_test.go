// schema_test.go 是 docs/05-code-structure.md §5.2 的可执行形式。
//
// 02-data-model.md 里那些"硬规则"如果只写在文档里，三个月后新增的迁移文件
// 就会悄悄破坏它们。这里把每一条变成 CI 里会红的检查。
package test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	reLineComment = regexp.MustCompile(`--[^\n]*`)
	reBlockCmt    = regexp.MustCompile(`(?s)/\*.*?\*/`)

	reCreateTrigger = regexp.MustCompile(`(?i)\bCREATE\s+(OR\s+REPLACE\s+)?TRIGGER\b`)
	reCreateTable   = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?` + "`?" + `(\w+)` + "`?" + `\s*\((.*?)\)\s*ENGINE`)
	// 列类型 TIMESTAMP（排除 CURRENT_TIMESTAMP 这类函数用法）
	reTimestampCol = regexp.MustCompile(`(?i)(^|\s)` + "`?" + `\w+` + "`?" + `\s+TIMESTAMP\b`)
	// 作用域字段带默认值
	reScopeDefault = regexp.MustCompile(`(?i)` + "`?" + `(tenant_id|space_id)` + "`?" + `[^,\n]*\bDEFAULT\b`)
	// 定长 CHAR 列（ULID 主键 / SHA-256 hash）。前置的 [^A-Za-z_] 用于排除 VARCHAR。
	reFixedChar = regexp.MustCompile(`(?i)(^|[^A-Za-z_` + "`" + `])CHAR\(\d+\)`)
	// FULLTEXT 索引定义（可能跨行，故匹配到右括号为止）
	reFulltext = regexp.MustCompile(`(?is)FULLTEXT\s+(KEY|INDEX)\s+` + "`?" + `\w+` + "`?" + `\s*\([^)]*\)[^,\n]*`)
)

func TestMigrationsObeySchemaRules(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "migrations")

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("migrations/ 尚不存在")
		}
		t.Fatal(err)
	}

	var sqlFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			sqlFiles = append(sqlFiles, filepath.Join(dir, e.Name()))
		}
	}
	if len(sqlFiles) == 0 {
		t.Skip("尚无迁移文件，骨架阶段跳过")
	}

	for _, path := range sqlFiles {
		name := filepath.Base(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sql := stripComments(string(raw))

		// 规则 1：禁止触发器
		if loc := reCreateTrigger.FindString(sql); loc != "" {
			t.Errorf("\n[%s] 出现 CREATE TRIGGER\n  原因: 业务逻辑必须在应用层，触发器不可跨数据库移植（Desktop 教训）\n  修复: 把逻辑上移到 domain 层", name)
		}

		// 规则 2：禁止 TIMESTAMP 列类型
		for line := range strings.SplitSeq(sql, "\n") {
			if reTimestampCol.MatchString(line) && !strings.Contains(strings.ToUpper(line), "CURRENT_TIMESTAMP") {
				t.Errorf("\n[%s] 使用了 TIMESTAMP 列类型\n  行: %s\n  原因: TIMESTAMP 有隐式时区转换且存在 2038 问题\n  修复: 改用 DATETIME(3)，统一存 UTC", name, strings.TrimSpace(line))
			}
		}

		// 规则 3：作用域字段不得有默认值
		if m := reScopeDefault.FindStringSubmatch(sql); m != nil {
			t.Errorf("\n[%s] 作用域字段 %s 带了 DEFAULT\n  原因: 默认值会让\"忘记传租户/空间\"变成静默 bug（Desktop 的 DEFAULT 'local-user' 教训）\n  修复: 去掉 DEFAULT，缺失即报错", name, m[1])
		}

		// 规则 4：每张表必须有 created_at
		for _, m := range reCreateTable.FindAllStringSubmatch(sql, -1) {
			table, body := m[2], m[3]
			if !strings.Contains(strings.ToLower(body), "created_at") {
				t.Errorf("\n[%s] 表 %s 缺少 created_at\n  原因: 无法追溯记录产生时间\n  修复: 增加 created_at DATETIME(3) NOT NULL", name, table)
			}
		}

		// 规则 5：字符集必须是 utf8mb4
		if strings.Contains(strings.ToUpper(sql), "ENGINE") && !strings.Contains(strings.ToLower(sql), "utf8mb4") {
			t.Errorf("\n[%s] 建表未声明 utf8mb4\n  原因: 中文与 emoji 需要 4 字节字符集", name)
		}

		// 规则 6：定长 CHAR 列必须显式 ascii_bin
		//
		// ID 与 hash 都是纯 ASCII。表默认的 utf8mb4 排序规则下，CHAR(26) 在索引里
		// 按 4 字节/字符计（104 字节 vs 26 字节），且比较要走大小写折叠——
		// ULID 是大写 Crockford base32，折叠既无意义又拖慢每一次主键查找。
		for line := range strings.SplitSeq(sql, "\n") {
			if !reFixedChar.MatchString(line) {
				continue
			}
			if !strings.Contains(strings.ToLower(line), "ascii_bin") {
				t.Errorf("\n[%s] 定长 CHAR 列未声明 ascii_bin\n  行: %s\n  原因: ID/hash 是纯 ASCII，utf8mb4 排序规则让索引膨胀 4 倍并引入无意义的大小写折叠\n  修复: 加 CHARACTER SET ascii COLLATE ascii_bin", name, strings.TrimSpace(line))
			}
		}

		// 规则 7：FULLTEXT 索引必须带 WITH PARSER ngram
		//
		// 默认分词器按空格切词，对中文等于整段当一个词——
		// 「MySQL 连接池配置」检索「连接池」会一条都搜不到。
		// 最坏的地方在于它不报错：索引建得好好的，MATCH 也能跑，只是永远返回空。
		// 这种静默失效不可能在代码评审里被看出来，只能靠这条检查。
		for _, m := range reFulltext.FindAllString(sql, -1) {
			if !strings.Contains(strings.ToLower(m), "with parser ngram") {
				t.Errorf("\n[%s] FULLTEXT 索引未指定 ngram 分词器\n  定义: %s\n  原因: 默认分词器按空格切词，中文整段被当作一个词——检索不报错，只是永远返回空\n  修复: 索引定义末尾加 WITH PARSER ngram", name, strings.Join(strings.Fields(m), " "))
			}
		}
	}
}

// TestMigrationsAreSequential 确保迁移文件编号连续且无重复。
// 编号跳号或重复会让"这个环境到底跑到哪一步"变成猜谜。
func TestMigrationsAreSequential(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "migrations")

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("migrations/ 尚不存在")
		}
		t.Fatal(err)
	}

	rePrefix := regexp.MustCompile(`^(\d{4})_`)
	seen := map[string]string{}
	var nums []string

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := rePrefix.FindStringSubmatch(e.Name())
		if m == nil {
			t.Errorf("迁移文件命名不符合规范: %s（应为 NNNN_描述.sql，如 0001_governance.sql）", e.Name())
			continue
		}
		if prev, dup := seen[m[1]]; dup {
			t.Errorf("迁移编号重复: %s 与 %s 都是 %s", prev, e.Name(), m[1])
		}
		seen[m[1]] = e.Name()
		nums = append(nums, m[1])
	}

	if len(nums) == 0 {
		t.Skip("尚无迁移文件")
	}
	for i := 1; i <= len(nums); i++ {
		key := padNum(i)
		if _, ok := seen[key]; !ok {
			t.Errorf("迁移编号不连续：缺少 %s", key)
		}
	}
}

func stripComments(s string) string {
	s = reBlockCmt.ReplaceAllString(s, "")
	return reLineComment.ReplaceAllString(s, "")
}

func padNum(i int) string {
	s := ""
	for _, d := range []int{1000, 100, 10, 1} {
		s += string(rune('0' + (i/d)%10))
	}
	return s
}
