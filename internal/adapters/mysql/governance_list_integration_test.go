//go:build integration

package mysql_test

import (
	"context"
	"testing"
)

func TestGovernanceListIndexesExistOnConfiguredDatabase(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	want := map[string][]string{
		"idx_users_tenant_page":       {"tenant_id", "id"},
		"idx_tokens_tenant_user_page": {"tenant_id", "user_id", "id"},
		"idx_spaces_tenant_page":      {"tenant_id", "id"},
	}
	for index, columns := range want {
		rows, err := db.QueryContext(ctx, `SELECT column_name FROM information_schema.statistics
			WHERE table_schema=DATABASE() AND index_name=? ORDER BY seq_in_index`, index)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			got = append(got, column)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(columns) {
			t.Fatalf("index %s columns=%v want=%v", index, got, columns)
		}
		for i := range columns {
			if got[i] != columns[i] {
				t.Fatalf("index %s columns=%v want=%v", index, got, columns)
			}
		}
	}
}
