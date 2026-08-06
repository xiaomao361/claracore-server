// Package mcp 是 MCP Host：协议解码、身份装配、结果编码。
//
// 这一层不含业务规则，也不碰存储（docs/05-code-structure.md §4）。
// 它只做三件事：把 bearer token 换成身份、把工具参数交给领域层、
// 把领域层的结果与错误编码成协议响应。
package mcp

import (
	"context"
	"errors"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"claracore-server/internal/domain/governance"
)

// governanceService 是本层对治理域的**全部**需求。
//
// 接口定义在消费方（docs/05-code-structure.md §3.1）：Host 只需要这两个方法，
// 不需要认识 governance.Service 的其余部分。附带的好处是本包的测试
// 不需要数据库——协议层的 bug 与存储层无关，用真库测等于把两件事绑死。
type governanceService interface {
	Authenticate(ctx context.Context, plaintext string) (governance.Identity, error)
	Scope(ctx context.Context, id governance.Identity) (governance.SpaceScope, error)
}

// identityKey 是身份在 context 中的键。
//
// 用未导出的空结构体类型作键，外部包无法伪造——身份只能由本包的
// 认证中间件放进去（铁律 4：身份只来自 token）。
type identityKey struct{}

// extraIdentityKey 是 auth.TokenInfo.Extra 里存放身份的键。
//
// 走 Extra 而不是把 Identity 拆成几个字符串字段：拆开再重组会在中间丢类型，
// PoC 里那段 asString/asStrings 的样板正是这么来的。
const extraIdentityKey = "governance.Identity"

func identityFrom(ctx context.Context) (governance.Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(governance.Identity)
	return id, ok
}

// verifyToken 是「token 即身份」的落点。
//
// 关于 Expiration：**故意不填**，并在中间件上开 AllowMissingExpiration。
// token 的过期判断只能有一份，它在 governance.Token.Validate 里，用的是
// 可注入的 platform.Clock。若把 ExpiresAt 也交给 SDK，就有了两个判断者、
// 两个时钟源——测试里注入假时钟时它们立刻打架，生产上时钟漂移时同理。
// 少一处判断，不是偷懒，是不给自己留一个必然不一致的地方。
func (h *Host) verifyToken(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	id, err := h.gov.Authenticate(ctx, token)
	if err != nil {
		// 真实原因只进服务端日志。
		h.logger.Warn("认证失败", "reason", err)
		// 返回 auth.ErrInvalidToken **本身**，一个字都不加。
		//
		// SDK 会把这里返回的错误原文写进 401 响应体
		// （auth.verify: `return nil, err.Error(), http.StatusUnauthorized`）。
		// 因此哪怕只是包一层 %w 带上原因，都会把「已吊销」「已过期」
		// 直接告诉调用方——而区分这三者等于告诉攻击者哪些 token 曾经有效
		// （docs/03-mcp-contract.md §3.3）。
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{
		UserID: string(id.UserID),
		Extra:  map[string]any{extraIdentityKey: id},
	}, nil
}

// injectIdentity 把已验证的身份放进请求 context。
//
// 绝不读取客户端声明的 header——Desktop 的 X-ClaraCore-Agent-ID 是可伪造的
// （铁律 4）。这里的身份只可能来自上一层中间件验过的 token。
func (h *Host) injectIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := auth.TokenInfoFromContext(r.Context())
		if info == nil {
			// 走到这里说明中间件顺序被改错了：RequireBearerToken 必须在外层。
			h.logger.Error("认证中间件顺序错误：context 里没有 TokenInfo")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id, ok := info.Extra[extraIdentityKey].(governance.Identity)
		if !ok {
			h.logger.Error("TokenInfo 里没有身份，verifyToken 与本中间件不匹配")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

// errNoIdentity 表示 tool handler 在 context 里找不到身份。
//
// 这不是一种业务错误，是装配错误：中间件链断了。它不该被映射成
// 「无权限」之类的用户可见错误——那会让一次部署事故看起来像权限配置问题。
var errNoIdentity = errors.New("mcp: context 中没有身份，认证中间件链路断了")
