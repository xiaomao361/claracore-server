package governance

import (
	"fmt"
	"sort"
	"time"

	"claracore-server/internal/platform"
)

// 空间类型（docs/02-data-model.md §2.2）。
const (
	SpacePersonal = "personal"
	SpaceProject  = "project"
	SpaceTeam     = "team"
)

type Space struct {
	ID          platform.ID
	TenantID    platform.ID
	Type        string
	Name        string
	OwnerUserID *platform.ID // personal/project 有主，team 为 nil
	CreatedAt   time.Time
}

func (s Space) IsOwnedBy(userID platform.ID) bool {
	return s.OwnerUserID != nil && *s.OwnerUserID == userID
}

// Permission 是 space_permissions 表里的一行显式授权。
type Permission struct {
	SpaceID   platform.ID
	UserID    platform.ID
	CanRead   bool
	CanWrite  bool
	GrantedBy platform.ID
	CreatedAt time.Time
}

// SpaceMembership 是 Store 交回的**原始事实**：一个空间，以及该用户在其上的
// 显式授权行（没有则为 nil）。
//
// Store 只负责「捞出与这个用户有关的空间」，不做任何权限判断——
// 权限规则必须只有一份，在领域层（docs/05-code-structure.md §2）。
// 一旦 SQL 里出现 `WHERE can_read = 1 OR type = 'team'` 这类判断，
// 规则就同时活在两个地方，改一处必忘另一处。
type SpaceMembership struct {
	Space      Space
	Permission *Permission
}

// SpaceAccess 是规则应用之后的结论。
type SpaceAccess struct {
	Space    Space
	CanRead  bool
	CanWrite bool
}

// SpaceScope 是「这个人能读写哪些空间」的唯一权威，也是 governance 域交给
// knowledge 域的全部内容——knowledge 不认识 user / permission 概念。
type SpaceScope struct {
	userID   platform.ID
	accesses []SpaceAccess
	personal *Space
}

// ResolveScope 把原始事实按三条规则合成权限结论。
//
// 三条规则来自 docs/02-data-model.md §2.2，注意它们**不是**同一种形态：
//
//	personal —— 仅 owner 读写，且不可授权他人。封闭性是设计约束而非默认值：
//	            它是用户敢于随手记录的前提，所以这里直接忽略 personal 上的授权行。
//	team     —— 全租户隐含可读；写需显式授权。
//	project  —— 完全显式（2026-08-06 拍板），唯一例外是创建者：
//	            owner 不该被自己建的空间锁在门外，这条规则不依赖一行可被误删的数据。
func ResolveScope(userID platform.ID, memberships []SpaceMembership) SpaceScope {
	scope := SpaceScope{userID: userID}

	for _, m := range memberships {
		access := SpaceAccess{Space: m.Space}

		switch m.Space.Type {
		case SpacePersonal:
			if !m.Space.IsOwnedBy(userID) {
				// 别人的个人空间。不可能被授权，出现即数据异常，直接跳过。
				continue
			}
			access.CanRead, access.CanWrite = true, true
			s := m.Space
			scope.personal = &s

		case SpaceTeam:
			access.CanRead = true
			if m.Permission != nil {
				access.CanWrite = m.Permission.CanWrite
			}

		case SpaceProject:
			if m.Space.IsOwnedBy(userID) {
				access.CanRead, access.CanWrite = true, true
			} else if m.Permission != nil {
				access.CanRead, access.CanWrite = m.Permission.CanRead, m.Permission.CanWrite
			}

		default:
			// 未知空间类型：不给任何权限。新增类型时必须显式处理，
			// 默认放行会让一个拼写错误变成越权。
			continue
		}

		if !access.CanRead && !access.CanWrite {
			continue
		}
		scope.accesses = append(scope.accesses, access)
	}

	sortAccesses(scope.accesses)
	return scope
}

