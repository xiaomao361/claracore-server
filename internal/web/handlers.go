package web

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// pageData 是每个模板都会拿到的公共部分。
type pageData struct {
	Title     string
	Principal governance.Principal
	CSRF      string
	Nav       string // 当前页，用于高亮导航
	Error     string
	Notice    string
	Version   string
}

func (h *Host) page(r *http.Request, title, nav string) pageData {
	p, _ := principalFrom(r.Context())
	return pageData{
		Title:     title,
		Principal: p,
		CSRF:      csrfToken(h.sessionTokenFrom(r)),
		Nav:       nav,
		Version:   h.opts.Version,
	}
}

// render 渲染模板。
//
// **先渲染到内存再写出去**：模板执行到一半才出错时，前半页已经写进
// ResponseWriter 了，那时既发不了 500 也回不了头，用户看到的是半张页面。
func (h *Host) render(w http.ResponseWriter, name string, data any) {
	tmpl, ok := h.tmpl[name]
	if !ok {
		h.logger.Error("模板未注册", "template", name)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	var buf []byte
	{
		b := newBuffer()
		if err := tmpl.ExecuteTemplate(b, "layout", data); err != nil {
			h.logger.Error("模板渲染失败", "template", name, "error", err)
			http.Error(w, "页面渲染失败", http.StatusInternalServerError)
			return
		}
		buf = b.Bytes()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf)
}

// ---------------------------------------------------------------------------
// 登录
// ---------------------------------------------------------------------------

func (h *Host) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// 已登录的人访问登录页直接送回首页，省掉一次「我明明登录了」的困惑
	if _, err := h.gov.ResumeSession(r.Context(), h.sessionTokenFrom(r)); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.render(w, "login.html", pageData{Title: "登录", Version: h.opts.Version})
}

