// Package embedding 是 ports.Embedder 的 HTTP 实现。
//
// 服务端不做本地推理（docs/01-architecture.md §2.2）：Desktop 那个
// @xenova/transformers 本地 ONNX 推理是为「下载即用」的桌面体验存在的，
// 搬到服务端只是负担——多一份模型文件、多一份内存、多一条与业务无关的
// 启动失败路径，换来的好处（不用另装 Ollama）在一台服务器上根本不成立。
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTimeout 是单次批量调用的上限。
//
// 它同时是索引 worker 卡住的上限：一次 Embed 阻塞多久，那一轮 tick
// 就停多久。设得太长的话，一个半死不活的 embedding 服务会让队列
// 看起来像"在处理"，其实一条也没动。
const DefaultTimeout = 30 * time.Second

// maxErrorBody 是从错误响应里读多少字节用于报错。
//
// 截断不是省内存：这段文本最终会写进 embeddings.last_error 列并显示在
// Web 治理面上，一整页 HTML 错误页糊进去只会让真正的原因更难看见。
const maxErrorBody = 512

// client 是两个 provider 共用的 HTTP 部分。
type client struct {
	http    *http.Client
	baseURL string
	model   string
	apiKey  string

	// provider 只用于错误信息，让运维一眼看出是哪一路出的问题。
	provider string
}

func newClient(provider, baseURL, model, apiKey string, timeout time.Duration) client {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return client{
		http:     &http.Client{Timeout: timeout},
		baseURL:  strings.TrimRight(baseURL, "/"),
		model:    model,
		apiKey:   apiKey,
		provider: provider,
	}
}

// postJSON 发一次 JSON 请求并解码响应。
//
// # 错误信息为什么这么克制
//
// 返回的 error 会被领域层原样写进 embeddings.last_error，而那一列会显示在
// Web 治理面上。Go 的 http 错误默认带着完整 URL（含内网地址与端口），
// 请求头里还可能有 API key——所以这里**不把底层 error 直接包进去**，
// 只保留 provider、状态码和响应体里那段人写的原因。
//
// 代价是排查一次连接问题要多看一眼服务日志。这个取舍是有意的：
// 内网拓扑泄露一次就收不回来，而日志随时能看。
func (c client) postJSON(ctx context.Context, path string, reqBody, out any) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("embedding(%s): 请求体编码失败", c.provider)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("embedding(%s): 请求构造失败", c.provider)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// 刻意不带 err：它含完整 URL。超时与连接拒绝在这里是同一类
		// 「服务不可用」，worker 的处理方式也完全相同。
		return fmt.Errorf("embedding(%s): 服务不可达或超时", c.provider)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		msg := strings.TrimSpace(string(detail))
		if resp.StatusCode == http.StatusNotFound {
			// 最常见的一次性配置错误：接口路径对不上。直接说清楚要什么。
			return fmt.Errorf("embedding(%s): 接口 %s 不存在（HTTP 404），请确认服务版本与 CLARACORE_EMBEDDING_URL", c.provider, path)
		}
		return fmt.Errorf("embedding(%s): 服务返回 HTTP %d: %s", c.provider, resp.StatusCode, msg)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("embedding(%s): 响应不是预期的 JSON 结构", c.provider)
	}
	return nil
}

// checkBatch 校验返回的向量满足 ports.Embedder 的约定。
//
// 三条都必须当场拦住，因为它们的共同点是**不拦的话不会报错**：
// 数量不对会让向量错位安到别的内容头上，维度不一致会让余弦在
// 某些候选上悄悄失效，空向量会让一条内容永远匹配不上任何查询。
// 三种都是事后几乎无法定位的故障。
func (c client) checkBatch(vectors [][]float32, want int) error {
	if len(vectors) != want {
		return fmt.Errorf("embedding(%s): 返回 %d 个向量，期望 %d 个", c.provider, len(vectors), want)
	}
	dim := 0
	for i, v := range vectors {
		if len(v) == 0 {
			return fmt.Errorf("embedding(%s): 第 %d 个向量为空", c.provider, i)
		}
		if dim == 0 {
			dim = len(v)
			continue
		}
		if len(v) != dim {
			return fmt.Errorf("embedding(%s): 同一批向量维度不一致（%d vs %d）", c.provider, dim, len(v))
		}
	}
	return nil
}
