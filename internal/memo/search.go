package memo

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// scanCap 是一次检索扫描的有效条目上限（按时间倒序）。检索在进程内打分：
	// 一个团队的记忆量级下，扫几千条比维护一套全文索引简单可靠，中文也不用分词。
	// 超出时结果里会标 scan_capped，不假装看过全部。
	scanCap = 5000

	defaultLimit = 10
	maxLimit     = 50

	// semanticFloor 以下的余弦不算语义命中。bge-m3 上无关文本多在 0.3~0.45，相关的在 0.5 以上；
	// 没有标注评估集之前先这样，命中时把 cosine 原值返回，由 agent 自己判断。
	semanticFloor = 0.5
	semanticTopK  = 50

	// rrfK 是倒数排名融合的常数。融合只看名次，不看两路分数的尺度——两路分数本来就不可比。
	rrfK = 60

	queryEmbedWait = 5 * time.Second
)

type SearchInput struct {
	Query  string
	Kind   string   // 空 = 记忆与知识都查
	Tags   []string // 同时具有全部标签
	Scope  string   // all（默认）| mine | shared
	Limit  int
	Offset int
}

type Hit struct {
	Entry
	Match  string   `json:"match,omitempty"`  // text | semantic | both；无 query 时为空
	Cosine *float64 `json:"cosine,omitempty"` // 语义命中时的余弦
}

// SearchResult 按 shipwright 分页结构返回。Semantic 说明这次是否用上了向量：
// off（未配置）| ok | unavailable（向量服务失败，本次只按文本）| skipped（无 query）。
type SearchResult struct {
	Items      []Hit  `json:"items"`
	Total      int    `json:"total"`
	Offset     int    `json:"offset"`
	NextOffset *int   `json:"next_offset"`
	Semantic   string `json:"semantic"`
	// SemanticError 只在 unavailable 时出现。
	SemanticError string `json:"semantic_error,omitempty"`
	// Unindexed 是参与检索但还没有向量的条目数：它们这次只能靠文本命中。
	Unindexed  int  `json:"unindexed,omitempty"`
	ScanCapped bool `json:"scan_capped,omitempty"`
}

type candidate struct {
	Entry
	vec []float32
}

func (s *Service) Search(ctx context.Context, caller string, in SearchInput) (SearchResult, error) {
	if err := checkOwner(caller); err != nil {
		return SearchResult{}, err
	}
	limit, offset := in.Limit, in.Offset
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return SearchResult{}, fail(CodeInvalid, "limit 取值 1~%d", maxLimit)
	}
	if offset < 0 {
		return SearchResult{}, fail(CodeInvalid, "offset 不能为负")
	}
	if in.Kind != "" && in.Kind != KindMemory && in.Kind != KindKnowledge {
		return SearchResult{}, fail(CodeInvalid, "kind 只能是 memory、knowledge 或留空")
	}
	tags, err := normalizeTags(in.Tags)
	if err != nil {
		return SearchResult{}, err
	}

	where := []string{"status = 'active'"}
	args := []any{}
	switch in.Scope {
	case "", "all":
		where = append(where, "(owner = ? OR shared = 1)")
		args = append(args, caller)
	case "mine":
		where = append(where, "owner = ?")
		args = append(args, caller)
	case "shared":
		where = append(where, "shared = 1")
	default:
		return SearchResult{}, fail(CodeInvalid, "scope 只能是 all、mine 或 shared")
	}
	if in.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, in.Kind)
	}
	for _, t := range tags {
		where = append(where, "tags LIKE ? ESCAPE '\\'")
		args = append(args, "% "+escapeLike(t)+" %")
	}

	query := strings.TrimSpace(in.Query)
	model := ""
	if s.embedder != nil {
		model = s.embedder.Model()
	}
	cands, capped, err := s.load(ctx, strings.Join(where, " AND "), args, model, query != "")
	if err != nil {
		return SearchResult{}, err
	}

	res := SearchResult{Offset: offset, ScanCapped: capped, Semantic: "skipped"}
	var ranked []Hit
	if query == "" {
		// 无 query：按时间倒序列出，相当于「最近写了什么」。
		for _, c := range cands {
			ranked = append(ranked, Hit{Entry: c.Entry})
		}
		if s.embedder == nil {
			res.Semantic = "off"
		}
	} else {
		var qvec []float32
		switch {
		case s.embedder == nil:
			res.Semantic = "off"
		default:
			ectx, cancel := context.WithTimeout(ctx, queryEmbedWait)
			vecs, err := s.embedder.Embed(ectx, []string{query})
			cancel()
			if err != nil {
				res.Semantic, res.SemanticError = "unavailable", err.Error()
			} else {
				res.Semantic, qvec = "ok", vecs[0]
			}
		}
		ranked = rankHybrid(cands, query, qvec)
		if qvec != nil {
			for _, c := range cands {
				if c.vec == nil {
					res.Unindexed++
				}
			}
		}
	}

	res.Total = len(ranked)
	if offset < len(ranked) {
		end := min(offset+limit, len(ranked))
		res.Items = ranked[offset:end]
		if end < len(ranked) {
			res.NextOffset = &end
		}
	}
	if res.Items == nil {
		res.Items = []Hit{}
	}
	return res, nil
}