func (h *Host) handleLogin(w http.ResponseWriter, r *http.Request) {
	// 登录表单不校 CSRF：此时还没有会话，也就没有可派生的 token。
	// 「登录 CSRF」（把人登进攻击者的账号）由 SameSite=Lax 挡住。
	username, password := r.FormValue("username"), r.FormValue("password")

	token, p, err := h.gov.Login(r.Context(), username, password)
	if err != nil {
		// 日志里记详细原因（可能是哈希格式坏了这类数据问题），
		// 页面上只说一句——区分失败原因等于提供账号枚举接口
		h.logger.Warn("登录失败", "username", username, "error", err)
		w.WriteHeader(http.StatusUnauthorized)
		h.render(w, "login.html", pageData{
			Title: "登录", Version: h.opts.Version,
			Error: "账号或密码不正确",
		})
		return
	}

	h.setSessionCookie(w, string(token), p.SessionExpiresAt)
	if p.MustChangePassword {
		http.Redirect(w, r, "/password", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Host) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := h.sessionTokenFrom(r)
	// 登出**要**校 CSRF：被人跨站强制登出虽然不致命，但它是一种
	// 实打实的骚扰，而这里校验的成本是一行。
	if token != "" && !checkCSRF(r, token) {
		http.Error(w, "表单已过期，请刷新页面后重试", http.StatusForbidden)
		return
	}
	if err := h.gov.Logout(r.Context(), token); err != nil {
		markWebAuditFailed(r)
		h.logger.Error("登出失败", "error", err)
	}
	h.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// 改密
// ---------------------------------------------------------------------------

func (h *Host) handlePasswordPage(w http.ResponseWriter, r *http.Request) {
	data := h.page(r, "修改密码", "password")
	if data.Principal.MustChangePassword {
		data.Notice = "这是管理员分配的初始密码，改掉之后才能使用其他功能。"
	}
	h.render(w, "password.html", data)
}

func (h *Host) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := h.page(r, "修改密码", "password")

	oldPw, newPw, confirm := r.FormValue("current"), r.FormValue("new"), r.FormValue("confirm")
	if newPw != confirm {
		markWebAuditFailed(r)
		data.Error = "两次输入的新密码不一致"
		h.render(w, "password.html", data)
		return
	}

	if err := h.gov.ChangePassword(r.Context(), p.TenantID, p.UserID, oldPw, newPw); err != nil {
		markWebAuditFailed(r)
		data.Error = userFacing(err, "当前密码不正确")
		h.render(w, "password.html", data)
		return
	}

	// 改密废掉了全部会话（包括当前这条）——那是有意的，见 ChangePassword 的注释。
	// 但把本人也踢回登录页是无谓的摩擦，所以这里用新密码重新登一次。
	token, np, err := h.gov.Login(r.Context(), p.Username, newPw)
	if err != nil {
		h.clearSessionCookie(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.setSessionCookie(w, string(token), np.SessionExpiresAt)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// 概览
// ---------------------------------------------------------------------------

type overviewSpace struct {
	Space governance.Space
	Stats knowledge.SpaceStats
	Write bool
}

type overviewTotals struct {
	Spaces         int
	WritableSpaces int
	Memories       int
	Cards          int
	Drafts         int
}

func (h *Host) handleOverview(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := struct {
		pageData
		Spaces []overviewSpace
		Totals overviewTotals
	}{pageData: h.page(r, "概览", "overview")}

	scope, err := h.gov.Scope(r.Context(), governance.Identity{TenantID: p.TenantID, UserID: p.UserID})
	if err != nil {
		h.logger.Error("解析空间权限失败", "error", err)
		data.Error = "读取空间信息失败"
		h.render(w, "overview.html", data)
		return
	}

	readable := scope.Readable()
	statsByID := map[platform.ID]knowledge.SpaceStats{}
	if len(readable) > 0 {
		stats, err := h.know.SpaceStats(r.Context(), p.TenantID, readable)
		if err != nil {
			// 统计取不到不该让整页打不开：空间列表本身仍然有用
			h.logger.Error("统计空间内容失败", "error", err)
			data.Error = "内容统计暂时不可用"
		}
		for _, st := range stats {
			statsByID[st.SpaceID] = st
		}
	}

	for _, a := range scope.All() {
		if !a.CanRead {
			continue
		}
		st := statsByID[a.Space.ID]
		data.Spaces = append(data.Spaces, overviewSpace{Space: a.Space, Stats: st, Write: a.CanWrite})
		data.Totals.Spaces++
		if a.CanWrite {
			data.Totals.WritableSpaces++
		}
		data.Totals.Memories += st.Memories
		data.Totals.Cards += st.Cards
		// 「本空间还有 N 条待审」正是 04 §6.1 点名的那个空白
		data.Totals.Drafts += st.DraftCards
	}
	h.render(w, "overview.html", data)
}

// ---------------------------------------------------------------------------
// 我的接入（token）
// ---------------------------------------------------------------------------

type tokensPageData struct {
	pageData
	Tokens []governance.Token
	// NewToken 只在刚创建后的那一次渲染里有值。明文**仅此一次展示**，
	// 库里只有哈希，刷新页面就再也拿不到了。
	NewToken   string
	MCPSnippet string
}

func (h *Host) handleTokensPage(w http.ResponseWriter, r *http.Request) {
	h.renderTokens(w, r, tokensPageData{pageData: h.page(r, "我的接入", "tokens")})
}

func (h *Host) renderTokens(w http.ResponseWriter, r *http.Request, data tokensPageData) {
	p, _ := principalFrom(r.Context())
	tokens, err := h.gov.ListTokens(r.Context(), p.TenantID, p.UserID)
	if err != nil {
		h.logger.Error("查询 token 失败", "error", err)
		data.Error = "读取 token 列表失败"
	}
	data.Tokens = tokens
	h.render(w, "tokens.html", data)
}

func (h *Host) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := tokensPageData{pageData: h.page(r, "我的接入", "tokens")}

	slug, name := r.FormValue("agent"), r.FormValue("name")
	if slug == "" || name == "" {
		markWebAuditFailed(r)
		data.Error = "agent 标识与设备名都要填"
		h.renderTokens(w, r, data)
		return
	}

	// 每个 token 绑一个 agent。这里每次都新建一个 agent 而不是复用同名的：
	// agent slug 在 (user, slug) 上有唯一键，重名会被数据库拦下并报
	// ErrAlreadyExists——那正是想要的反馈「你已经有一个叫 clara 的 agent 了」。
	agent, err := h.gov.CreateAgent(r.Context(), p.TenantID, p.UserID, slug, name)
	if errors.Is(err, governance.ErrAlreadyExists) {
		markWebAuditFailed(r)
		data.Error = fmt.Sprintf("你已经有一个标识为 %q 的 agent，换一个标识，或者复用它已有的 token", slug)
		h.renderTokens(w, r, data)
		return
	}
	if err != nil {
		markWebAuditFailed(r)
		data.Error = userFacing(err, "创建 agent 失败")
		h.renderTokens(w, r, data)
		return
	}

	plaintext, issued, err := h.gov.IssueToken(r.Context(), governance.IssueTokenInput{
		TenantID: p.TenantID, UserID: p.UserID, AgentID: agent.ID, Name: name,
	})
	if err != nil {
		markWebAuditFailed(r)
		setWebAuditTarget(r, "agent", agent.ID, "")
		data.Error = userFacing(err, "签发 token 失败")
		h.renderTokens(w, r, data)
		return
	}
	setWebAuditTarget(r, "token", issued.ID, "")

	data.NewToken = plaintext
	data.MCPSnippet = mcpSnippet(h.opts.PublicMCPURL, plaintext)
	data.Notice = "这是这个 token 明文唯一一次出现。关掉页面就再也取不回来了，现在就复制走。"
	h.renderTokens(w, r, data)
}

func (h *Host) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())

	// 只允许吊销自己的 token：拿到一个别人的 token ID 就能把同事的
	// agent 停掉，那是一个不该存在的能力。RevokeToken 只按 tenant 过滤，
	// 所以这道归属检查必须在这里做。
	tokenID := platform.ID(r.FormValue("id"))
	mine, err := h.gov.ListTokens(r.Context(), p.TenantID, p.UserID)
	if err != nil {
		markWebAuditFailed(r)
		http.Error(w, "读取 token 列表失败", http.StatusInternalServerError)
		return
	}
	owned := false
	for _, t := range mine {
		if t.ID == tokenID {
			owned = true
			break
		}
	}
	if !owned {
		markWebAuditFailed(r)
		// 不确认「这个 token 存在但不是你的」
		http.NotFound(w, r)
		return
	}

	if err := h.gov.RevokeToken(r.Context(), p.TenantID, tokenID); err != nil {
		markWebAuditFailed(r)
		h.logger.Error("吊销 token 失败", "error", err)
		data := tokensPageData{pageData: h.page(r, "我的接入", "tokens")}
		data.Error = "吊销 token 失败"
		h.renderTokens(w, r, data)
		return
	}
	http.Redirect(w, r, "/tokens", http.StatusSeeOther)
}

// mcpSnippet 生成可直接粘贴的 MCP 配置块。
//
// 不要让人自己拼 JSON——接入体验的差距全在这里（04 §3.1 页面 6）。
// 地址已填、token 已嵌入，复制走就能用。
func mcpSnippet(url, token string) string {
	return fmt.Sprintf(`{
  "mcpServers": {
    "claracore": {
      "type": "http",
      "url": %q,
      "headers": {
        "Authorization": "Bearer %s"
      }
    }
  }
}`, url, token)
}

// ---------------------------------------------------------------------------
// 成员管理（管理员）
// ---------------------------------------------------------------------------

type membersPageData struct {
	pageData
	Members []governance.User
	// NewPassword 是刚创建/重置出来的初始密码，同样只展示一次。
	NewPassword string
	ForUser     string
}

func (h *Host) handleMembersPage(w http.ResponseWriter, r *http.Request) {
	h.renderMembers(w, r, membersPageData{pageData: h.page(r, "成员管理", "members")})
}

func (h *Host) renderMembers(w http.ResponseWriter, r *http.Request, data membersPageData) {
	p, _ := principalFrom(r.Context())
	users, err := h.gov.ListUsers(r.Context(), p.TenantID)
	if err != nil {
		h.logger.Error("查询成员失败", "error", err)
		data.Error = "读取成员列表失败"
	}
	data.Members = users
	h.render(w, "members.html", data)
}

func (h *Host) handleMemberCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := membersPageData{pageData: h.page(r, "成员管理", "members")}

	username, display, role := r.FormValue("username"), r.FormValue("display"), r.FormValue("role")
	password, err := generatePassword()
	if err != nil {
		markWebAuditFailed(r)
		data.Error = "生成初始密码失败"
		h.renderMembers(w, r, data)
		return
	}

	// 初始密码由服务端生成而不是让管理员自己想一个：管理员想出来的
	// 密码有极大概率是 Team2026! 这种，而它会被同一批人共用。
	user, _, err := h.gov.CreateUser(r.Context(), governance.CreateUserInput{
		TenantID: p.TenantID, Username: username, DisplayName: display,
		Password: password, Role: role,
	})
	if errors.Is(err, governance.ErrAlreadyExists) {
		markWebAuditFailed(r)
		data.Error = fmt.Sprintf("账号 %q 已经存在", username)
		h.renderMembers(w, r, data)
		return
	}
	if err != nil {
		markWebAuditFailed(r)
		data.Error = userFacing(err, "创建账号失败")
		h.renderMembers(w, r, data)
		return
	}
	setWebAuditTarget(r, "user", user.ID, "")

	data.NewPassword = password
	data.ForUser = user.Username
	data.Notice = "把这个初始密码发给本人。它只出现这一次，且对方首次登录后必须改掉。"
	h.renderMembers(w, r, data)
}

