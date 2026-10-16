# claracore-server

团队 agent 的记忆与知识库，以 MCP 服务的形式部署在 bosun（`/Volumes/E61/zhouwei/projects/bosun`） 网关之后。

- **memory**：可观察的事实、决定、偏好、踩坑结论（短）。
- **knowledge**：agent 自己沉淀的长期笔记或文档（可长）。

条目归属于调用人（bosun 转发的 `X-Bosun-User`），默认只有本人的 agent 看得到；`shared=true` 时全团队可见，但只有作者能取代。过时或有误的条目用 `memory_supersede` 取代，旧版本保留可查，不删除。

> 2026-10 起这是精简版：Web 控制台、空间治理、审核流、Continuity、外部源全部移除，身份、授权、限流、审计交给 bosun。旧实现在 `main` 分支历史里。

## 工具

| 工具 | 只读 | 用途 |
|---|---|---|
| `memory_save` | 否 | 保存一条记忆或知识（title、body、kind、tags、source、shared） |
| `memory_search` | 是 | 按情境检索；不给 query 时按时间倒序列出。正文默认截到 300 字 |
| `memory_get` | 是 | 按 id 读全文，包括已被取代的条目 |
| `memory_supersede` | 否 | 用新内容取代自己的一条，或只作废（需 reason） |

结果遵循 shipwright 契约 v1（`shipwright/docs/CONTRACT.md`）：单个紧凑 JSON 文本块，省略内容时带 `_more` 与 `_meta["shipwright/more"]`，失败带错误码。

## 检索

在进程内对可见的有效条目（按时间倒序最多 5000 条）打分：

- **文本**：拉丁词按词、中文按相邻二字匹配，标题 > 标签 > 正文加权；不需要分词和全文索引。
- **语义**（可选）：配置 embedding 服务后，按余弦相似度（≥ 0.5）取前 50 条。
- 两路按名次做倒数排名融合（RRF），每条结果标注 `match`：text / semantic / both。

结果里的 `semantic` 如实说明本次检索的状态：`off`（未配置）、`ok`、`unavailable`（向量服务失败，本次只按文本，附 `semantic_error`）、`skipped`（无 query）。`unindexed` 是还没有向量、只能靠文本命中的条目数。写入时会顺手算向量，失败不影响写入，后台每分钟补算；换模型后旧向量会自动重算。

## 运行

```bash
make build
CLARACORE_TOKEN=<网关凭据> ./claracore-server -addr 0.0.0.0:9830 -db /data/claracore.db
```

| 环境变量 | 说明 |
|---|---|
| `CLARACORE_TOKEN` | 网关的上游凭据（`Authorization: Bearer`）。监听非回环地址时必填 |
| `CLARACORE_ADDR` / `CLARACORE_DB` | 监听地址（默认 `127.0.0.1:9830`）/ SQLite 文件（默认 `claracore.db`） |
| `CLARACORE_EMBEDDING_PROVIDER` | `ollama` 或 `openai`（OpenAI 兼容接口）；不设则只做文本检索 |
| `CLARACORE_EMBEDDING_URL` / `_MODEL` / `_API_KEY` | 向量服务地址、模型（如 `bge-m3`）、密钥 |
| `CLARACORE_DEV_USER` | 仅本机调试：请求不带 `X-Bosun-User` 时使用的身份。生产留空 |

`GET /health` 不需要令牌，返回条目数、未建索引数与语义检索是否开启；数据库不可用时返回 503。

## 在 bosun 登记

1. 上游地址 `http://<主机>:9830/mcp`，上游请求头 `Authorization: Bearer <CLARACORE_TOKEN>`。
2. 勾选 **per_caller**（按调用人区分的服务）：bosun 会拒绝为它开结果缓存，并要求上游凭据。不勾的话，A 的检索结果可能经缓存返回给 B。
3. 令牌的「身份标识」统一填写（如邮箱）。条目按这个值归属，改了就对不上旧条目。
4. 「连接时说明」可写一句用法提示，例如：开始任务前用 memory_search 查相关旧记忆，用户说「记一下」时用 memory_save。

## 开发

```bash
make check   # gofmt + vet + test
```
