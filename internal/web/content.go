package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// 本文件是治理面的两页：卡片审核与内容浏览（docs/04-web-scope.md §3.1）。
//
// 审核页是**整个 Web 上唯一的内容写入场景**（01 §10 的唯一例外）。
// 它与「让人在库里随手写」的区别是三条硬条件：只针对 draft 状态、
// 发生在明确的摄取流程内、有原文可对照。少任何一条，这个口子
// 就会变成「Web 上也能创建内容」，而那意味着两套写入路径。

// ---------------------------------------------------------------------------
// 待审队列
// ---------------------------------------------------------------------------

type reviewListData struct {
	pageData
	Items      []knowledge.ContentItem
	Spaces     []governance.SpaceAccess
	SpaceID    string
	Total      int
	NextCursor string
	PrevExists bool
}

func (h *Host) handleReviewList(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := reviewListData{pageData: h.page(r, "卡片审核", "review")}
	if n, err := strconv.Atoi(r.URL.Query().Get("batch")); err == nil && n > 0 && n <= knowledge.MaxBatchReviewCards {
		data.Notice = fmt.Sprintf("已将 %d 张所选草稿标记为不需要；历史记录仍然保留。", n)
	}

	// 审核队列用**可写**空间集合：能写就能审（04 §6 拍板，不引入
	// 新的权限概念）。用可读集合的话，人会看到一堆自己按不动的按钮。
	scope, err := h.gov.Scope(r.Context(), governance.Identity{TenantID: p.TenantID, UserID: p.UserID})
	if err != nil {
		h.logger.Error("解析空间权限失败", "error", err)
		data.Error = "读取空间信息失败"
		h.render(w, "review.html", data)
		return
	}
	var writable []platform.ID
	for _, a := range scope.All() {
		if a.CanWrite {
			writable = append(writable, a.Space.ID)
			data.Spaces = append(data.Spaces, a)
		}
	}
	if len(writable) == 0 {
		data.Notice = "你在任何空间都没有写权限，所以没有可审的内容。"
		h.render(w, "review.html", data)
		return
	}

	data.SpaceID = r.URL.Query().Get("space")
	res, err := h.know.ListContents(r.Context(), knowledge.ListContentsInput{
		TenantID: p.TenantID,
		Spaces:   writable,
		Kind:     knowledge.EntityCard,
		Statuses: []string{knowledge.CardDraft},
		SpaceID:  platform.ID(data.SpaceID),
		Cursor:   r.URL.Query().Get("cursor"),
	})
	if err != nil {
		data.Error = userFacing(err, "读取待审队列失败")
		h.render(w, "review.html", data)
		return
	}
	data.Items, data.Total, data.NextCursor = res.Items, res.Total, res.NextCursor
	data.PrevExists = r.URL.Query().Get("cursor") != ""
	h.render(w, "review.html", data)
}

// handleReviewBatchDeprecate only removes explicitly selected drafts from the
// queue. It deliberately has no batch-publish sibling: understanding and
// recall triggers still require one-card-at-a-time judgment.
func (h *Host) handleReviewBatchDeprecate(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		markWebAuditFailed(r)
		http.Error(w, "批量审核表单无效", http.StatusBadRequest)
		return
	}
	selected, err := parseDraftCardSelections(r.Form["selection"])
	if err != nil {
		markWebAuditFailed(r)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	scope, err := h.gov.Scope(r.Context(), governance.Identity{TenantID: p.TenantID, UserID: p.UserID})
	if err != nil {
		markWebAuditFailed(r)
		http.Error(w, "读取空间权限失败", http.StatusInternalServerError)
		return
	}
	updated, err := h.know.DeprecateDraftCards(r.Context(), knowledge.DeprecateDraftCardsInput{
		TenantID: p.TenantID, WritableSpaces: writableSpaceIDs(scope), Cards: selected,
	})
	if err != nil {
		markWebAuditFailed(r)
		switch {
		case errors.Is(err, knowledge.ErrVersionConflict), errors.Is(err, knowledge.ErrInvalidArgument):
			http.Error(w, "所选草稿已经变化或不再待审；整批均未处理，请刷新后重新选择", http.StatusConflict)
		case errors.Is(err, knowledge.ErrNotFound):
			http.NotFound(w, r)
		default:
			h.logger.Error("批量标记草稿失败", "error", err)
			http.Error(w, "批量标记失败", http.StatusInternalServerError)
		}
		return
	}
	q := url.Values{"batch": {strconv.Itoa(len(updated))}}
	if spaceID := strings.TrimSpace(r.FormValue("space")); spaceID != "" {
		q.Set("space", spaceID)
	}
	http.Redirect(w, r, "/review?"+q.Encode(), http.StatusSeeOther)
}

