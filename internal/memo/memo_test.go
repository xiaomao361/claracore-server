package memo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/platform"
)

func newTestService(t *testing.T, emb *fakeEmbedder) *Service {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := &platform.OffsetClock{Base: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), Step: time.Second}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if emb == nil {
		return NewService(db, nil, clock, log)
	}
	return NewService(db, emb, clock, log)
}

// fakeEmbedder 按关键词给出固定方向的向量，足以检验融合与状态，不检验语义质量。
type fakeEmbedder struct {
	fail  error
	model string
}

func (f *fakeEmbedder) Model() string {
	if f.model == "" {
		return "fake-v1"
	}
	return f.model
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := []float32{0.1, 0.1, 0.1}
		if strings.Contains(t, "支付") || strings.Contains(t, "付款") {
			v[0] = 1
		}
		if strings.Contains(t, "部署") {
			v[1] = 1
		}
		out[i] = v
	}
	return out, nil
}

func code(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func mustSave(t *testing.T, s *Service, owner string, in SaveInput) Entry {
	t.Helper()
	r, err := s.Save(context.Background(), owner, in)
	if err != nil {
		t.Fatal(err)
	}
	return r.Entry
}

func TestVisibility(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()
	private := mustSave(t, s, "a@x", SaveInput{Title: "私有", Body: "只给 a"})
	shared := mustSave(t, s, "a@x", SaveInput{Title: "共享", Body: "大家都能看", Shared: true})

	if _, err := s.Get(ctx, "b@x", private.ID); code(err) != CodeNotFound {
		t.Fatalf("别人的私有条目应当是 NOT_FOUND，得到 %v", err)
	}
	if _, err := s.Get(ctx, "b@x", shared.ID); err != nil {
		t.Fatalf("共享条目应当可读: %v", err)
	}
	res, err := s.Search(ctx, "b@x", SearchInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || res.Items[0].ID != shared.ID {
		t.Fatalf("b 只应看到共享那条: %+v", res.Items)
	}
	if _, err := s.Search(ctx, "", SearchInput{}); code(err) != CodeDenied {
		t.Fatalf("没有调用者身份应当 PERMISSION_DENIED，得到 %v", err)
	}
}

func TestSupersede(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()
	old := mustSave(t, s, "a@x", SaveInput{Title: "超时阈值", Body: "回调超时 3 秒", Tags: []string{"pay"}, Shared: true})

	if _, err := s.Supersede(ctx, "b@x", SupersedeInput{ID: old.ID, Body: "改了"}); code(err) != CodeDenied {
		t.Fatalf("非作者取代应当 PERMISSION_DENIED，得到 %v", err)
	}
	r, err := s.Supersede(ctx, "a@x", SupersedeInput{ID: old.ID, Body: "回调超时 5 秒", Reason: "配置已改"})
	if err != nil {
		t.Fatal(err)
	}
	if r.New == nil || r.New.Title != "超时阈值" || !r.New.Shared || len(r.New.Tags) != 1 {
		t.Fatalf("新条目应沿用标题、共享与标签: %+v", r.New)
	}
	got, _ := s.Get(ctx, "a@x", old.ID)
	if got.Status != "superseded" || got.SupersededBy != r.New.ID || got.SupersedeReason != "配置已改" {
		t.Fatalf("旧条目状态不对: %+v", got)
	}
	if _, err := s.Supersede(ctx, "a@x", SupersedeInput{ID: old.ID, Body: "再改"}); code(err) != CodePrecondition {
		t.Fatalf("对已取代条目再取代应当 FAILED_PRECONDITION，得到 %v", err)
	}
	res, _ := s.Search(ctx, "a@x", SearchInput{Query: "回调超时"})
	if res.Total != 1 || res.Items[0].ID != r.New.ID {
		t.Fatalf("检索只应返回新条目: %+v", res.Items)
	}

	if _, err := s.Supersede(ctx, "a@x", SupersedeInput{ID: r.New.ID}); code(err) != CodeInvalid {
		t.Fatalf("只作废不写 reason 应当 INVALID_ARGUMENT，得到 %v", err)
	}
	retired, err := s.Supersede(ctx, "a@x", SupersedeInput{ID: r.New.ID, Reason: "功能下线"})
	if err != nil || retired.New != nil || retired.Old.Status != "superseded" {
		t.Fatalf("只作废: %+v %v", retired, err)
	}
}

func TestTextSearchChinese(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()
	hit := mustSave(t, s, "a@x", SaveInput{Title: "支付回调超时", Body: "网关重试三次后放弃", Tags: []string{"gbjk-zhifu"}})
	mustSave(t, s, "a@x", SaveInput{Title: "部署流程", Body: "合并到 main 自动部署"})

	res, err := s.Search(ctx, "a@x", SearchInput{Query: "回调 超时"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Semantic != "off" || res.Total != 1 || res.Items[0].ID != hit.ID || res.Items[0].Match != "text" {
		t.Fatalf("中文二元组应命中: %+v", res)
	}
	res, _ = s.Search(ctx, "a@x", SearchInput{Query: "怎么部署"})
	if res.Total != 1 || res.Items[0].Title != "部署流程" {
		t.Fatalf("带虚词的自然语言查询也应命中: %+v", res.Items)
	}
	res, _ = s.Search(ctx, "a@x", SearchInput{Query: "退款"})
	if res.Total != 0 {
		t.Fatalf("无关查询不应命中: %+v", res.Items)
	}
	res, _ = s.Search(ctx, "a@x", SearchInput{Tags: []string{"GBJK-zhifu"}})
	if res.Total != 1 || res.Items[0].ID != hit.ID {
		t.Fatalf("标签过滤（大小写不敏感）: %+v", res.Items)
	}
	res, _ = s.Search(ctx, "a@x", SearchInput{Tags: []string{"gbjk"}})
	if res.Total != 0 {
		t.Fatalf("标签必须整词匹配: %+v", res.Items)
	}
}

func TestHybridAndSemanticStates(t *testing.T) {
	emb := &fakeEmbedder{}
	s := newTestService(t, emb)
	ctx := context.Background()
	r, _ := s.Save(ctx, "a@x", SaveInput{Title: "付款失败排查", Body: "先看渠道返回码"})
	if !r.Indexed {
		t.Fatalf("配置了向量服务时应当当场建索引: %+v", r)
	}
	mustSave(t, s, "a@x", SaveInput{Title: "部署流程", Body: "合并即部署"})

	// 「支付」与「付款」文本不重合，只能靠语义命中。
	res, err := s.Search(ctx, "a@x", SearchInput{Query: "支付"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Semantic != "ok" || res.Total != 1 || res.Items[0].Match != "semantic" || res.Items[0].Cosine == nil {
		t.Fatalf("应语义命中付款那条: %+v", res)
	}

	emb.fail = errors.New("embedding(ollama): 服务不可达或超时")
	res, err = s.Search(ctx, "a@x", SearchInput{Query: "部署"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Semantic != "unavailable" || res.SemanticError == "" || res.Total != 1 {
		t.Fatalf("向量服务失败时应退回文本并如实标注: %+v", res)
	}

	saved, err := s.Save(ctx, "a@x", SaveInput{Title: "向量服务挂了也能写", Body: "写入不受影响"})
	if err != nil || saved.Indexed || saved.IndexError == "" {
		t.Fatalf("写入应成功但标注未建索引: %+v %v", saved, err)
	}
	emb.fail = nil
	n, err := s.IndexPending(ctx, 32)
	if err != nil || n != 1 {
		t.Fatalf("后台应补上那一条: n=%d err=%v", n, err)
	}
	st, _ := s.Stats(ctx)
	if st.Entries != 3 || st.Unindexed != 0 {
		t.Fatalf("stats: %+v", st)
	}

	// 换模型：旧向量全部视为待重算。
	emb.model = "fake-v2"
	st, _ = s.Stats(ctx)
	if st.Unindexed != 3 {
		t.Fatalf("换模型后应全部待重算: %+v", st)
	}
}

func TestPaginationAndValidation(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		mustSave(t, s, "a@x", SaveInput{Title: "条目", Body: "内容"})
	}
	res, _ := s.Search(ctx, "a@x", SearchInput{Limit: 2})
	if len(res.Items) != 2 || res.Total != 5 || res.NextOffset == nil || *res.NextOffset != 2 {
		t.Fatalf("第一页: %+v", res)
	}
	res, _ = s.Search(ctx, "a@x", SearchInput{Limit: 2, Offset: 4})
	if len(res.Items) != 1 || res.NextOffset != nil {
		t.Fatalf("最后一页: %+v", res)
	}

	for _, in := range []SaveInput{
		{Title: "", Body: "x"},
		{Title: "x", Body: " "},
		{Title: "x", Body: "x", Kind: "card"},
		{Title: "x", Body: "x", Tags: []string{"有 空格"}},
		{Title: "x", Body: "x", Source: "a\nb"},
	} {
		if _, err := s.Save(ctx, "a@x", in); code(err) != CodeInvalid {
			t.Errorf("%+v 应当 INVALID_ARGUMENT，得到 %v", in, err)
		}
	}
	if _, err := s.Search(ctx, "a@x", SearchInput{Limit: 51}); code(err) != CodeInvalid {
		t.Errorf("limit 越界应当 INVALID_ARGUMENT")
	}
}

func TestReopenKeepsSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatalf("重复打开同一个库应当成功: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("库版本高于程序时应拒绝打开")
	}
}

func TestSemanticRelativeMargin(t *testing.T) {
	q := []float32{1, 0}
	at := func(cos float64) []float32 { return []float32{float32(cos), float32(math.Sqrt(1 - cos*cos))} }
	cands := []candidate{
		{Entry: Entry{ID: "best", Title: "x"}, vec: at(0.73)},
		{Entry: Entry{ID: "noise", Title: "y"}, vec: at(0.54)}, // 过了绝对门槛，但离最高分太远
		{Entry: Entry{ID: "close", Title: "z"}, vec: at(0.70)},
		{Entry: Entry{ID: "low", Title: "w"}, vec: at(0.45)},
	}
	var ids []string
	for _, h := range rankHybrid(cands, "无关", q) {
		ids = append(ids, h.ID)
	}
	if strings.Join(ids, ",") != "best,close" {
		t.Fatalf("应只保留最高分附近的语义命中: %v", ids)
	}
}

func TestTokenize(t *testing.T) {
	got := strings.Join(tokenize("Redis 连接池耗尽 a"), "|")
	if got != "redis|连接|接池|池耗|耗尽" {
		t.Fatalf("tokenize: %s", got)
	}
}

// 2026-10-20 团队知识实测漏召回的三类问法：标识符被虚词淹没、标识符权重不够、同一个东西换了叫法。
func TestTextSearchIdentifiersAndSynonyms(t *testing.T) {
	s := newTestService(t, nil)
	ctx := context.Background()
	services := mustSave(t, s, "a@x", SaveInput{Title: "后端服务清单", Body: "gb-pigeon 信鸽服务、gb-statement 对账服务"})
	billing := mustSave(t, s, "a@x", SaveInput{Title: "云账单口径", Body: "覆盖贯柏、企康两个租户"})
	domains := mustSave(t, s, "a@x", SaveInput{Title: "各端前端域名", Body: "管理端测试环境 gb-admin-nt；核工业221租户 hg221"})

	top := func(q string) string {
		t.Helper()
		res, err := s.Search(ctx, "a@x", SearchInput{Query: q})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Items) == 0 {
			return ""
		}
		return res.Items[0].ID
	}
	if got := top("gb-pigeon 是干什么的"); got != services.ID {
		t.Fatalf("命中标识符应不受三分之一门槛限制: %q", got)
	}
	if got := top("hg221 是哪个租户"); got != domains.ID {
		t.Fatalf("标识符命中应压过普通二元组（另一条只命中「租户」）: %q, 账单条目 %q", got, billing.ID)
	}
	if got := top("管理后台测试地址"); got != domains.ID {
		t.Fatalf("管理后台→管理端、地址→域名 应归一后命中: %q", got)
	}
	res, _ := s.Search(ctx, "a@x", SearchInput{Query: "gb-order 是干什么的"})
	if res.Total != 0 {
		t.Fatalf("标识符没命中时仍按门槛判断，不应因虚词命中: %+v", res.Items)
	}
}

func TestIsIdentifier(t *testing.T) {
	for tok, want := range map[string]bool{
		"gb-pigeon": true, "hg221": true, "k8s": true, "snake_case": true,
		"redis": false, "v1": false, "2026": false, "1.2.3": false, "管理": false,
	} {
		if got := isIdentifier(tok); got != want {
			t.Errorf("isIdentifier(%q) = %v, want %v", tok, got, want)
		}
	}
}

// 两路名次交叉时融合分相同，文本分高的应排前面，而不是按时间。
func TestFusionTieBreaksOnText(t *testing.T) {
	q := []float32{1, 0}
	at := func(cos float64) []float32 { return []float32{float32(cos), float32(math.Sqrt(1 - cos*cos))} }
	cands := []candidate{
		{Entry: Entry{ID: "semantic-first", Title: "平台", Body: "部署"}, vec: at(0.62)},
		{Entry: Entry{ID: "text-first", Title: "部署流程", Body: "部署"}, vec: at(0.60)},
	}
	hits := rankHybrid(cands, "部署流程", q)
	if len(hits) != 2 || hits[0].ID != "text-first" {
		t.Fatalf("同分时应让文本分高的在前: %+v", hits)
	}
}
