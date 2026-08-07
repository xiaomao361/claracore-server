// Package knowledge 是核心域：记什么，什么时候该想起来。
//
// 本包**不认识 user / permission 概念**，也不 import governance
// （由 test/arch_test.go 强制）。权限规则只有一份，在 governance.ResolveScope；
// 应用层把它解析成确定的空间 ID 集合后传进来，本包只回答
// 「在这批空间里，这条内容是什么」。
//
// 这不是形式上的解耦：一旦 knowledge 能看见 Permission，
// 「顺手在这儿也判断一下 can_write」就会变成第二份权限规则，
// 而两份规则永远只会改对其中一份。
package knowledge

import "errors"

// 领域错误一律用哨兵错误，由 Host 层映射为协议错误，映射表集中一处
// （docs/05-code-structure.md §6.1）。
var (
	// ErrNotFound 表示在调用方有权访问的空间里找不到该实体。
	//
	// 「不存在」与「存在但无权访问」刻意合并为同一个错误：区分二者
	// 等于让任何人都能用一个 ID 探测出「这条内容存在于某个我看不见的空间」。
	// 与 governance.ErrSpaceForbidden 的取舍一致。
	ErrNotFound = errors.New("knowledge: 内容不存在或无权访问")

	// ErrVersionConflict 表示乐观锁冲突：这条内容在你读取之后被人改过。
	//
	// 绝不静默覆盖（docs/02-data-model.md §6）。冲突要交回调用方决定
	// 重试还是合并——agent 有上下文，存储层没有。
	ErrVersionConflict = errors.New("knowledge: 内容已被他人修改")

	// ErrAlreadyExists 表示违反唯一性约束，目前只有一处：
	// 同两个实体之间同一 kind 的关联重复创建。
	ErrAlreadyExists = errors.New("knowledge: 目标已存在")

	// ErrTenantMismatch 表示取到的行不属于声明的租户。
	//
	// 与 governance.checkAuthConsistency 同一性质：正常情况下永远不触发，
	// 它防的是错误的迁移或补数据造成的跨租户越权。别当冗余代码删掉。
	ErrTenantMismatch = errors.New("knowledge: 租户不一致")

	ErrInvalidArgument = errors.New("knowledge: 参数非法")
)