func (h *Host) handleMemberStatus(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	target := platform.ID(r.FormValue("id"))
	status := r.FormValue("status")

	// 不许停用自己：管理员把自己关在门外之后没有恢复路径
	// （无自助注册、没有第二个入口）。这不是业务规则，是一道操作护栏。
	if target == p.UserID {
		markWebAuditFailed(r)
		data := membersPageData{pageData: h.page(r, "成员管理", "members")}
		data.Error = "不能停用自己的账号——没有别的入口能把你放回来"
		h.renderMembers(w, r, data)
		return
	}

	if err := h.gov.SetUserStatus(r.Context(), p.TenantID, target, status); err != nil {
		markWebAuditFailed(r)
		data := membersPageData{pageData: h.page(r, "成员管理", "members")}
		data.Error = userFacing(err, "更新账号状态失败")
		h.renderMembers(w, r, data)
		return
	}
	http.Redirect(w, r, "/admin/members", http.StatusSeeOther)
}

func (h *Host) handleMemberReset(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := membersPageData{pageData: h.page(r, "成员管理", "members")}
	target := platform.ID(r.FormValue("id"))

	password, err := generatePassword()
	if err != nil {
		markWebAuditFailed(r)
		data.Error = "生成初始密码失败"
		h.renderMembers(w, r, data)
		return
	}
	if err := h.gov.ResetPassword(r.Context(), p.TenantID, target, password); err != nil {
		markWebAuditFailed(r)
		data.Error = userFacing(err, "重置密码失败")
		h.renderMembers(w, r, data)
		return
	}

	data.NewPassword = password
	data.ForUser = string(target)
	data.Notice = "临时密码只出现这一次。对方登录后会被强制改掉，且他别处已登录的浏览器已全部失效。"
	h.renderMembers(w, r, data)
}

