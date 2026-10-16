package memo

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"claracore-server/internal/embedding"
	"claracore-server/internal/platform"
)

const (
	KindMemory    = "memory"
	KindKnowledge = "knowledge"

	maxTitle      = 200
	maxBody       = 50000
	maxTags       = 8
	maxTag        = 32
	maxSource     = 200
	maxReason     = 500
	maxOwner      = 128
	embedTextCap  = 4000
	saveEmbedWait = 10 * time.Second
)

// Code 与 shipwright 契约 v1 的错误码一一对应，MCP 层原样输出。
type Code string

const (
	CodeInvalid      Code = "INVALID_ARGUMENT"
	CodeNotFound     Code = "NOT_FOUND"
	CodeDenied       Code = "PERMISSION_DENIED"
	CodePrecondition Code = "FAILED_PRECONDITION"
)

// Error 是预期内的失败：调用方能据此改正参数或换个做法。
type Error struct {
	Code Code
	Msg  string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Msg }

func fail(code Code, format string, args ...any) error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Entry 是一条记忆或知识。Owner 是 bosun 令牌上的身份标识（X-Bosun-User）。
type Entry struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Owner           string   `json:"owner"`
	Shared          bool     `json:"shared"`
	Title           string   `json:"title"`
	Body            string   `json:"body"`
	Tags            []string `json:"tags"`
	Source          string   `json:"source,omitempty"`
	Status          string   `json:"status"`
	SupersededBy    string   `json:"superseded_by,omitempty"`
	SupersedeReason string   `json:"supersede_reason,omitempty"`
	CreatedAt       string   `json:"created_at"`
}

type Service struct {
	db       *sql.DB
	embedder embedding.Embedder // nil = 未配置向量检索，只走文本匹配
	clock    platform.Clock
	ids      *platform.IDGenerator
	log      *slog.Logger
}

func NewService(db *sql.DB, embedder embedding.Embedder, clock platform.Clock, log *slog.Logger) *Service {
	return &Service{db: db, embedder: embedder, clock: clock, ids: platform.NewIDGenerator(clock), log: log}
}

// SemanticConfigured 只用于 /health 自述。
func (s *Service) SemanticConfigured() bool { return s.embedder != nil }

type SaveInput struct {
	Kind   string
	Title  string
	Body   string
	Tags   []string
	Source string
	Shared bool
}

// SaveResult.Indexed=false 表示向量没算成（未配置或服务不可用）；后台会补，文本检索不受影响。
type SaveResult struct {
	Entry      Entry  `json:"entry"`
	Indexed    bool   `json:"indexed"`
	IndexError string `json:"index_error,omitempty"`
}

func (s *Service) Save(ctx context.Context, owner string, in SaveInput) (SaveResult, error) {
	if err := checkOwner(owner); err != nil {
		return SaveResult{}, err
	}
	e, err := s.newEntry(owner, in)
	if err != nil {
		return SaveResult{}, err
	}
	if err := insert(ctx, s.db, e); err != nil {
		return SaveResult{}, err
	}
	res := SaveResult{Entry: e}
	res.Indexed, res.IndexError = s.embedOne(ctx, e)
	return res, nil
}

func (s *Service) newEntry(owner string, in SaveInput) (Entry, error) {
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		kind = KindMemory
	}
	if kind != KindMemory && kind != KindKnowledge {
		return Entry{}, fail(CodeInvalid, "kind 只能是 memory 或 knowledge，收到 %q", in.Kind)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" || utf8.RuneCountInString(title) > maxTitle {
		return Entry{}, fail(CodeInvalid, "title 必填，最多 %d 字", maxTitle)
	}
	body := strings.TrimSpace(in.Body)
	if body == "" || utf8.RuneCountInString(body) > maxBody {
		return Entry{}, fail(CodeInvalid, "body 必填，最多 %d 字；更长的内容请拆成几条", maxBody)
	}
	tags, err := normalizeTags(in.Tags)
	if err != nil {
		return Entry{}, err
	}
	source := strings.TrimSpace(in.Source)
	if utf8.RuneCountInString(source) > maxSource || strings.ContainsAny(source, "\r\n") {
		return Entry{}, fail(CodeInvalid, "source 是单行文本，最多 %d 字", maxSource)
	}
	return Entry{
		ID: string(s.ids.New()), Kind: kind, Owner: owner, Shared: in.Shared,
		Title: title, Body: body, Tags: tags, Source: source, Status: "active",
		CreatedAt: s.clock.Now().Format(time.RFC3339),
	}, nil
}

func checkOwner(owner string) error {
	if owner == "" || utf8.RuneCountInString(owner) > maxOwner {
		return fail(CodeDenied, "无法识别调用者：请经由 bosun 网关调用（需要 X-Bosun-User）")
	}
	return nil
}