// load 取可见的有效条目，按时间倒序，最多 scanCap 条。只有需要语义打分时才读向量。
func (s *Service) load(ctx context.Context, where string, args []any, model string, withVec bool) ([]candidate, bool, error) {
	vecCol := "NULL"
	if withVec && model != "" {
		vecCol = "CASE WHEN embed_model = ? THEN embedding END"
		args = append([]any{model}, args...)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryCols+`, `+vecCol+` FROM entries WHERE `+where+
		` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, scanCap+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var blob []byte
		e, err := scanEntry(rows, &blob)
		if err != nil {
			return nil, false, err
		}
		c := candidate{Entry: e}
		if len(blob) > 0 {
			c.vec = decodeVector(blob)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > scanCap {
		return out[:scanCap], true, nil
	}
	return out, false, nil
}

// rankHybrid 文本与语义两路各自排名，再按倒数排名融合。qvec 为 nil 时只有文本一路。
func rankHybrid(cands []candidate, query string, qvec []float32) []Hit {
	tokens := tokenize(query)
	type scoredIdx struct {
		i     int
		score float64
	}
	var text, sem []scoredIdx
	for i, c := range cands {
		if sc := textScore(tokens, c.Entry); sc > 0 {
			text = append(text, scoredIdx{i, sc})
		}
		if qvec != nil && c.vec != nil {
			if cos := cosine(qvec, c.vec); cos >= semanticFloor {
				sem = append(sem, scoredIdx{i, cos})
			}
		}
	}
	// 同分保持原顺序（时间倒序），所以用稳定排序。
	byScore := func(a, b scoredIdx) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return 0
	}
	slices.SortStableFunc(text, byScore)
	slices.SortStableFunc(sem, byScore)
	if len(sem) > semanticTopK {
		sem = sem[:semanticTopK]
	}

	fused := map[int]float64{}
	match := map[int]string{}
	cos := map[int]float64{}
	for r, x := range text {
		fused[x.i] += 1.0 / float64(rrfK+r+1)
		match[x.i] = "text"
	}
	for r, x := range sem {
		fused[x.i] += 1.0 / float64(rrfK+r+1)
		cos[x.i] = math.Round(x.score*1e4) / 1e4
		if match[x.i] == "text" {
			match[x.i] = "both"
		} else {
			match[x.i] = "semantic"
		}
	}
	order := make([]int, 0, len(fused))
	for i := range fused {
		order = append(order, i)
	}
	slices.SortFunc(order, func(a, b int) int {
		if fused[a] != fused[b] {
			if fused[a] > fused[b] {
				return -1
			}
			return 1
		}
		return a - b // 同分按时间倒序
	})
	hits := make([]Hit, len(order))
	for k, i := range order {
		hits[k] = Hit{Entry: cands[i].Entry, Match: match[i]}
		if c, ok := cos[i]; ok {
			hits[k].Cosine = &c
		}
	}
	return hits
}

// tokenize 把查询切成匹配单元：拉丁字母与数字按词（至少 2 个字符），中日韩文字按相邻两字。
// 不做分词：二元组覆盖率对短查询足够，而且不会因为词典缺词而漏召回。
func tokenize(q string) []string {
	q = strings.ToLower(q)
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	var word []rune
	var cjk []rune
	flush := func() {
		if len(word) >= 2 {
			add(string(word))
		}
		word = word[:0]
		switch {
		case len(cjk) == 1:
			add(string(cjk))
		case len(cjk) > 1:
			for i := 0; i+1 < len(cjk); i++ {
				add(string(cjk[i : i+2]))
			}
		}
		cjk = cjk[:0]
	}
	for _, r := range q {
		switch {
		case isCJK(r):
			if len(word) > 0 {
				flush()
			}
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.':
			if len(cjk) > 0 {
				flush()
			}
			word = append(word, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// textScore 是加权覆盖率：标题命中记 3、标签 2、正文 1，除以全部命中标题时的满分。
// 命中不到三分之一的查询单元（两个以内则须全中）视为不相关，返回 0。
// 门槛不能更高：自然语言查询里的虚词（「怎么部署」里的「怎么」「么部」）会把有效的那个二元组淹掉。
func textScore(tokens []string, e Entry) float64 {
	if len(tokens) == 0 {
		return 0
	}
	title := strings.ToLower(e.Title)
	tags := " " + strings.Join(e.Tags, " ") + " "
	body := strings.ToLower(e.Body)
	matched, sum := 0, 0.0
	for _, t := range tokens {
		w := 0.0
		switch {
		case strings.Contains(title, t):
			w = 3
		case strings.Contains(tags, t):
			w = 2
		case strings.Contains(body, t):
			w = 1
		}
		if w > 0 {
			matched++
			sum += w
		}
	}
	need := (len(tokens) + 2) / 3
	if len(tokens) <= 2 {
		need = len(tokens)
	}
	if matched < need {
		return 0
	}
	return sum / float64(3*len(tokens))
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0 // 维度不同（换过模型）的旧向量不参与，等后台重算
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// Clip 把正文截到 max 个字符；返回是否截断与原长度，供 MCP 层产出 _more 标记。
func Clip(s string, max int) (string, bool, int) {
	n := utf8.RuneCountInString(s)
	if max <= 0 || n <= max {
		return s, false, n
	}
	return string([]rune(s)[:max]) + "…", true, n
}
