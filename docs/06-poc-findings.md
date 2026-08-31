# PoC 验证结论

> 执行日期：2026-08-05
> 代码：`apps/claracore-server/poc/`（丢弃型验证代码，非正式结构）
> SDK：`github.com/modelcontextprotocol/go-sdk v1.7.0`，Go 1.26.6

---

## 结论：架构决策全部成立，可以开工

四项验证目标全部通过，**没有需要推翻的架构决策**。

| # | 验证项 | 结果 |
|---|---|---|
| 1 | `Stateless = true` 能否正常服务 | ✅ 通过 |
| 2 | bearer token 认证链路 | ✅ 通过（无 token / 错误 token 均正确返回 401） |
| 3 | token → `(tenant, user, agent)` 传递到 tool handler | ✅ **完全打通** |
| 4 | Claude Code 实际连接 | ✅ **实测连接成功** |

---

## 1. 最大风险已解除：旧协议在 stateless 下正常协商

这是开工前唯一的阻塞项（`01-architecture.md` §4.2）。

实测协商结果：

| 客户端请求版本 | 服务端协商结果 |
|---|---|
| `2026-07-28` | `2025-11-25` |
| `2025-11-25` | `2025-11-25` |
| `2025-03-26` | `2025-03-26` |

**stateless 模式对旧协议向后兼容良好**，`2024-11-05` ~ `2025-11-25` 全部在支持列表内。

### 1.1 为什么请求 `2026-07-28` 会得到 `2025-11-25`

这不是 bug。SDK 源码 `mcp/shared.go` 的 `negotiatedVersion()`：

```go
if slices.Contains(supportedProtocolVersions, clientVersion) && clientVersion < protocolVersion20260728 {
    return clientVersion
}
return protocolVersion20251125
```

注释说明了原因：**`2026-07-28` 废弃了 `initialize` 握手**（SEP-2575 的无握手方向）。因此任何走 `initialize` 路径的客户端，协商上限就是 `2025-11-25`。

要真正使用 `2026-07-28`，客户端必须走**无握手路径**：不发 `initialize`，每个请求在 `_meta` 中携带协议版本与 capabilities。服务端侧 SDK 已就绪，等客户端跟进即可，**无需我们做任何事**。

---

## 2. Claude Code 实测行为

服务端记录到的真实请求：

```
[REQ] method=initialize            protocol="2025-11-25" client=claude-code/2.1.222 meta=false
[REQ] method=notifications/initialized                    client=claude-code/2.1.222
[REQ] method=tools/list                                   client=claude-code/2.1.222
```

`claude mcp list` 结果：`claracore-poc: http://127.0.0.1:51888/mcp (HTTP) - ✔ Connected`

**要点**：

- Claude Code 2.1.222 **已支持 `2025-11-25`**——2026-07 时观察到的"客户端跟不上"问题现已不存在
- 它仍走 `initialize` 握手，未使用 `2026-07-28` 的 `_meta` 无握手机制（`meta=false`）
- **stateless 模式与 Claude Code 完全兼容**

---

## 3. 「token 即身份」验证通过

三个 token 换到三种身份，`01-architecture.md` §5.1 的设计完全落地：

| token | tenantId | userId | agentId | readableSpaces |
|---|---|---|---|---|
| `ccs_poc_clara_001` | `t_demo` | `u_zhouwei` | **clara** | personal:zhouwei, team:demo |
| `ccs_poc_codex_002` | `t_demo` | `u_zhouwei` | **codex** | personal:zhouwei, team:demo |
| `ccs_poc_other_003` | `t_other` | `u_someone` | clara | personal:someone |

**验证结论**：

- 同一 user 的两个 agent 被正确区分 → **agent 作为审计维度可行**
- 跨租户身份隔离成立
- 身份完全来自 token，**不读取任何客户端声明的 header**

### 3.1 关键实现发现：context 可直接贯通

**HTTP 中间件设置的 context 能直接传递到 MCP tool handler。**

因此身份传递用标准 `context.WithValue` 即可：

```
auth.RequireBearerToken(verifier) → injectIdentity(注入 context) → MCP handler → tool handler 读取
```

**不需要**用 `getServer(*http.Request)` 的闭包方案为每个身份构造独立 server 实例。这显著简化了正式实现——`getServer` 可保留给将来真正需要按租户切换工具集的场景。

SDK 的 `auth.TokenInfo` 提供 `UserID` 与 `Extra map[string]any`，租户与 agent 信息放 `Extra` 即可。

---

## 4. 需要修正的既有设计

### 4.1 nginx 配置可以简化（修正 `01-architecture.md` §9.1）

实测：**stateless 模式下 `GET` 与 `DELETE` 均返回 `405 Method Not Allowed`**。

这意味着**没有 GET 建立的 SSE 长连接**，所有交互都是 POST 请求-响应。因此：

- ❌ `proxy_read_timeout 3600s` — **不需要**，没有长连接要保活
- ✅ `proxy_buffering off` — **仍然需要**，因为 POST 的响应体仍是 `text/event-stream`（实测响应头 `Content-Type: text/event-stream`、`Transfer-Encoding: chunked`）

### 4.2 可选：`JSONResponse` 模式

`StreamableHTTPOptions.JSONResponse = true` 可让响应返回 `application/json` 而非 `text/event-stream`。

**若启用，nginx 就完全不需要任何特殊配置**（普通 JSON 反代即可）。代价是失去流式返回能力（如工具执行中的进度通知）。

**建议**：暂不启用，保留流式能力；但记录为部署遇阻时的逃生舱。**待实测**。

### 4.3 stateless 下 `InitializeParams` 是默认值

tool handler 中通过 `req.Session.InitializeParams().ProtocolVersion` 读到的是 `2025-03-26`（SDK 默认值），**不是客户端实际协商的版本**——因为 stateless 下每个请求使用临时 session 与默认初始化参数。

**影响**：业务逻辑不得依赖 session 中的协议版本或客户端信息。目前设计中无此依赖，仅作记录。

---

## 5. 附带确认

- `auth.RequireBearerToken` 默认**要求 `TokenInfo.Expiration` 非零**，否则拒绝。正式实现中 token 必须有过期时间，或显式设置 `AllowMissingExpiration`
- `RequireBearerTokenOptions` 提供 `ClockSkew`，多实例部署时应设置小的正值以容忍时钟漂移
- `mcp.AddTool[In, Out]` 泛型从 Go 结构体推断 JSON Schema，`jsonschema:"..."` tag 提供字段描述——工具定义无需手写 schema
- 国内环境需 `GOPROXY=https://goproxy.cn,direct`，默认 `proxy.golang.org` 拉不动

---

## 6. 下一步

阻塞项已清空，可以按 `05-code-structure.md` 的结构开始正式实现。建议顺序：

1. 项目骨架 + `test/arch_test.go` 依赖方向检查（**先立约束，再写业务**）
2. `migrations/` + governance 域（tenant/user/agent/token/space/permission）
3. MCP Host + auth（PoC 代码可作参考，但不直接复用）
4. knowledge 域（memory → card → recall_trigger）
5. 两段式检索 + embedding 适配器
6. Web Host
7. 摄取（md → agent 提炼 → 审核）
