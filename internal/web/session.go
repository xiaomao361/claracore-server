package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"claracore-server/internal/domain/governance"
)

// sessionCookie 是会话 cookie 的名字。
//
// 前缀 `__Host-` 是浏览器强制的一组约束：必须 Secure、必须 Path=/、
// 不能带 Domain。它挡住的是「同一个域下另一个子域写一个同名 cookie
// 把你的会话顶掉」这类攻击。**但它要求 HTTPS**，所以本地开发
// （SecureCookie=false）时退回普通名字——两个名字不会同时存在。
const (
	sessionCookie       = "ccs_session"
	sessionCookieSecure = "__Host-ccs_session"
)

func (h *Host) cookieName() string {
	if h.opts.SecureCookie {
		return sessionCookieSecure
	}
	return sessionCookie
}

func (h *Host) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:  h.cookieName(),
		Value: token,
		Path:  "/",
		// HttpOnly：JS 读不到。即使哪天页面上出现一个 XSS，
		// 会话令牌也不会被脚本偷走。
		HttpOnly: true,
		// SameSite=Lax：跨站发起的 POST 不带 cookie，这是 CSRF 的第一道门。
		// 第二道是下面的 csrfToken——两道都要，因为 SameSite 依赖浏览器行为，
		// 而 CSRF token 依赖服务端校验。
		SameSite: http.SameSiteLaxMode,
		Secure:   h.opts.SecureCookie,
		Expires:  expires,
	})
}

func (h *Host) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.cookieName(),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.opts.SecureCookie,
		MaxAge:   -1,
	})
}

func (h *Host) sessionTokenFrom(r *http.Request) string {
	c, err := r.Cookie(h.cookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

// principalKey 是当前登录者在 context 中的键。
//
// 与 MCP Host 那侧同一套做法：**未导出的空结构体类型**，
// 外部包无法伪造这个键——身份只能由本包的中间件放进去。
type principalKey struct{}

func principalFrom(ctx context.Context) (governance.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(governance.Principal)
	return p, ok
}

// csrfToken 由会话令牌派生，**不额外存储**。
//
// 攻击者要伪造它就得先知道会话令牌，而 cookie 是 HttpOnly 的、
// 跨站请求又读不到。派生而非随机存储的好处是无状态：
// 不需要为每个表单在库里多写一行，也没有「token 过期了但会话还在」
// 这种要单独处理的中间态。
//
// 用 HMAC 而不是直接 sha256(token)：前者即使派生值泄露也推不回令牌。
func csrfToken(sessionToken string) string {
	mac := hmac.New(sha256.New, []byte(sessionToken))
	mac.Write([]byte("ccs-csrf-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func checkCSRF(r *http.Request, sessionToken string) bool {
	got := r.FormValue("csrf")
	want := csrfToken(sessionToken)
	// 常数时间比较：这条比较发生在每次写操作上，没有理由留时序侧信道
	return hmac.Equal([]byte(got), []byte(want))
}

// requireUser 要求一张有效会话。
//
// 会话无效时**重定向到登录页而不是回 401**：这是给人看的界面，
// 一个白屏 401 对使用者没有任何指导意义。
func (h *Host) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := h.sessionTokenFrom(r)
		p, err := h.gov.ResumeSession(r.Context(), token)
		if err != nil {
			h.clearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		write := isWrite(r)
		ctx := context.WithValue(r.Context(), principalKey{}, p)
		if write {
			// Base recording stays centralized here. Handlers may enrich this
			// request-local target after a successful domain call.
			ctx = context.WithValue(ctx, webAuditStateKey{}, &webAuditState{})
		}

		// 写操作一律校 CSRF。放在中间件里而不是每个 handler 各判一次：
		// 少判一次不会有编译错误，只会有一个可以被跨站触发的写接口。
		if write && !checkCSRF(r, token) {
			authenticated := r.WithContext(ctx)
			h.logger.Warn("CSRF 校验失败", "path", r.URL.Path, "user", p.UserID)
			markWebAuditFailed(authenticated)
			h.recordWebWrite(authenticated, p, http.StatusForbidden)
			http.Error(w, "表单已过期，请刷新页面后重试", http.StatusForbidden)
			return
		}
		// checkCSRF 会解析表单，所以要在它之后再复制 Request；
		// 否则副本看不到已解析的 Form，body 又已经被读完。
		authenticated := r.WithContext(ctx)
		if write {
			captured := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(captured, authenticated)
			// The full administrator audit query has a stronger contract: its
			// meta-audit row is written synchronously before the query runs. Do
			// not add a second best-effort governance row here.
			if r.Pattern != "POST /admin/audit" {
				h.recordWebWrite(authenticated, p, captured.Status())
			}
			return
		}
		next.ServeHTTP(w, authenticated)
	})
}

// requirePasswordSet 在 requireUser 之上再加一条：初始密码必须已经改过。
//
// 管理员分配的初始密码在改掉之前不该能做任何别的事——那个密码
// 至少经过了一次口头或聊天工具的传递。
func (h *Host) requirePasswordSet(next http.Handler) http.Handler {
	return h.requireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFrom(r.Context())
		if p.MustChangePassword {
			markWebAuditFailed(r)
			http.Redirect(w, r, "/password", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// requireAdmin 再加一条角色判断。
//
// 角色从**每次回查的用户行**来（见 governance.ResumeSession），不是
// 从会话里读的缓存值：一个刚被降级的账号必须在下一次请求就失去管理页。
func (h *Host) requireAdmin(next http.Handler) http.Handler {
	return h.requirePasswordSet(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFrom(r.Context())
		if !p.IsAdmin() {
			// 404 而不是 403：不确认「这个管理页存在」。
			// 与领域层「不存在与无权访问返回同一个错误」是同一条哲学。
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func isWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
