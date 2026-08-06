// Package platform 提供横切基础设施：时钟、ID 生成、错误类型。
//
// 领域层不直接调用 time.Now()，而是依赖 Clock 接口
// ——否则任何与时间相关的逻辑都无法测试（docs/05-code-structure.md §6.2）。
package platform

import "time"

// Clock 抽象当前时间。所有时间一律 UTC。
type Clock interface {
	Now() time.Time
}

// SystemClock 是生产实现，始终返回 UTC 时间。
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// FixedClock 用于测试：时间静止在给定时刻。
type FixedClock struct {
	T time.Time
}

func (c FixedClock) Now() time.Time { return c.T.UTC() }

// OffsetClock 用于测试：在基准时刻上按调用次数递增，
// 便于验证依赖时间顺序的逻辑（如乐观锁、召回排序）。
type OffsetClock struct {
	Base time.Time
	Step time.Duration
	n    int
}

func (c *OffsetClock) Now() time.Time {
	t := c.Base.Add(time.Duration(c.n) * c.Step)
	c.n++
	return t.UTC()
}