func normalizeTags(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, t := range raw {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		if utf8.RuneCountInString(t) > maxTag || strings.IndexFunc(t, unicode.IsSpace) >= 0 {
			return nil, fail(CodeInvalid, "标签 %q 不合法：不含空白，最多 %d 字", t, maxTag)
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > maxTags {
		return nil, fail(CodeInvalid, "标签最多 %d 个", maxTags)
	}
	return out, nil
}

// tags 列存成 " a b "，前后带空格，按标签过滤时用 LIKE '% a %' 精确命中整个标签。
func encodeTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return " " + strings.Join(tags, " ") + " "
}

func decodeTags(s string) []string {
	f := strings.Fields(s)
	if f == nil {
		return []string{}
	}
	return f
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insert(ctx context.Context, db execer, e Entry) error {
	_, err := db.ExecContext(ctx, `INSERT INTO entries (id, kind, owner, shared, title, body, tags, source, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', ?)`,
		e.ID, e.Kind, e.Owner, boolInt(e.Shared), e.Title, e.Body, encodeTags(e.Tags), e.Source, e.CreatedAt)
	return err
}

const entryCols = `id, kind, owner, shared, title, body, tags, source, status, coalesce(superseded_by, ''), supersede_reason, created_at`

func scanEntry(row interface{ Scan(...any) error }, extra ...any) (Entry, error) {
	var e Entry
	var shared int
	var tags string
	dest := append([]any{&e.ID, &e.Kind, &e.Owner, &shared, &e.Title, &e.Body, &tags, &e.Source,
		&e.Status, &e.SupersededBy, &e.SupersedeReason, &e.CreatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return Entry{}, err
	}
	e.Shared = shared == 1
	e.Tags = decodeTags(tags)
	return e, nil
}

// Get 取一条（含已被取代的）。别人的私有条目与不存在的条目同样返回 NOT_FOUND，不泄露存在性。
func (s *Service) Get(ctx context.Context, caller, id string) (Entry, error) {
	if err := checkOwner(caller); err != nil {
		return Entry{}, err
	}
	e, err := scanEntry(s.db.QueryRowContext(ctx, `SELECT `+entryCols+` FROM entries WHERE id = ? AND (owner = ? OR shared = 1)`,
		strings.TrimSpace(id), caller))
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, fail(CodeNotFound, "没有 id=%s 的条目，或它不属于你也未共享；可用 memory_search 查找", id)
	}
	return e, err
}

type SupersedeInput struct {
	ID     string
	Reason string
	// 新内容；Title 与 Body 都为空表示只作废、不给替代。
	Title  string
	Body   string
	Tags   []string // nil = 沿用旧标签
	Source *string  // nil = 沿用旧来源
	Shared *bool    // nil = 沿用旧的共享设置
}

type SupersedeResult struct {
	Old        Entry  `json:"old"`
	New        *Entry `json:"new,omitempty"`
	Indexed    bool   `json:"indexed,omitempty"`
	IndexError string `json:"index_error,omitempty"`
}

// Supersede 用新内容取代一条记忆（旧的保留，标记 superseded 并指向新的），或只把它作废。
// 只有作者本人能取代；共享条目别人看得见但改不了。
func (s *Service) Supersede(ctx context.Context, caller string, in SupersedeInput) (SupersedeResult, error) {
	old, err := s.Get(ctx, caller, in.ID)
	if err != nil {
		return SupersedeResult{}, err
	}
	if old.Owner != caller {
		return SupersedeResult{}, fail(CodeDenied, "这条由 %s 写入，只有作者能取代；可以另存一条并在 body 里说明更正", old.Owner)
	}
	if old.Status != "active" {
		return SupersedeResult{}, fail(CodePrecondition, "这条已被取代（superseded_by=%s），请对最新的那条操作", old.SupersededBy)
	}
	reason := strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(reason) > maxReason {
		return SupersedeResult{}, fail(CodeInvalid, "reason 最多 %d 字", maxReason)
	}

	retire := strings.TrimSpace(in.Title) == "" && strings.TrimSpace(in.Body) == ""
	var next *Entry
	if !retire {
		si := SaveInput{Kind: old.Kind, Title: in.Title, Body: in.Body, Tags: old.Tags, Source: old.Source, Shared: old.Shared}
		if si.Title == "" {
			si.Title = old.Title
		}
		if strings.TrimSpace(si.Body) == "" {
			si.Body = old.Body
		}
		if in.Tags != nil {
			si.Tags = in.Tags
		}
		if in.Source != nil {
			si.Source = *in.Source
		}
		if in.Shared != nil {
			si.Shared = *in.Shared
		}
		e, err := s.newEntry(caller, si)
		if err != nil {
			return SupersedeResult{}, err
		}
		next = &e
	} else if reason == "" {
		return SupersedeResult{}, fail(CodeInvalid, "只作废不给新内容时，reason 必填")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SupersedeResult{}, err
	}
	defer tx.Rollback()
	var newID any
	if next != nil {
		if err := insert(ctx, tx, *next); err != nil {
			return SupersedeResult{}, err
		}
		newID = next.ID
	}
	// status='active' 条件挡住并发的两次取代：后到的那次影响 0 行。
	res, err := tx.ExecContext(ctx, `UPDATE entries SET status = 'superseded', superseded_by = ?, supersede_reason = ?
		WHERE id = ? AND status = 'active'`, newID, reason, old.ID)
	if err != nil {
		return SupersedeResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return SupersedeResult{}, fail(CodePrecondition, "这条刚被别的调用取代了，请先 memory_get 看最新状态")
	}
	if err := tx.Commit(); err != nil {
		return SupersedeResult{}, err
	}

	old.Status, old.SupersedeReason = "superseded", reason
	if next != nil {
		old.SupersededBy = next.ID
	}
	out := SupersedeResult{Old: old, New: next}
	if next != nil {
		out.Indexed, out.IndexError = s.embedOne(ctx, *next)
	}
	return out, nil
}

// ---- 向量 ----

func embedText(title string, tags []string, body string) string {
	t := title + "\n" + strings.Join(tags, " ") + "\n" + body
	if utf8.RuneCountInString(t) <= embedTextCap {
		return t
	}
	return string([]rune(t)[:embedTextCap])
}

// embedOne 在写入后顺手算向量。失败不影响写入本身：记下原因，交给 IndexPending 重试。
func (s *Service) embedOne(ctx context.Context, e Entry) (bool, string) {
	if s.embedder == nil {
		return false, "未配置向量检索"
	}
	ctx, cancel := context.WithTimeout(ctx, saveEmbedWait)
	defer cancel()
	vecs, err := s.embedder.Embed(ctx, []string{embedText(e.Title, e.Tags, e.Body)})
	if err != nil {
		s.recordEmbedError(e.ID, err)
		return false, err.Error()
	}
	if err := s.storeVector(context.WithoutCancel(ctx), e.ID, vecs[0]); err != nil {
		s.log.Error("保存向量失败", "id", e.ID, "err", err)
		return false, "向量已算出但未能保存"
	}
	return true, ""
}

func (s *Service) storeVector(ctx context.Context, id string, v []float32) error {
	_, err := s.db.ExecContext(ctx, `UPDATE entries SET embed_model = ?, embedding = ?, embed_error = NULL WHERE id = ?`,
		s.embedder.Model(), encodeVector(v), id)
	return err
}

func (s *Service) recordEmbedError(id string, cause error) {
	if _, err := s.db.Exec(`UPDATE entries SET embed_error = ? WHERE id = ?`, cause.Error(), id); err != nil {
		s.log.Error("记录向量错误失败", "id", id, "err", err)
	}
}

// IndexPending 给缺向量（或向量来自旧模型）的有效条目补算一批。返回处理条数。
func (s *Service) IndexPending(ctx context.Context, batch int) (int, error) {
	if s.embedder == nil {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, title, tags, body FROM entries
		WHERE status = 'active' AND (embed_model IS NULL OR embed_model != ?)
		ORDER BY created_at DESC LIMIT ?`, s.embedder.Model(), batch)
	if err != nil {
		return 0, err
	}
	var ids, texts []string
	for rows.Next() {
		var id, title, tags, body string
		if err := rows.Scan(&id, &title, &tags, &body); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		texts = append(texts, embedText(title, decodeTags(tags), body))
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return 0, err
	}
	vecs, err := s.embedder.Embed(ctx, texts)
	if err != nil {
		for _, id := range ids {
			s.recordEmbedError(id, err)
		}
		return 0, err
	}
	for i, id := range ids {
		if err := s.storeVector(ctx, id, vecs[i]); err != nil {
			return i, err
		}
	}
	return len(ids), nil
}

// RunIndexer 周期补向量，直到 ctx 结束。
func (s *Service) RunIndexer(ctx context.Context, every time.Duration) {
	if s.embedder == nil {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n, err := s.IndexPending(ctx, 32)
		if err != nil && ctx.Err() == nil {
			s.log.Warn("补向量失败，下轮重试", "err", err)
		} else if n > 0 {
			s.log.Info("补向量", "count", n)
		}
		if n == 32 {
			continue // 还有积压，不等下一轮
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Stats 只用于 /health。
type Stats struct {
	Entries   int `json:"entries"`
	Unindexed int `json:"unindexed"`
}

func (s *Service) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	model := ""
	if s.embedder != nil {
		model = s.embedder.Model()
	}
	err := s.db.QueryRowContext(ctx, `SELECT count(*),
		coalesce(sum(CASE WHEN embed_model IS NULL OR embed_model != ? THEN 1 ELSE 0 END), 0)
		FROM entries WHERE status = 'active'`, model).Scan(&st.Entries, &st.Unindexed)
	if s.embedder == nil {
		st.Unindexed = 0
	}
	return st, err
}

func encodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decodeVector(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
