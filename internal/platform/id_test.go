package platform

import (
	"sort"
	"testing"
	"time"
)

func TestIDFormat(t *testing.T) {
	g := NewIDGenerator(SystemClock{})
	id := g.New()

	if len(id) != ulidLen {
		t.Fatalf("长度应为 %d，实际 %d: %q", ulidLen, len(id), id)
	}
	for i := 0; i < len(id); i++ {
		if indexOf(byte(id[i])) < 0 {
			t.Fatalf("出现非 Crockford Base32 字符 %q（位置 %d）: %s", id[i], i, id)
		}
	}
}

// TestIDTimestampRoundTrip 是 ULID 编码正确性的核心验证：
// 编码后再解码，必须回到同一毫秒。
func TestIDTimestampRoundTrip(t *testing.T) {
	cases := []time.Time{
		time.UnixMilli(0).UTC(),
		time.Date(2026, 8, 5, 16, 0, 0, 0, time.UTC),
		time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2100, 12, 31, 23, 59, 59, 0, time.UTC),
	}

	for _, want := range cases {
		g := NewIDGenerator(FixedClock{T: want})
		id := g.New()

		got, ok := id.Timestamp()
		if !ok {
			t.Fatalf("解码失败: %s", id)
		}
		if !got.Equal(want.Truncate(time.Millisecond)) {
			t.Errorf("时间戳往返不一致\n  期望: %s\n  实际: %s\n  ID: %s", want, got, id)
		}
	}
}

// TestIDLexicographicOrder 验证 ULID 的核心承诺：
// 字典序 == 时间序。这是选它做主键的理由，必须成立。
func TestIDLexicographicOrder(t *testing.T) {
	base := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	clock := &OffsetClock{Base: base, Step: time.Millisecond}
	g := NewIDGenerator(clock)

	var ids []string
	for i := 0; i < 200; i++ {
		ids = append(ids, string(g.New()))
	}

	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)

	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("字典序与生成顺序不一致（位置 %d）\n  生成: %s\n  排序: %s", i, ids[i], sorted[i])
		}
	}
}

// TestIDMonotonicWithinSameMillisecond 验证同毫秒内的单调性。
// 批量创建时若同毫秒生成的 ID 无序，同批记录的顺序会随机。
func TestIDMonotonicWithinSameMillisecond(t *testing.T) {
	fixed := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	g := NewIDGenerator(FixedClock{T: fixed})

	prev := string(g.New())
	for i := 0; i < 1000; i++ {
		cur := string(g.New())
		if cur <= prev {
			t.Fatalf("同毫秒内未单调递增（第 %d 次）\n  前: %s\n  后: %s", i, prev, cur)
		}
		prev = cur
	}
}

func TestIDUniqueness(t *testing.T) {
	g := NewIDGenerator(SystemClock{})
	seen := make(map[ID]bool, 10000)
	for i := 0; i < 10000; i++ {
		id := g.New()
		if seen[id] {
			t.Fatalf("生成重复 ID: %s", id)
		}
		seen[id] = true
	}
}

func TestIDRejectsMalformed(t *testing.T) {
	bad := []ID{
		"",
		"TOOSHORT",
		"IIIIIIIIIIIIIIIIIIIIIIIIII", // I 不在字母表内
		"ZZZZZZZZZZZZZZZZZZZZZZZZZZ", // 补零位非零
	}
	for _, id := range bad {
		if _, ok := id.Timestamp(); ok {
			t.Errorf("非法 ID 被接受: %q", id)
		}
	}
}