// sortAccesses 让列表顺序稳定：team > project > personal，同类按 ID。
//
// ID 是 ULID（时间有序），所以同类内即按创建时间排。顺序稳定本身是需求：
// 返回给 agent 的空间列表每次抖动，会让 agent 的选择也跟着抖。
func sortAccesses(list []SpaceAccess) {
	rank := func(t string) int {
		switch t {
		case SpaceTeam:
			return 0
		case SpaceProject:
			return 1
		case SpacePersonal:
			return 2
		default:
			return 3
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		ri, rj := rank(list[i].Space.Type), rank(list[j].Space.Type)
		if ri != rj {
			return ri < rj
		}
		return list[i].Space.ID < list[j].Space.ID
	})
}

// All 返回全部有权访问的空间及其读写位，顺序稳定。
func (s SpaceScope) All() []SpaceAccess {
	out := make([]SpaceAccess, len(s.accesses))
	copy(out, s.accesses)
	return out
}

// Readable 返回全部可读空间 ID，用于检索粗筛的 WHERE space_id IN (...)。
func (s SpaceScope) Readable() []platform.ID {
	var out []platform.ID
	for _, a := range s.accesses {
		if a.CanRead {
			out = append(out, a.Space.ID)
		}
	}
	return out
}

func (s SpaceScope) CanRead(id platform.ID) bool {
	for _, a := range s.accesses {
		if a.Space.ID == id {
			return a.CanRead
		}
	}
	return false
}

func (s SpaceScope) CanWrite(id platform.ID) bool {
	for _, a := range s.accesses {
		if a.Space.ID == id {
			return a.CanWrite
		}
	}
	return false
}

// Space 按 ID 取空间信息。检索结果必须标注来源空间
// （docs/03-mcp-contract.md §3.2），调用方据此填充返回值。
func (s SpaceScope) Space(id platform.ID) (Space, bool) {
	for _, a := range s.accesses {
		if a.Space.ID == id {
			return a.Space, true
		}
	}
	return Space{}, false
}

// PersonalSpace 返回调用者的个人空间，即写入类工具的默认目标。
func (s SpaceScope) PersonalSpace() (Space, error) {
	if s.personal == nil {
		return Space{}, ErrNoPersonalSpace
	}
	return *s.personal, nil
}

// ResolveWriteTarget 把可选的 space 参数解析为确定的 space_id。
//
// 未指定即写入个人空间（docs/03-mcp-contract.md §3.1）——随手记录不该有心智负担，
// 要共享再走 promote。解析发生在这里，所以传到存储层的 space_id 永远是显式值，
// 与「作用域字段不给默认值」的数据库约束不冲突。
func (s SpaceScope) ResolveWriteTarget(requested platform.ID) (Space, error) {
	if requested == "" {
		return s.PersonalSpace()
	}
	space, ok := s.Space(requested)
	if !ok || !s.CanWrite(requested) {
		// 无权限与不存在返回同一个错误：区分二者等于泄露「这个空间存在」。
		return Space{}, fmt.Errorf("%w: %s", ErrSpaceForbidden, requested)
	}
	return space, nil
}

// ResolveReadTargets 把可选的 spaces 参数解析为确定的空间集合。
//
// 未指定即全部可读空间（docs/03-mcp-contract.md §3.1）。
//
// 指定了其中某个不可读时**报错而非静默跳过**：静默过滤会让 agent 以为
// 自己搜过了那个空间并得到「没有结果」，从而做出错误判断。
// 检索为空和无权检索，对调用方是完全不同的两件事。
func (s SpaceScope) ResolveReadTargets(requested []platform.ID) ([]platform.ID, error) {
	if len(requested) == 0 {
		readable := s.Readable()
		if len(readable) == 0 {
			return nil, ErrNoPersonalSpace
		}
		return readable, nil
	}

	out := make([]platform.ID, 0, len(requested))
	seen := make(map[platform.ID]bool, len(requested))
	for _, id := range requested {
		if !s.CanRead(id) {
			return nil, fmt.Errorf("%w: %s", ErrSpaceForbidden, id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}
