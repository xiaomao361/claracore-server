package mcp

import (
	"errors"
	"fmt"
	"log/slog"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
)

// 对外错误码。agent 按码分支，不解析文案——文案会变，码不会。
const (
	codeForbidden = "FORBIDDEN"
	codeNotFound  = "NOT_FOUND"
	codeInvalid   = "INVALID_ARGUMENT"
	codeConflict  = "VERSION_CONFLICT"
	codeDataFault = "DATA_FAULT"
	codeInternal  = "INTERNAL"
)

// toolError 是**唯一**的领域错误 → 协议错误映射点
// （docs/05-code-structure.md §6.1：映射表集中在一处，不散落）。
//
// 散落的映射表会以两种方式坏掉：新工具忘了映射，于是内部错误原文泄露给调用方；
// 或者两处对同一个错误给出不同的码，agent 的分支逻辑随之失效。
//
// 认证阶段的错误不走这里——那三个 token 错误必须在 verifyToken 里
// 就收敛成同一个 401，区分它们等于告诉攻击者哪些 token 曾经有效。
func toolError(logger *slog.Logger, tool string, err error) error {
	switch {
	case err == nil:
		return nil

	// 无权限与不存在合并为同一种响应：区分二者等于泄露「这个空间存在」。
	// 领域层已经在 ResolveWriteTarget / ResolveReadTargets 里做了同样的合并，
	// 这里保持一致。
	case errors.Is(err, governance.ErrSpaceForbidden),
		errors.Is(err, governance.ErrSpaceNotFound):
		return protocolError(codeForbidden, "无该空间的访问权限")

	case errors.Is(err, governance.ErrAlreadyExists):
		// 这个可以说清楚：重名是调用方能自己修正的事，藏着只会让人反复重试。
		return protocolError(codeInvalid, "目标已存在")

	case errors.Is(err, governance.ErrInvalidArgument):
		// 参数错误带上原文：它描述的是调用方自己传的东西，不涉及他人数据。
		return protocolError(codeInvalid, err.Error())

	case errors.Is(err, governance.ErrUserNotFound),
		errors.Is(err, governance.ErrAgentNotFound):
		return protocolError(codeNotFound, "目标不存在")

	// 个人空间缺失是数据损坏，不是正常业务状态（见 governance.ErrNoPersonalSpace）。
	// 给它一个独立的码：让运维一眼看出这是数据问题，而不是又一次权限配置失误。
	case errors.Is(err, governance.ErrNoPersonalSpace):
		logger.Error("数据异常：用户没有个人空间", "tool", tool, "error", err)
		return protocolError(codeDataFault, "账号数据异常，请联系管理员")

	// 认证类错误不该出现在工具执行阶段——能走到这里说明中间件被绕过了。
	case errors.Is(err, governance.ErrTokenNotFound),
		errors.Is(err, governance.ErrTokenRevoked),
		errors.Is(err, governance.ErrTokenExpired),
		errors.Is(err, governance.ErrTokenMalformed),
		errors.Is(err, governance.ErrUserDisabled),
		errors.Is(err, governance.ErrTenantInactive):
		logger.Error("认证错误出现在工具执行阶段，中间件链路可能被绕过", "tool", tool, "error", err)
		return protocolError(codeInternal, "服务内部错误")

	// --- 核心域 ---
	//
	// knowledge 与 governance 各有一套哨兵，**刻意不合并**：两个域的
	// ErrNotFound 含义不同（一个是"这条内容你看不到"，一个是"这个用户不存在"），
	// 合并之后就没法给它们不同的措辞了。

	// 乐观锁冲突要把当前版本号带出去，否则 agent 只知道撞车了，
	// 还得再发一次 get 才能重试——而那次 get 之后可能再撞一次。
	//
	// 契约 §3.4 描述的是一个带 currentVersion 字段的 JSON 对象，
	// 但 SDK 的错误通道只能传字符串（tool 返回 error → isError 文本）。
	// 折中成一句格式稳定的话：码在前，版本号在后。
	case errors.Is(err, knowledge.ErrVersionConflict):
		var conflict *knowledge.VersionConflictError
		if errors.As(err, &conflict) {
			return protocolError(codeConflict,
				fmt.Sprintf("内容已被他人修改，当前版本 %d。请重新 get 后基于最新内容再改", conflict.Current))
		}
		return protocolError(codeConflict, "内容已被他人修改，请重新 get 后再改")

	// 与 governance 一样合并「不存在」与「无权访问」：区分二者等于让任何人
	// 都能用一个 ID 探测出「这条内容存在于某个我看不见的空间」。
	case errors.Is(err, knowledge.ErrNotFound):
		return protocolError(codeNotFound, "内容不存在或无权访问")

	case errors.Is(err, knowledge.ErrAlreadyExists):
		return protocolError(codeInvalid, "这条关联已经存在")

	case errors.Is(err, knowledge.ErrInvalidArgument):
		// 带原文：它描述的是调用方自己传的东西，不涉及他人数据。
		return protocolError(codeInvalid, err.Error())

	// 与 governance.checkAuthConsistency 同一性质：正常情况下永不触发，
	// 触发了就是数据损坏导致的跨租户越权。必须留下日志。
	case errors.Is(err, knowledge.ErrTenantMismatch):
		logger.Error("数据异常：取到的内容不属于当前租户", "tool", tool, "error", err)
		return protocolError(codeDataFault, "数据异常，请联系管理员")

	case errors.Is(err, errNoIdentity):
		logger.Error("装配错误：tool handler 拿不到身份", "tool", tool)
		return protocolError(codeInternal, "服务内部错误")

	default:
		// 未识别的错误一律吞掉原文。SQL 错误里含表名列名，
		// 超时错误里含内网地址——这些都不该出现在 agent 的上下文里。
		logger.Error("未映射的错误", "tool", tool, "error", err)
		return protocolError(codeInternal, "服务内部错误")
	}
}

// protocolError 是回给调用方的错误形态：一个码 + 一句不泄露内部信息的话。
func protocolError(code, message string) error {
	return fmt.Errorf("%s: %s", code, message)
}