// ---------------------------------------------------------------------------
// 空间与权限（管理员）
// ---------------------------------------------------------------------------

type spaceGrantView struct {
	User       governance.User
	Permission *governance.Permission
}

type adminSpaceView struct {
	Space     governance.Space
	OwnerName string
	Grants    []spaceGrantView
}

type spacesPageData struct {
	pageData
	Spaces []adminSpaceView
}

func (h *Host) handleSpacesPage(w http.ResponseWriter, r *http.Request) {
	h.renderSpaces(w, r, spacesPageData{pageData: h.page(r, "空间与权限", "spaces")})
}

func (h *Host) renderSpaces(w http.ResponseWriter, r *http.Request, data spacesPageData) {
	p, _ := principalFrom(r.Context())
	spaces, err := h.gov.ListSpaces(r.Context(), p.TenantID)
	if err != nil {
		h.logger.Error("查询空间失败", "error", err)
		data.Error = "读取空间列表失败"
		h.render(w, "spaces.html", data)
		return
	}
	users, err := h.gov.ListUsers(r.Context(), p.TenantID)
	if err != nil {
		h.logger.Error("查询空间成员失败", "error", err)
		data.Error = "读取成员列表失败"
		h.render(w, "spaces.html", data)
		return
	}
	permissions, err := h.gov.ListSpacePermissions(r.Context(), p.TenantID)
	if err != nil {
		h.logger.Error("查询空间授权失败", "error", err)
		data.Error = "读取权限列表失败"
		h.render(w, "spaces.html", data)
		return
	}

	userByID := make(map[platform.ID]governance.User, len(users))
	for _, user := range users {
		userByID[user.ID] = user
	}
	permByPair := make(map[[2]platform.ID]governance.Permission, len(permissions))
	for _, permission := range permissions {
		permByPair[[2]platform.ID{permission.SpaceID, permission.UserID}] = permission
	}
	for _, space := range spaces {
		// 个人空间的封闭性不可配置，不出现在管理员授权面。
		if space.Type == governance.SpacePersonal {
			continue
		}
		view := adminSpaceView{Space: space}
		if space.OwnerUserID != nil {
			view.OwnerName = userByID[*space.OwnerUserID].DisplayName
		}
		for _, user := range users {
			if space.IsOwnedBy(user.ID) {
				continue
			}
			grant := spaceGrantView{User: user}
			if permission, ok := permByPair[[2]platform.ID{space.ID, user.ID}]; ok {
				cp := permission
				grant.Permission = &cp
			}
			view.Grants = append(view.Grants, grant)
		}
		data.Spaces = append(data.Spaces, view)
	}
	h.render(w, "spaces.html", data)
}

