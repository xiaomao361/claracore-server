package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 这一层的测试不需要真实的 embedding 服务：用 httptest 造一个，
// 反而能构造出真服务不会给你的那些坏响应——而正是那些坏响应
// 决定了这个适配器有没有价值。

func serve(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

// ollamaByInput 按请求里的文本给向量，并统计请求次数。
func ollamaByInput(t *testing.T, vecs map[string]string, calls *int) string {
	return serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("期望走 /api/embed，实际 %s", r.URL.Path)
		}
		var req struct{ Input []string }
		json.NewDecoder(r.Body).Decode(&req)
		*calls++
		var parts []string
		for _, in := range req.Input {
			parts = append(parts, vecs[in])
		}
		w.Write([]byte(`{"embeddings":[` + strings.Join(parts, ",") + `]}`))
	})
}

func TestOllamaOneTextPerRequest(t *testing.T) {
	// Ollama 的批量返回顺序不可靠（Local200 实测乱序），所以每段单独请求，对应关系由请求本身保证。
	calls := 0
	url := ollamaByInput(t, map[string]string{"a": "[1,0]", "b": "[0,1]"}, &calls)

	got, err := NewOllama(url, "bge-m3", "", time.Second).Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(got) != 2 || got[0][0] != 1 || got[1][1] != 1 {
		t.Fatalf("应逐段请求并按入参归位: calls=%d got=%v", calls, got)
	}
}

func TestOpenAICompatReordersByIndex(t *testing.T) {
	// 规范允许返回顺序与入参不同。按数组顺序取就会把向量安到别的内容头上。
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`))
	})

	got, err := NewOpenAICompat(url, "text-embedding-3-small", "k", time.Second).
		Embed(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0][0] != 1 || got[1][1] != 1 {
		t.Fatalf("必须按 index 归位而不是按返回顺序，实际: %v", got)
	}
}

func TestOpenAICompatSendsAPIKey(t *testing.T) {
	var auth string
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	})

	if _, err := NewOpenAICompat(url, "m", "secret-key", time.Second).
		Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer secret-key" {
		t.Fatalf("API key 未随请求发出，实际 Authorization = %q", auth)
	}
}

func TestRejectsCountMismatch(t *testing.T) {
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"embeddings":[[1,0],[0,1]]}`)) // 一段文本回了两个
	})

	_, err := NewOllama(url, "m", "", time.Second).Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("数量对不上必须报错——按下标硬配会让向量错位，事后无法定位")
	}
}

func TestRejectsInconsistentDim(t *testing.T) {
	calls := 0
	url := ollamaByInput(t, map[string]string{"a": "[1,0,0]", "b": "[0,1]"}, &calls)

	_, err := NewOllama(url, "m", "", time.Second).Embed(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("同批维度不一致必须报错——它会让余弦在部分候选上悄悄失效")
	}
}

func TestRejectsEmptyVector(t *testing.T) {
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"embeddings":[[]]}`))
	})

	_, err := NewOllama(url, "m", "", time.Second).Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("空向量必须报错——它会让这条内容永远匹配不上任何查询")
	}
}

// TestErrorLeaksNoInternalAddress 是一条安全属性。
//
// 返回的 error 会被写进 embeddings.last_error 并显示在 Web 治理面上。
// Go 的 http 错误默认带完整 URL（含内网地址与端口），直接包进去
// 就是把内网拓扑写进了一张会被展示的表。
func TestErrorLeaksNoInternalAddress(t *testing.T) {
	// 指向一个不会有人监听的地址，触发连接失败
	_, err := NewOllama("http://192.168.31.244:65533", "m", "", 200*time.Millisecond).
		Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("连接失败应报错")
	}
	if strings.Contains(err.Error(), "192.168.31.244") || strings.Contains(err.Error(), "65533") {
		t.Fatalf("错误信息泄露了内网地址: %v", err)
	}
}

func TestNotFoundExplainsEndpoint(t *testing.T) {
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := NewOllama(url, "m", "", time.Second).Embed(context.Background(), []string{"a"})
	if err == nil || !strings.Contains(err.Error(), "/api/embed") {
		t.Fatalf("404 是最常见的一次性配置错误，报错该说清楚缺的是哪个接口，实际: %v", err)
	}
}

func TestUpstreamErrorBodyIsTruncated(t *testing.T) {
	url := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(strings.Repeat("x", maxErrorBody*4)))
	})

	_, err := NewOllama(url, "m", "", time.Second).Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("上游 500 应报错")
	}
	// last_error 列宽有限，一整页错误页糊进去只会淹掉真正的原因
	if len(err.Error()) > maxErrorBody*2 {
		t.Fatalf("错误信息未截断，长度 %d", len(err.Error()))
	}
}