func parseDraftCardSelections(raw []string) ([]knowledge.DraftCardVersion, error) {
	if len(raw) == 0 || len(raw) > knowledge.MaxBatchReviewCards {
		return nil, fmt.Errorf("每次请选择 1 到 %d 张草稿", knowledge.MaxBatchReviewCards)
	}
	out := make([]knowledge.DraftCardVersion, 0, len(raw))
	for _, value := range raw {
		cut := strings.LastIndexByte(value, ':')
		if cut <= 0 || cut == len(value)-1 {
			return nil, errors.New("所选草稿缺少有效版本，请刷新后重试")
		}
		version, err := strconv.Atoi(value[cut+1:])
		if err != nil || version <= 0 {
			return nil, errors.New("所选草稿缺少有效版本，请刷新后重试")
		}
		out = append(out, knowledge.DraftCardVersion{ID: platform.ID(value[:cut]), Version: version})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 审核详情：左原文，右编辑
// ---------------------------------------------------------------------------

type reviewDetailData struct {
	pageData
	Card                knowledge.Card
	Space               governance.Space
	Editable            bool
	Triggers            string
	DuplicateCandidates []knowledge.CardDuplicateCandidate
}

func (h *Host) handleReviewDetail(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id := platform.ID(r.PathValue("id"))

	card, scope, err := h.loadWritableCard(r, id)
	if err != nil {
		h.notFoundOr500(w, r, err, "读取卡片失败")
		return
	}
	space, _ := scope.Space(card.SpaceID)

	data := reviewDetailData{
		pageData: h.page(r, "审核："+card.Title, "review"),
		Card:     card,
		Space:    space,
		Editable: card.Status == knowledge.CardDraft,
		// 召回条件在表单里是一行一条的文本域。比一堆 <input> 好用得多，
		// 而审核时最常做的动作正是「加两三条触发条件」。
		Triggers: triggerLines(card),
	}
	data.DuplicateCandidates, err = h.know.FindCardDuplicateCandidates(r.Context(), p.TenantID, card.ID, scope.Readable())
	if err != nil {
		h.logger.Warn("读取重复知识卡候选失败", "card_id", card.ID, "error", err)
		data.Notice = "重复候选暂时不可用；你仍可继续审核这张卡。"
	}
	if !data.Editable {
		data.Notice = strings.TrimSpace(data.Notice + " 这张卡已经不是待审草稿，Web 仅提供只读查看。需要修正已发布内容时，请让 agent 走正常更新流程。")
	}
	_ = p
	h.render(w, "review_detail.html", data)
}

func triggerLines(c knowledge.Card) string {
	lines := make([]string, 0, len(c.RecallTriggers))
	for _, t := range c.RecallTriggers {
		lines = append(lines, t.Text)
	}
	return strings.Join(lines, "\n")
}

// handleReviewSubmit 处理确认与退回。
//
// 两个动作走同一个领域用例（UpdateCard），只差一个 status——
// 「审核」在这个系统里不是一个独立的实体或流程，就是一次带写权限
// 校验的更新（04 §6：能写就能审，无独立审核实体）。
func (h *Host) handleReviewSubmit(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id := platform.ID(r.PathValue("id"))
	card, scope, err := h.loadWritableCard(r, id)
	if err != nil {
		markWebAuditFailed(r)
		h.notFoundOr500(w, r, err, "读取卡片失败")
		return
	}
	if card.Status != knowledge.CardDraft {
		markWebAuditFailed(r)
		http.Error(w, "只有待审草稿可以在 Web 中修改", http.StatusConflict)
		return
	}
	expectedVersion, err := strconv.Atoi(r.FormValue("version"))
	if err != nil || expectedVersion != card.Version {
		markWebAuditFailed(r)
		http.Error(w, "卡片版本已经变化，请刷新后重新审核", http.StatusConflict)
		return
	}

	var status string
	switch r.FormValue("action") {
	case "publish":
		status = knowledge.CardActive
	case "reject":
		// 退回落成 deprecated 而不是留在 draft（2026-08-10 拍板）：
		// draft 队列必须能被清空，否则几千条永远挂着，
		// 「还有 N 条待审」那个数字就失去意义了。
		status = knowledge.CardDeprecated
	default:
		markWebAuditFailed(r)
		http.Error(w, "未知的操作", http.StatusBadRequest)
		return
	}

	triggers := splitTriggers(r.FormValue("triggers"))
	updated, err := h.know.UpdateCard(r.Context(), knowledge.UpdateCardInput{
		TenantID:        p.TenantID,
		ID:              id,
		WritableSpaces:  writableSpaceIDs(scope),
		ExpectedVersion: &expectedVersion,
		Status:          &status,
		// 标题与类型也能改：摄取给的标题是文件名或第一个 #，
		// cardType 一律是「摄取原料」。不让改的话，审完的卡类型
		// 全是一个值，card_search 的 cardType 过滤就废了。
		Title:          formPtr(r, "title"),
		CardType:       formPtr(r, "cardType"),
		Distilled:      formPtr(r, "distilled"),
		Understanding:  formPtr(r, "understanding"),
		WhyKeep:        formPtr(r, "whyKeep"),
		Unresolved:     formPtr(r, "unresolved"),
		RecallTriggers: &triggers,
	})
	if err != nil {
		markWebAuditFailed(r)
		// 校验失败（比如确认时「理解」栏还空着）要回到详情页并说清楚，
		// 不能把人填的东西丢掉——那会让人不敢再点第二次
		h.renderReviewError(w, r, id, userFacing(err, "保存失败"))
		return
	}
	setWebAuditTarget(r, knowledge.EntityCard, updated.ID, updated.SpaceID)
	http.Redirect(w, r, "/review", http.StatusSeeOther)
}

func (h *Host) renderReviewError(w http.ResponseWriter, r *http.Request, id platform.ID, msg string) {
	card, scope, err := h.loadWritableCard(r, id)
	if err != nil {
		h.notFoundOr500(w, r, err, "读取卡片失败")
		return
	}
	space, _ := scope.Space(card.SpaceID)
	data := reviewDetailData{
		pageData: h.page(r, "审核："+card.Title, "review"),
		Card:     card, Space: space, Editable: card.Status == knowledge.CardDraft, Triggers: r.FormValue("triggers"),
	}
	// 把人刚填的内容放回表单，别让他重打一遍
	data.Card.Title = firstNonEmpty(r.FormValue("title"), card.Title)
	data.Card.CardType = firstNonEmpty(r.FormValue("cardType"), card.CardType)
	data.Card.Distilled = r.FormValue("distilled")
	data.Card.Understanding = r.FormValue("understanding")
	data.Card.WhyKeep = r.FormValue("whyKeep")
	data.Card.Unresolved = r.FormValue("unresolved")
	data.Error = msg
	h.render(w, "review_detail.html", data)
}

// ---------------------------------------------------------------------------
// 内容浏览（只读）
// ---------------------------------------------------------------------------

type browseData struct {
	pageData
	Items      []knowledge.ContentItem
	Spaces     []governance.SpaceAccess
	Kind       string
	SpaceID    string
	Status     string
	Query      string
	Total      int
	NextCursor string
	NextURL    string
	ListURL    string
	SpaceNames map[platform.ID]string
}

func (h *Host) handleBrowse(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := browseData{pageData: h.page(r, "内容浏览", "browse"), ListURL: r.URL.RequestURI(), SpaceNames: make(map[platform.ID]string)}

	scope, err := h.gov.Scope(r.Context(), governance.Identity{TenantID: p.TenantID, UserID: p.UserID})
	if err != nil {
		h.logger.Error("解析空间权限失败", "error", err)
		data.Error = "读取空间信息失败"
		h.render(w, "browse.html", data)
		return
	}
	// 浏览用**可读**集合：看得见但改不了是完全正常的状态
	readable := scope.Readable()
	for _, a := range scope.All() {
		if a.CanRead {
			data.Spaces = append(data.Spaces, a)
			data.SpaceNames[a.Space.ID] = a.Space.Name
		}
	}
	if len(readable) == 0 {
		data.Notice = "你还没有任何可访问的空间。"
		h.render(w, "browse.html", data)
		return
	}

	q := r.URL.Query()
	data.Kind = q.Get("kind")
	if data.Kind != knowledge.EntityMemory {
		// 默认看卡片：它才是这个产品的核心载体，
		// 而记忆量大、对别人基本无意义（04 §6 对动态页的同一判断）
		data.Kind = knowledge.EntityCard
	}
	data.SpaceID, data.Status, data.Query = q.Get("space"), q.Get("status"), strings.TrimSpace(q.Get("q"))

	var statuses []string
	if data.Status != "" {
		statuses = []string{data.Status}
	}

	res, err := h.know.ListContents(r.Context(), knowledge.ListContentsInput{
		TenantID: p.TenantID,
		Spaces:   readable,
		Kind:     data.Kind,
		Statuses: statuses,
		SpaceID:  platform.ID(data.SpaceID),
		Query:    data.Query,
		Cursor:   q.Get("cursor"),
	})
	if err != nil {
		data.Error = userFacing(err, "读取内容列表失败")
		h.render(w, "browse.html", data)
		return
	}
	data.Items, data.Total, data.NextCursor = res.Items, res.Total, res.NextCursor
	if data.NextCursor != "" {
		params := url.Values{"kind": {data.Kind}, "cursor": {data.NextCursor}}
		if data.SpaceID != "" {
			params.Set("space", data.SpaceID)
		}
		if data.Status != "" {
			params.Set("status", data.Status)
		}
		if data.Query != "" {
			params.Set("q", data.Query)
		}
		data.NextURL = "/browse?" + params.Encode()
	}
	h.render(w, "browse.html", data)
}

type browseDetailData struct {
	pageData
	Kind   string
	Memory knowledge.Memory
	Card   knowledge.Card
	Space  governance.Space
	Links  []knowledge.Link
	// Writable 决定要不要显示「去审核」的入口。
	Writable            bool
	DuplicateCandidates []knowledge.CardDuplicateCandidate
	BackURL             string
}

func (h *Host) handleBrowseDetail(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id := platform.ID(r.PathValue("id"))
	kind := r.URL.Query().Get("kind")

	scope, err := h.gov.Scope(r.Context(), governance.Identity{TenantID: p.TenantID, UserID: p.UserID})
	if err != nil {
		h.notFoundOr500(w, r, err, "读取空间信息失败")
		return
	}
	readable := scope.Readable()

	data := browseDetailData{pageData: h.page(r, "详情", "browse"), Kind: kind, BackURL: safeBrowseBackURL(r.URL.Query().Get("back"))}
	switch kind {
	case knowledge.EntityMemory:
		m, links, err := h.know.GetMemory(r.Context(), p.TenantID, id, readable)
		if err != nil {
			h.notFoundOr500(w, r, err, "读取内容失败")
			return
		}
		data.Memory, data.Links = m, links
		data.Space, _ = scope.Space(m.SpaceID)
		data.Title = m.Title
	default:
		data.Kind = knowledge.EntityCard
		c, links, err := h.know.GetCard(r.Context(), p.TenantID, id, readable)
		if err != nil {
			h.notFoundOr500(w, r, err, "读取内容失败")
			return
		}
		data.Card, data.Links = c, links
		data.Space, _ = scope.Space(c.SpaceID)
		data.Title = c.Title
		data.Writable = scope.CanWrite(c.SpaceID)
		data.DuplicateCandidates, err = h.know.FindCardDuplicateCandidates(r.Context(), p.TenantID, c.ID, readable)
		if err != nil {
			h.logger.Warn("读取重复知识卡候选失败", "card_id", c.ID, "error", err)
			data.Notice = "重复候选暂时不可用；卡片正文仍可正常查看。"
		}
	}
	h.render(w, "browse_detail.html", data)
}

func safeBrowseBackURL(raw string) string {
	if raw == "/browse" || strings.HasPrefix(raw, "/browse?") {
		return raw
	}
	return "/browse"
}

// ---------------------------------------------------------------------------
// 共用
// ---------------------------------------------------------------------------

// loadWritableCard 取一张**自己能改**的卡。
//
// 用可写集合定位而不是「可读定位 + 再判可写」：对一条自己改不了的卡，
// 「不存在」与「无权修改」给调用方的下一步是同一件事
// （与领域层 UpdateCardInput.WritableSpaces 同一条理由）。
func (h *Host) loadWritableCard(r *http.Request, id platform.ID) (knowledge.Card, governance.SpaceScope, error) {
	p, _ := principalFrom(r.Context())
	scope, err := h.gov.Scope(r.Context(), governance.Identity{TenantID: p.TenantID, UserID: p.UserID})
	if err != nil {
		return knowledge.Card{}, governance.SpaceScope{}, err
	}
	var writable []platform.ID
	for _, a := range scope.All() {
		if a.CanWrite {
			writable = append(writable, a.Space.ID)
		}
	}
	if len(writable) == 0 {
		return knowledge.Card{}, scope, knowledge.ErrNotFound
	}
	card, _, err := h.know.GetCard(r.Context(), p.TenantID, id, writable)
	return card, scope, err
}

func writableSpaceIDs(scope governance.SpaceScope) []platform.ID {
	var out []platform.ID
	for _, a := range scope.All() {
		if a.CanWrite {
			out = append(out, a.Space.ID)
		}
	}
	return out
}

// notFoundOr500 把「找不到或无权访问」渲染成 404。
//
// 不区分二者：区分等于让任何人都能用一个 ID 探测出
// 「这条内容存在于某个我看不见的空间」。与领域层合并 ErrNotFound
// 与无权访问是同一条哲学。
func (h *Host) notFoundOr500(w http.ResponseWriter, r *http.Request, err error, msg string) {
	if errors.Is(err, knowledge.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	h.logger.Error(msg, "error", err, "path", r.URL.Path)
	http.Error(w, msg, http.StatusInternalServerError)
}

// splitTriggers 把文本域按行拆成召回条件。
// 清洗（去空行、去重、长度校验）交给领域层的 normalizeTriggers，
// 这里只做最朴素的拆分——多清洗一遍就是把规则复制到了第二个地方。
func splitTriggers(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	if out == nil {
		// 非 nil 的空切片：领域层用 nil 表示「不改」，
		// 空切片表示「清空」。审核页提交的永远是「这就是最终状态」。
		out = []string{}
	}
	return out
}

// formPtr 让「表单里有这个字段」与「没有」可区分。
//
// 领域层用 *string 表达 nil=不改 / ""=清空，而 HTML 表单
// 总会把每个字段都提交上来（哪怕是空的）——所以审核页提交的
// 就是全量状态，每个字段都取指针。
func formPtr(r *http.Request, name string) *string {
	v := r.FormValue(name)
	return &v
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