func (h *Host) handleSpaceCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	space, err := h.gov.CreateProjectSpace(r.Context(), p.TenantID, p.UserID, r.FormValue("name"))
	if err != nil {
		markWebAuditFailed(r)
		data := spacesPageData{pageData: h.page(r, "空间与权限", "spaces")}
		data.Error = userFacing(err, "创建项目空间失败")
		h.renderSpaces(w, r, data)
		return
	}
	setWebAuditTarget(r, "space", space.ID, space.ID)
	http.Redirect(w, r, "/admin/spaces", http.StatusSeeOther)
}

func (h *Host) handleSpaceGrant(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	spaceID, userID := platform.ID(r.FormValue("space")), platform.ID(r.FormValue("user"))
	var canRead, canWrite bool
	switch r.FormValue("access") {
	case "read":
		canRead = true
	case "write":
		canRead, canWrite = true, true
	default:
		markWebAuditFailed(r)
		data := spacesPageData{pageData: h.page(r, "空间与权限", "spaces")}
		data.Error = "权限只能选择只读或读写"
		h.renderSpaces(w, r, data)
		return
	}
	if err := h.gov.GrantSpaceAccess(r.Context(), p.TenantID, spaceID, userID, p.UserID, canRead, canWrite); err != nil {
		markWebAuditFailed(r)
		data := spacesPageData{pageData: h.page(r, "空间与权限", "spaces")}
		data.Error = userFacing(err, "保存空间权限失败")
		h.renderSpaces(w, r, data)
		return
	}
	setWebAuditTarget(r, "space", spaceID, spaceID)
	http.Redirect(w, r, "/admin/spaces", http.StatusSeeOther)
}

func (h *Host) handleSpaceRevoke(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	spaceID, userID := platform.ID(r.FormValue("space")), platform.ID(r.FormValue("user"))
	if err := h.gov.RevokeSpaceAccess(r.Context(), p.TenantID, spaceID, userID); err != nil {
		markWebAuditFailed(r)
		data := spacesPageData{pageData: h.page(r, "空间与权限", "spaces")}
		data.Error = userFacing(err, "撤销空间权限失败")
		h.renderSpaces(w, r, data)
		return
	}
	setWebAuditTarget(r, "space", spaceID, spaceID)
	http.Redirect(w, r, "/admin/spaces", http.StatusSeeOther)
}

// userFacing 决定错误信息能不能直接给人看。
//
// ErrInvalidArgument 描述的是调用方自己填的东西（密码太短、角色非法），
// 说清楚才能改对。其余一律换成一句笼统的话并记日志——
// 与 MCP Host 的 toolError 是同一条纪律：SQL 错误里有表名列名，
// 超时错误里有内网地址，那些不该出现在页面上。
func userFacing(err error, fallback string) string {
	if errors.Is(err, governance.ErrInvalidArgument) {
		return err.Error()
	}
	if errors.Is(err, governance.ErrInvalidCredentials) {
		return fallback
	}
	return fallback
}

// tokenTTLNote 说明这一版签发的 token 不带过期时间。
//
// governance.IssueTokenInput.TTL 留空即永不过期，这是有意的：
// 「一个 agent 多 token 并存」是平滑轮换的前提，而长期驻留在机器上的
// token 强制过期只会制造「某天早上所有 agent 一起失联」。
// 想让它失效就吊销——那是一个明确的动作。
var _ = time.Hour
