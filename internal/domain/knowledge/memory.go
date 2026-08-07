package knowledge

import (
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/platform"
)

// Memory 的生命周期状态。
//
// deleted 是软删除：memoria_delete 不真删行（docs/03-mcp-contract.md §4.1）。
// 硬删会让指向它的 link 变成悬空引用，而多态引用没有外键能拦住这件事；
// 更要紧的是「误删一条记忆」在个人知识库里是不可接受的。
const (
	MemoryActive     = "active"
	MemoryArchived   = "archived"
	MemorySuperseded = "superseded"
	MemoryDeleted    = "deleted"
)

// Memory 是一条记忆——与 Card 并列的一等公民，不是它的父类型。
type Memory struct {
	ID            platform.ID
	TenantID      platform.ID
	SpaceID       platform.ID
	AuthorUserID  platform.ID
	AuthorAgentID *platform.ID // 人工创建时为 nil
	Title         string
	Body          string
	Status        string
	CopiedFrom    *Origin
	Version       int
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// Labels 由 Store 关联加载。取单条时填充，批量粗筛时可能为 nil——
	// 那是「没加载」而不是「没有标签」，调用方按需再取。
	Labels []string
}

// IsVisible 表示这条记忆是否应出现在默认检索结果里。
//
// archived / superseded 是「还在，但别再主动想起它」；deleted 是软删除。
// 三者都不进默认检索，但都还能按 ID 取到——这正是软删除的意义。
func (m Memory) IsVisible() bool { return m.Status == MemoryActive }

func validMemoryStatus(s string) bool {
	switch s {
	case MemoryActive, MemoryArchived, MemorySuperseded, MemoryDeleted:
		return true
	}
	return false
}

// validate 是写入前的最后一道校验，Create 与 Update 共用。
//
// 放在实体上而不是各个用例里：两条路径各写一份校验，
// 迟早会出现「创建时拦住了、更新时能绕过」的不对称漏洞。
func (m *Memory) validate() error {
	if err := requireScope(m.TenantID, m.SpaceID, m.AuthorUserID); err != nil {
		return err
	}
	title, err := requireTitle(m.Title)
	if err != nil {
		return err
	}
	m.Title = title

	if strings.TrimSpace(m.Body) == "" {
		return fmt.Errorf("%w: 记忆正文不能为空", ErrInvalidArgument)
	}
	if !validMemoryStatus(m.Status) {
		return fmt.Errorf("%w: 记忆状态取值非法 %q", ErrInvalidArgument, m.Status)
	}
	labels, err := normalizeLabels(m.Labels)
	if err != nil {
		return err
	}
	m.Labels = labels
	return nil
}
