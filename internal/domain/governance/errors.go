// Package governance 是治理域：谁是谁，谁能干什么。
//
// 本包不依赖任何基础设施——没有 SQL、没有 HTTP、没有环境变量。
// 存储通过本包定义的 Store 接口反向注入（docs/05-code-structure.md §3.1）。
//
// 核心产出是 SpaceScope：把「这个 token 背后的人能读写哪些空间」解析成一个值，
// 供 knowledge 域消费。knowledge 不认识 user / permission 概念。
package governance

import "errors"

// 领域错误一律用哨兵错误，由 Host 层映射为协议错误
// （MCP 错误码 / HTTP 状态码），映射表集中一处（docs/05-code-structure.md §6.1）。
var (
	// ErrTokenNotFound 表示 token 不存在。
	//
	// 对外必须与 ErrTokenRevoked / ErrTokenExpired 映射为同一种响应：
	// 区分「不存在」与「已吊销」等于告诉攻击者哪些 token 曾经有效。
	ErrTokenNotFound  = errors.New("governance: token 不存在")
	ErrTokenRevoked   = errors.New("governance: token 已吊销")
	ErrTokenExpired   = errors.New("governance: token 已过期")
	ErrTokenMalformed = errors.New("governance: token 格式非法")

	ErrUserNotFound   = errors.New("governance: 用户不存在")
	ErrUserDisabled   = errors.New("governance: 用户已停用")
	ErrAgentNotFound  = errors.New("governance: agent 不存在")
	ErrTenantInactive = errors.New("governance: 租户未启用")

	// ErrSpaceForbidden 表示对目标空间无所需权限。
	ErrSpaceForbidden = errors.New("governance: 无该空间的访问权限")
	ErrSpaceNotFound  = errors.New("governance: 空间不存在")

	// ErrNoPersonalSpace 表示用户没有个人空间。
	//
	// 这是数据损坏而非正常业务状态：个人空间在创建用户时一并创建，
	// 缺失说明创建流程被中断过。不静默补建——补建会掩盖问题，
	// 且并发下可能建出第二个（数据库唯一约束会拦，但那时报错更难懂）。
	ErrNoPersonalSpace = errors.New("governance: 用户没有个人空间")

	// ErrAlreadyExists 表示违反了一条唯一性约束：账号重名、agent slug 重复、
	// 同一用户的第二个个人空间、同一租户的第二个团队空间。
	//
	// 这些约束由数据库强制（应用层的 SELECT-then-INSERT 在并发下必然失效），
	// 因此判定发生在存储层，但它描述的是**业务事实**而非存储细节——
	// 「这个账号已经有人用了」是要展示给人看的话。哨兵定义在这里，
	// 是为了让 Host 层不必 import 数据库驱动去判断错误码
	// （那会撞上「Host 不得直连基础设施」的架构约束）。
	ErrAlreadyExists = errors.New("governance: 目标已存在")
	// ErrAlreadyProvisioned 表示这个单部署单元已经有租户，不能再次执行
	// 首次初始化。它与普通的账号重名不同：操作者需要停止初始化，而不是换名重试。
	ErrAlreadyProvisioned = errors.New("governance: 部署单元已经初始化")

	ErrInvalidArgument = errors.New("governance: 参数非法")

	// ErrInvalidCredentials 是 Web 登录失败的**唯一**出口。
	//
	// 账号不存在、密码不对、账号已停用、租户已停用——全部收敛到这一个。
	// 区分它们等于提供一个账号枚举接口：用不同的用户名试一遍就能
	// 得到一份有效账号清单。「已停用」尤其不能说，那等于告诉对方
	// 「这个人存在，只是被关了」。
	//
	// 与 MCP 那侧三个 token 错误收敛成同一个 401 是同一条纪律。
	ErrInvalidCredentials = errors.New("governance: 账号或密码不正确")

	// ErrSessionInvalid 表示会话不存在、已过期，或它背后的账号已停用。
	// 同样不区分——对浏览器来说下一步都是「回登录页」。
	ErrSessionInvalid = errors.New("governance: 会话无效或已过期")
)
