package knowledge

import (
	"fmt"
	"sort"
	"strings"

	"claracore-server/internal/platform"
)

// 实体类型。labels / links 用多态引用 (entity_type, entity_id) 指向它们
// （docs/02-data-model.md §3.1）。
const (
	EntityMemory = "memory"
	EntityCard   = "card"
)

// Ref 是一个多态引用。
//
// 合成一个类型而不是到处传 (typ string, id platform.ID) 两个参数：
// 两个相邻的字符串参数迟早会有人传反，而编译器不会说话。
type Ref struct {
	Type string
	ID   platform.ID
}

func (r Ref) Validate() error {
	switch r.Type {
	case EntityMemory, EntityCard:
	default:
		return fmt.Errorf("%w: 未知的实体类型 %q", ErrInvalidArgument, r.Type)
	}
	if r.ID == "" {
		return fmt.Errorf("%w: 实体 ID 为空", ErrInvalidArgument)
	}
	return nil
}

func (r Ref) String() string { return r.Type + ":" + string(r.ID) }

// Origin 记录提升来源（docs/02-data-model.md §4）。
//
// 提升是**复制而非移动**，原件保留。这一对字段让「这条团队约定
// 来自谁的哪次踩坑」可追溯，也是原件更新时提示衍生副本可能过时的依据。
type Origin struct {
	Type string
	ID   platform.ID
}

// 各字段长度上限，与迁移文件中的列宽一一对应。
//
// 在领域层拦而不是让数据库拦：MySQL 非严格模式下超长会被静默截断，
// 严格模式下报的是一句不知所云的 Data too long for column。
// 两种都不是调用方能理解的反馈。
const (
	MaxTitleLen       = 255
	MaxLabelLen       = 64
	MaxCardTypeLen    = 64
	MaxTriggerTextLen = 512
	MaxLinkKindLen    = 32
)

// normalizeLabels 清洗标签：去空白、去空项、去重，并按字典序排列。
//
// 排序不是洁癖：标签集合会进检索条件与返回值，顺序不稳定会让
// 相同内容的两次调用返回不同结果，缓存也跟着失效。
func normalizeLabels(raw []string) ([]string, error) {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if len([]rune(l)) > MaxLabelLen {
			return nil, fmt.Errorf("%w: 标签 %q 超过 %d 字", ErrInvalidArgument, l, MaxLabelLen)
		}
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	sort.Strings(out)
	return out, nil
}

// requireTitle 校验标题：必填且不超列宽。
func requireTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fmt.Errorf("%w: 缺少标题", ErrInvalidArgument)
	}
	if len([]rune(title)) > MaxTitleLen {
		return "", fmt.Errorf("%w: 标题超过 %d 字", ErrInvalidArgument, MaxTitleLen)
	}
	return title, nil
}

// requireScope 校验作用域字段齐备。
//
// 「作用域字段无默认值，缺失即报错」是铁律（Desktop 的 DEFAULT 'local-user' 教训）。
// 数据库那一层已经不给默认值了，这里再拦一次是为了让错误信息说人话——
// 否则调用方看到的是 Column 'space_id' cannot be null。
func requireScope(tenantID, spaceID, authorUserID platform.ID) error {
	var missing []string
	if tenantID == "" {
		missing = append(missing, "租户")
	}
	if spaceID == "" {
		missing = append(missing, "空间")
	}
	if authorUserID == "" {
		missing = append(missing, "作者")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: 缺少 %s", ErrInvalidArgument, strings.Join(missing, "、"))
	}
	return nil
}

// requireSpaces 校验空间集合非空。
//
// 空集合意味着调用方对任何空间都没有相应权限。让它变成一次
// `WHERE space_id IN ()` 的空查询，返回的会是「找不到」——
// 那是个误导：真相是「你没有任何可访问的空间」，两者的排查方向完全不同。
func requireSpaces(spaces []platform.ID) error {
	if len(spaces) == 0 {
		return fmt.Errorf("%w: 未提供任何可访问的空间", ErrInvalidArgument)
	}
	return nil
}
