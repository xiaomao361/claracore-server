package mysql

import (
	"strings"
	"testing"
)

// splitStatements 是迁移执行器里唯一有分支逻辑的部分，也是最容易
// 「看起来对、遇到某个字面量就错」的部分。它不需要数据库，必须有测试。
func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "普通两条语句",
			in:   "CREATE TABLE a (id INT);\nCREATE TABLE b (id INT);",
			want: []string{"CREATE TABLE a (id INT)", "CREATE TABLE b (id INT)"},
		},
		{
			name: "末尾无分号",
			in:   "SELECT 1",
			want: []string{"SELECT 1"},
		},
		{
			name: "行注释里的分号不算分隔",
			in:   "-- 先建表; 再说\nCREATE TABLE a (id INT);",
			want: []string{"CREATE TABLE a (id INT)"},
		},
		{
			name: "# 行注释",
			in:   "# 注释; 里有分号\nSELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "块注释里的分号不算分隔",
			in:   "/* a; b */ SELECT 1;",
			want: []string{"SELECT 1"},
		},
		{
			name: "字符串字面量里的分号不算分隔",
			in:   "INSERT INTO t VALUES ('a;b');",
			want: []string{"INSERT INTO t VALUES ('a;b')"},
		},
		{
			name: "字符串内的两个单引号是转义而非闭合",
			in:   "INSERT INTO t VALUES ('it''s; fine');",
			want: []string{"INSERT INTO t VALUES ('it''s; fine')"},
		},
		{
			name: "反斜杠转义的引号",
			in:   `INSERT INTO t VALUES ('a\'; b');`,
			want: []string{`INSERT INTO t VALUES ('a\'; b')`},
		},
		{
			name: "反引号标识符内的分号",
			in:   "SELECT `we;ird` FROM t;",
			want: []string{"SELECT `we;ird` FROM t"},
		},
		{
			name: "生成列表达式里的括号与逗号",
			in:   "CREATE TABLE s (t VARCHAR(16), g CHAR(26) GENERATED ALWAYS AS (IF(t = 'personal', o, NULL)) VIRTUAL);",
			want: []string{"CREATE TABLE s (t VARCHAR(16), g CHAR(26) GENERATED ALWAYS AS (IF(t = 'personal', o, NULL)) VIRTUAL)"},
		},
		{
			name: "空语句与多余分号被丢弃",
			in:   ";;\nSELECT 1;;\n\n;",
			want: []string{"SELECT 1"},
		},
		{
			name: "全是注释",
			in:   "-- 什么也没有\n/* 真的没有 */\n",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitStatements(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("语句数不符\n  got  (%d): %q\n  want (%d): %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("第 %d 条不符\n  got:  %q\n  want: %q", i+1, got[i], tc.want[i])
				}
			}
		})
	}
}

// 真实迁移文件必须能被正确拆分——这条把 splitStatements 和 migrations/ 绑在一起，
// 将来某个迁移用了奇怪的字面量，这里会先红。
func TestSplitRealGovernanceMigration(t *testing.T) {
	const sql = `
CREATE TABLE tenants (
  id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE spaces (
  type VARCHAR(16) NOT NULL,
  personal_owner_id CHAR(26) GENERATED ALWAYS AS (IF(type = 'personal', owner_user_id, NULL)) VIRTUAL,
  PRIMARY KEY (type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`
	got := splitStatements(sql)
	if len(got) != 2 {
		t.Fatalf("期望 2 条语句，得到 %d 条: %q", len(got), got)
	}
}

func TestCompareMigrationSetsRequiresExactBinaryDatabaseMatch(t *testing.T) {
	all := []migration{
		{version: "0001", name: "0001_governance.sql", checksum: "checksum-one"},
		{version: "0002", name: "0002_knowledge.sql", checksum: "checksum-two"},
	}

	t.Run("current", func(t *testing.T) {
		got, err := compareMigrationSets(all, map[string]string{
			"0001": "checksum-one",
			"0002": "checksum-two",
		})
		if err != nil {
			t.Fatalf("当前结构不应失败: %v", err)
		}
		if got.LatestVersion != "0002" || got.Expected != 2 || got.Applied != 2 {
			t.Fatalf("状态摘要不符: %+v", got)
		}
	})

	cases := []struct {
		name    string
		applied map[string]string
		want    string
	}{
		{
			name:    "missing migration",
			applied: map[string]string{"0001": "checksum-one"},
			want:    "缺少 0002_knowledge.sql",
		},
		{
			name: "modified migration",
			applied: map[string]string{
				"0001": "checksum-one",
				"0002": "different-checksum",
			},
			want: "校验和不一致",
		},
		{
			name: "database ahead of binary",
			applied: map[string]string{
				"0001": "checksum-one",
				"0002": "checksum-two",
				"0003": "checksum-three",
			},
			want: "不能用旧二进制执行迁移或接流量",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compareMigrationSets(all, tc.applied)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望包含 %q 的错误，得到 %v", tc.want, err)
			}
		})
	}
}

func TestPlanMigrationSetsRequiresContiguousTrustedPrefix(t *testing.T) {
	all := []migration{
		{version: "0001", name: "0001_governance.sql", checksum: "checksum-one"},
		{version: "0002", name: "0002_knowledge.sql", checksum: "checksum-two"},
		{version: "0003", name: "0003_activity.sql", checksum: "checksum-three"},
	}

	plan, err := planMigrationSets(all, map[string]string{
		"0001": "checksum-one",
		"0002": "checksum-two",
	})
	if err != nil {
		t.Fatalf("valid pending suffix should be plannable: %v", err)
	}
	if plan.CurrentVersion != "0002" || plan.TargetVersion != "0003" || plan.NextMigration != "0003_activity.sql" ||
		plan.Applied != 2 || plan.Expected != 3 || plan.Pending != 1 {
		t.Fatalf("unexpected upgrade plan: %+v", plan)
	}

	fresh, err := planMigrationSets(all, map[string]string{})
	if err != nil {
		t.Fatalf("empty fresh ledger should plan the complete suffix: %v", err)
	}
	if fresh.CurrentVersion != "" || fresh.Pending != 3 || fresh.NextMigration != "0001_governance.sql" {
		t.Fatalf("unexpected fresh install plan: %+v", fresh)
	}

	_, err = planMigrationSets(all, map[string]string{
		"0001": "checksum-one",
		"0003": "checksum-three",
	})
	if err == nil || !strings.Contains(err.Error(), "账本不连续") || !strings.Contains(err.Error(), "拒绝自动补洞") {
		t.Fatalf("migration gap must fail closed, got %v", err)
	}
}
