# ClaraCore Server — 接手指南

> 最后更新：2026-08-31
> 给下一次接手的人或 agent（Clara / Codex / 其他）

---

## 一句话状态

**团队测试实例已部署**：`http://192.168.3.200:51891` 由 `/home/gbjk/nginx` 的独立端口反代到 Supervisor 管理的 ClaraCore Server，应用目录是 `/home/gbjk/zhouwei/claracore-server`。管理员能在浏览器里管理账号、项目空间与权限、摄取源、索引和 token；agent 通过 24 个 MCP 工具管理 Memory、Card、自动上下文与 Shared Line。当前运行版本见本文「当前最远证据」。自动部署探针已从服务器内外通过；尚未完成的是登录后的人工视觉与完整业务验收。

首次部署使用 `claracore-server -bootstrap -bootstrap-password-file <0600绝对路径>` 原子创建首个租户、管理员、团队空间和管理员个人空间；非空部署单元会明确拒绝重复初始化，管理员首次登录必须改密。

`192.168.3.200` 已完成首次初始化：租户 `ClaraCore 团队`、管理员账号 `admin`、团队空间 `团队记忆`。临时密码只保存在服务器的 `/home/gbjk/zhouwei/claracore-server/.initial-admin-password`（0600），未写入配置或日志；首次登录强制进入改密页，改密交付完成后应删除该临时文件。

**整条链路闭环了**（0810 第六刀）。这是这个产品的一句话价值，第一次真的跑通：

```
docs/06-poc-findings.md → 摄取成 draft
                        → 人在浏览器里审：补上「理解」与两条召回条件
                        → 后台 worker 补向量
                        → agent 问「我在排查 MCP 传输层问题，想直接读
                          initialize 里的协议版本来做分支」
                        → score=0.7515，召回理由「遇到 MCP 传输层或
                          协议版本协商问题时」
```

0.7515 比之前那个对抗性构造的 0.6195 高得多——因为这次的召回条件是**人照着原文写的**，不是测试数据。

**活动流与完整审计已落地到代码**：`0006_audit_logs.sql`、独立的 `domain/activity` 与 MySQL Store、24 个 MCP 工具的统一装饰器、Web 写中间件、`/activity`，以及需要二次确认且先写 meta-audit 才能查询的 `/admin/audit`。Shared Line 审计只保留 line ID、版本和计数，不落摘要、下一步或交接正文；自动上下文选择与反馈用不含内容的随机 `contextId` 关联，可按租户汇总近 30 天选择、放弃、已关联反馈选择与 outcome。

**0811 这一刀已收到真库 + 真浏览器层**：`make check` 通过（domain 87.1%）；`0006` 已应用到 `development` MySQL；完整 `make itest` 通过，包括活动 SQL 可见性、租户隔离、keyset 分页、保留期删除与 MCP 真实写入。浏览器用隔离的临时租户走了登录 → `/activity` → `/admin/audit` 二次确认 → meta-audit 出现的完整链路，并补测 390px 窄屏。这些是开发环境验收，不是部署或生产验收。

两条端到端证据（`make itest` 每次真跑，Ollama 不在线则 skip）：

- `test/mcp_tools_integration_test.go` —— 走 MCP 协议的完整工作流：`memoria_create` 默认落个人空间 → 写不可写空间被 `FORBIDDEN` 拦下 → `card_create` 带召回条件 → 索引 worker 补上 3 条向量 → `card_recall` 命中并给出召回理由 → 乐观锁冲突带回当前版本 → 软删除后仍可按 ID 取到但不进检索。
- `test/recall_e2e_integration_test.go` —— 检索质量本身：一张召回条件与情境**没有字面重叠**的卡，加一张有字面重叠的干扰卡。纯全文路径把干扰卡排第一，两段式检索把正确的那张纠正回第一位（bge-m3 实测 0.6195 vs 0.6051）。**这条是唯一能证明检索「确实在起作用」而不只是「跑通了」的测试，别让它烂掉。**

`make run` 起来就是完整的服务：`/mcp` 是 MCP Host，`/` 是 Web 治理面，后台跑索引 worker 与 trace 保留期清理 worker。装配全在 `cmd/server/main.go`。

Web 那条链路 0810 在真浏览器会话里手跑过一遍：未登录访问首页 303 → 登录 → 被强制赶去改密页 → 改密后进概览（看到空间与权限）→ 签发 token 拿到明文与可直接粘贴的 MCP 配置块 → 建同事账号拿到生成的初始密码 → 不带 CSRF 的 POST 得到 403。0811 又在真浏览器补验了活动流与完整审计：普通时间线只显示自己全部 + 共享写入，管理员确认后能看到全量，且首条就是本次 `admin.audit.query` meta-audit。

**Web 生产视觉已在 0828 完成全页面统一**：沿用已验收的 Figma 工作台方向，`layout.html` 与嵌入 CSS 已收敛为持久桌面侧栏、工作区页头、概览指标、统一筛选工具栏、紧凑卡片/表格、活动流与审核双栏；680px 以下降级为可横向滚动的底部导航。跳到主要内容、单一 H1、`aria-current`、可见焦点、40px 控件、减弱动态、局部表格滚动、表格语义与 token / MCP 配置渐进增强复制均已落地。自动化浏览器逐页核对 11 个页面族在 1440 / 390px 下无整页横向溢出；尚待用户在目标服务器用真实身份完成主观视觉与业务操作验收。

**Continuity 已在 0827 落地**：`0008_continuity.sql`、领域 Service、MySQL Store 与 `shared_line_list/get/update/handoff/status` 五个工具均已实现。owner 稳定、跨 agent writer provenance、多线歧义 fail closed、位置乐观锁、归档/恢复、最近 10 条位置/交接历史和 exact resume packet 都有领域/MCP/真库测试。

**0827–0828 体验数据、共同线 Web、筛选与上下文能力已部署**：三个空间均有带 `[演示]` 前缀和 `demo-seed-20260827-v1` / `demo-seed` 标签的 Memory / Card，覆盖 active、archived、superseded、deleted、draft、deprecated、事实替代、卡片演进、跨实体关联与提升来源；另有一条 `ClaraCore Server 体验数据与筛选验收` Shared Line。它们是明确标记的拟真测试数据，不是业务事实。共同线现为左侧导航中的独立页面，按进行中/已归档/全部查看租户内各 agent；详情展示当前位置、位置历史和交接历史，可归档/恢复且经过 CSRF 与治理审计。内容浏览支持在类型、空间、状态范围内全文查找，查询与游标会安全编码并跨页保留；下拉筛选由同源 `/static/app.js` 选择即提交，切换类型会清除不兼容状态，并保留“应用筛选”作为无 JavaScript 兜底。自动 Card 上下文已与 Desktop 对齐为确定性 Stage B 预筛，终端 Agent 的模型做最终判断；服务端不再运行第二个大模型。Memory 原子 supersede、Card 原子 evolution、Shared Line 生命周期/历史与反馈治理均已包含。Nginx `/mcp` 上游 Host 固定为 loopback，避免 SDK 的 DNS-rebinding 防护把普通客户端拒为 403。

---

## 接手指引：先读哪三段

这份文档有 600 多行，但接手时**只有三段是必读的**：

1. **本节上面的「一句话状态」** —— 现在在哪。
2. **§下一步** —— 从哪开始，以及为什么是这个顺序。
3. **你要动的那一层的「已固化的决定」** —— 每层一段，标题都写着「动 XX 之前先读这段」。它们记的是**为什么这么做**和**踩过什么坑**，比读代码快得多。

其余按需查。§已知的坑那张表值得扫一眼——里面全是"不报错但会静默出错"的东西。

**一条纪律**：改任何一层之前，先看那一层有没有反向验证探针的记录（§已经站住的代码 里的探针清单）。新增约束或新写一层适配器后，用同样方式验证一次——**从不失败的检查等于没有检查**。

---

## 2026-08-10 这一天：六刀

按顺序，每一刀都有真库/真模型验证：

| # | commit | 内容 |
|---|---|---|
| 1 | `c86709a` | 检索：0003 迁移 + `ports/Embedder` + 两段式检索 + 索引 worker |
| 2 | `3690555` | MCP 工作面：16 个工具打通 token → 权限 → 知识 |
| 3 | `64449d9` | 文档对齐：各设计文档从「设计中，无代码」更新到实际状态 |
| 4 | `220e1b3` | Web 治理面：登录 / 强制改密 / 概览 / token / 成员管理 |
| 5 | `904bf3a` | 摄取：Markdown 目录 → 幂等的 draft 原料 |
| 6 | `f3f41af` | 卡片审核 + 内容浏览 → **整条链路闭环** |

这一天的反向验证探针共 **70 条**（16+14+24+16），漏网 0；另有 1 条打在非缺陷上、3 条探针本身写错（编译失败或打错测试）。详见 §已经站住的代码。

---

## 30 秒上手

```bash
cd /Users/zhouwei/Documents/ClaraCore/apps/claracore-server

make check     # 全部检查：fmt + vet + 架构约束 + domain 覆盖率门槛 + 测试。必须全绿才提交
make migrate   # 应用数据库迁移（幂等，已是最新时 no-op）
make itest     # 集成测试（schema + Store 契约 + MCP 端到端），需真实 MySQL。改完迁移或 SQL 必须手跑
make run       # 启动服务（需要 .env，已存在）

# 摄取一个 Markdown 目录（幂等，可反复跑）。空间 ID 与用户 ID 从 Web 的概览页取
go run ./cmd/server -ingest ./docs \
  -ingest-space <空间ID> -ingest-source clara-docs -ingest-user <用户ID>
curl http://127.0.0.1:51890/livez
curl http://127.0.0.1:51890/readyz
```

`.env` 已由毛仔创建（含真实密码，已 gitignore，**不要读取、不要复制其内容到任何文件或对话**）。模板见 `.env.example`。

### 环境（已就绪，已验证连通）

| 组件 | 地址 | 备注 |
|---|---|---|
| MySQL | `192.168.3.200:3316` | 8.0.36，库 `claracore_server`，专用账号（非 root），`ngram_token_size=2` 已确认 |
| Redis | `192.168.3.200:6381` | 7.0.15，key 前缀 `ccs:`，**可选**（未配置时系统降级仍完整可用） |
| Go | 1.26.6 | 国内需 `GOPROXY=https://goproxy.cn,direct` |
| MCP SDK | `modelcontextprotocol/go-sdk v1.7.0` | PoC 已验证 |
| Embedding | Ollama `127.0.0.1:11434`，模型 `bge-m3`（1024 维）| **可选**。`CCS_EMBEDDING_PROVIDER=disabled` 时检索降级为中文全文匹配，功能完整 |

---

## 文档导航

**`docs/` 是设计基准，本文档是落地实况。** 两者冲突时以本文档与代码为准——落地时改掉的设计草案都在 `docs/` 里就地标注了，不必两边猜。

| 文档 | 什么时候看 | 状态 |
|---|---|---|
| `docs/01-architecture.md` | 想知道"为什么这么设计"、部署形态、nginx 配置 | §11.0 有实现进度表 |
| `docs/02-data-model.md` | **写迁移文件前必读**。表结构、三层空间、检索模型、并发控制 | 已落地，偏差就地标注 |
| `docs/03-mcp-contract.md` | 写 MCP 工具前必读 | 24/24 已落地 |
| `docs/08-usage-guide.md` | 实际接入、调用、验收和缺口速查 | 与当前测试部署同步 |
| `docs/04-web-scope.md` | 写 Web 前必读。页面清单 + **可见性边界** | 活动流与管理员完整审计已实现，§3.3 有进度表 |
| `docs/05-code-structure.md` | **动手前必读**。目录结构、依赖方向、约束机制 | 目录树已按实际结构更新 |
| `docs/06-poc-findings.md` | 遇到 MCP 协议/传输层问题时查 | 已验证的结论与已知限制 |

各文档末尾的「待确认」清单已于 2026-08-06 全部拍板，速查表见本文 §已拍板。**目前没有未决产品决策**；已知工程改进见本文「缺口总览」，人工浏览器与完整业务验收仍需用户完成。

---

## 已经站住的代码

```
test/arch_test.go              依赖方向 + 领域层无包级变量。10 条规则
test/schema_test.go            迁移文件 schema 规则（7 条）+ 编号连续性
test/schema_integration_test.go 库里的实际结构（make itest，需真实 MySQL）
internal/config/               唯一读环境变量的地方。启动时校验，失败即 fatal
internal/platform/             Clock（可测试的时间）+ ULID（6 个测试全绿）
internal/adapters/mysql/       连接池（启动即 Ping）+ 迁移执行器（12 个拆分用例）
  └─ governance.go             governance.Store 的 MySQL 实现，11 个方法
  └─ knowledge.go              knowledge.Store 的写入侧实现，13 个方法
  └─ activity.go               audit_logs 写入、SQL 可见性查询、管理员查询、trace 分批清理
  └─ search.go                 粗筛 SQL：权限下推、MATCH 打分、标签交集、候选截断
  └─ embedding.go              向量 BLOB 编解码 + 索引队列（upsert / 领活 / 写回 / 标失败）
  └─ *_integration_test.go     对着真实 MySQL 的契约测试（make itest）
internal/domain/governance/    身份认证 + SpaceScope 解析 + token 生命周期
internal/domain/knowledge/     memory / card / 召回条件 / 标签 / 关联 / promote
  └─ search.go                 两段式检索编排：memoria_search / card_search / card_recall
  └─ embedding.go              索引队列：入队、批量算、失败重试、待 embedding 的文本怎么拼
  └─ store_fake_test.go        行为基准：剥离关联列、乐观锁、唯一键、事务回滚
                                 └─ store_fake_search_test.go 检索侧的行为基准：空间/租户/状态过滤、标签交集、
                               MATCH 只排序不过滤、内容未变不重算
                               （domain/ 合计 87.1% 覆盖，门槛 85）
internal/domain/activity/      不可变活动事件 + 分类校验 + keyset 分页 + 保留期清理契约
internal/ports/embedder.go     Model() + Embed()。纯基础设施接口，不认识领域概念
internal/adapters/embedding/   Ollama（/api/embed 批量）+ OpenAI-compatible（按 index 归位）
  └─ embedding_test.go         用 httptest 造真服务不会给你的坏响应：数量对不上、
                               维度不一致、空向量、404、错误信息泄露内网地址
internal/mcp/                  MCP Host：bearer 认证 + 身份注入 + 错误映射 + 24 个工具
  └─ activity.go               24 个工具统一审计装饰器 + 强类型安全元数据提取
  └─ knowledge.go              编排骨架：begin() 是唯一的权限解析入口 + 结果编码
  └─ memory.go / card.go / space.go   Memory 6 + Card 5 + 空间 3
  └─ continuity.go / context.go       Continuity 5 + 系统/上下文 5
  └─ host_test.go              8 个协议层测试，用 fake 治理服务，不碰数据库
  └─ tools_test.go             24 个工具层测试，用只记录入参的 fake 核心域——
                               这一层唯一该测的是「Host 往领域层传了什么」
internal/web/                  Host B：治理面（SSR + htmx 的位置，第一版还没用上 htmx）
  └─ server.go                 路由三层门 + 每页一套模板 + 安全响应头
  └─ session.go                会话 cookie、CSRF、requireUser/PasswordSet/Admin
  └─ handlers.go               登录 / 改密 / 概览 / 我的接入 / 成员管理
  └─ content.go                卡片审核（Web 唯一的内容写入场景）+ 内容浏览
  └─ activity.go               Web 写审计 + 普通活动流 + 管理员二次确认/meta-audit
  └─ templates|static/         //go:embed，CSS 编进二进制
  └─ web_test.go               33 个测试，全是安全属性：越权、CSRF、cookie、转义
internal/domain/knowledge/list.go  ListContents：治理面的列表查询（**不是检索**）
internal/adapters/crypto/      argon2id，参数编码进 PHC 串（调参不锁死存量用户）
migrations/0004_sessions.sql   Web 会话表，与 MCP token 彻底分开
migrations/0005_ingest.sql     cards 加 source_key / source_hash + uk_cards_source（幂等锚点）
migrations/0006_audit_logs.sql 活动/审计日志 + 普通可见性、管理员时间线、trace 清理索引
internal/ingest/markdown.go    第一个知识源：本地 Markdown 目录。**不提炼**
  └─ markdown_test.go          7 个测试：源身份稳定性、指纹跟内容走、坏文件不中断整批
internal/domain/knowledge/
  └─ source.go                 Document + DocumentSource（接口在 domain，实现在 ingest）
  └─ ingest.go                 幂等三分支 + 失败清单
test/mcp_e2e_integration_test.go  完整装配：真库 + 真 token 打通 whoami（make itest）
test/mcp_tools_integration_test.go 全栈工作流：真 token → MCP → 真库 → 真 Ollama
migrations/0001_governance.sql 治理域 6 张表，已应用到 192.168.3.200 的库
migrations/0002_knowledge.sql  核心域 5 张表，已应用
migrations/0003_embeddings.sql 运行时域 embeddings 表 + 给 card_recall_triggers 补 ngram 全文索引
cmd/server/main.go             最小入口，装配 + 优雅关闭 + -migrate + 两类后台 worker
Makefile                       make check 是提交前的门槛
poc/                           丢弃型验证代码，独立 go.mod，不参与主模块构建
```

**约束已反向验证过**：造过违规探针，9 + 3 + 8 + 4 + 7 + 21 + 14 + 14 + 24 + 16 + 16 条违规全部被捕获（依次是：架构与 schema 规则、`ascii_bin` 规则、governance Store、`ErrAlreadyExists` 映射与 fake 唯一性、MCP Host 的安全属性、knowledge 领域层与 Store、检索与索引队列）。新增约束或新写一层适配器后请用同样方式验证一次——**从不失败的检查等于没有检查**。

0810 检索那 14 条的清单（漏网 0）：领域层 6 条（检索空间过滤失效、语义与全文命中混排、查询向量算不出时报错而不降级、错位向量按下标硬配、替换召回条件不清旧向量、入队不与内容同事务）；Embedder 适配器 2 条（按数组顺序取向量而非 index、错误信息带上底层 error 泄露内网地址）；MySQL 5 条（粗筛漏租户隔离、召回权限信冗余列、upsert 里 content_hash 提前赋值、MATCH 进 WHERE 丢掉字面没命中的候选、标签过滤退化成并集）；schema 1 条（造一张没有 ngram 的 FULLTEXT 探针表，确认那条检查真的会红）。

0810 审核与浏览那 16 条（漏网 0，但**两条探针第一版打错了**）：列表与权限 3 条、游标分页 5 条、审核页 6 条、浏览页 2 条。

两条打错的都值得记：① 「取满即认为还有下一页」我拿 `TestNoNextCursorOnExactFit` 去打，而那条测试恰好在改动后仍然成立——真正会红的是 `TestCursorPaginationCoversEverythingExactlyOnce`（去掉多取一条之后**永远翻不到第 2 页**）。**探针打错测试，看起来和漏网一模一样。** ② 另一条注入后 `errors` 变成未使用的 import，编译失败——那种 `go test` 非零是假阳性。两条都重打之后捕获。

0810 摄取那 16 条（漏网 0，另有 1 条探针**打在了非缺陷上**）：幂等 3 条（内容没变也重写、找不到就一律新建、同批重复源标识放行）；保住人的劳动 3 条（源更新覆盖提炼与理解、产物落成 active、摄取把状态改回 draft）；失败清单 2 条；Markdown 源 6 条（SourceKey 掺绝对路径、不同源同名文件撞车、不跳过 .git、标题退回空、超限文件静默跳过、指纹不跟内容走）；MySQL 2 条。

**那条非缺陷值得记，它和 0807 是同一形态**：去掉 `FindCardBySourceKey` 里 `sourceKey == ""` 的提前返回，测试照样绿。查了真库确认原因——`source_key` 存的是 NULL，而 `WHERE source_key = ''` 对 NULL 行**匹配 0 行**，所以数据库给出的答案完全一样。那个提前返回是省一次往返 + 表达意图，不是正确性要求。真正的失效形态是「source_key 写成空串而不是 NULL」，那一条**被捕获了**。与 0807「单独去掉 resolveExpectedVersion 的提前检查不构成缺陷」是同一课：**探针要照着真实的失效形态造，不是照着代码行造**。

0810 Web 治理面那 24 条（**第一轮有 1 条漏网，已补测**）：认证与授权 4 条（管理页忘挂中间件、requireAdmin 退化成 requireUser、强制改密形同虚设、会话无效时放行）；CSRF 与凭据 4 条（写操作不校 CSRF、CSRF 用 == 比较、cookie 丢掉 HttpOnly、登录失败页说清具体原因）；越权 4 条（吊销别人的 token、管理员停用自己、token 明文每次都展示、初始密码用可预测随机源）；模板与响应头 2 条（换成 text/template、CSP 放开 unsafe-inline）；领域层 7 条；密码哈希 3 条。

**那条漏网值得记**：探针把 `ResumeSession` 里的 `!user.IsActive()` 检查删掉，`TestResumeSessionRereadsUser` 照样绿——因为那条测试是用 `SetUserStatus` 停用的，而它顺手删掉了会话，于是失败发生在「找不到会话」那一步，回查用户行那道检查根本没被走到。**测试通过是靠另一条路径**。补了 `TestResumeSessionRereadsUserEvenIfSessionSurvives`：直接改库里的行、绕过 `SetUserStatus`，模拟「会话还在但人已停用」。这正是探针存在的意义——它验的不是「功能对不对」，是「测试到底在测什么」。

0810 MCP 工具那 14 条（漏网 0）：**全部打在 Host 层**，因为这一层最容易写出"看起来对但没防住"的代码——权限解析漏一步，领域层还会兜住，测试照样全绿，而线上真正拦住越权的那一步已经没了。清单：写入用可读集合定位、写目标不校权限、检索对不可读空间静默跳过、空间排序权重在 Host 里另写一份、promote 只校目标不校源、检索结果不标注来源空间、丢掉 semantic 标记、召回不回传触发条件、版本冲突不带当前版本、未映射错误原文外泄、ErrNotFound 说成"无权限"、空标签编码成 null、新工具带上身份参数、Host 替领域层补卡片默认状态。

探针的两条纪律（都是踩出来的）：**先 `go build` 确认注入后能编译**（编译失败的 `go test` 非零是假阳性）；**确认 perl 表达式真的匹配到了东西**（没改动的文件当然测试全绿，那也是假阳性）。第 21 条探针还顺带发现：单独去掉 `resolveExpectedVersion` 的提前检查**不构成缺陷**——存储层的版本条件仍会拦下来，行为完全等价。真正的静默覆盖形态是「忽略调用方传来的版本号」，探针要照着那个形态造。

### 0001 迁移已验证的行为

| 验证项 | 结果 |
|---|---|
| 6 条 DDL 在 MySQL 8.0.36 上执行 | 通过，215ms |
| 重复执行 | no-op，不报错 |
| 篡改已应用的迁移文件 | 拒绝执行，报出两边校验和 |
| 21 个 CHAR 列的排序规则 | 全部 `ascii_bin` |
| 每用户一个 personal / 每租户一个 team | 数据库层拦截，且不误杀多个 project |

### 0002 迁移已验证的行为

| 验证项 | 结果 |
|---|---|
| 5 条 DDL 在 MySQL 8.0.36 上执行 | 通过，835ms |
| 中文全文检索真的分词 | 通过。插真数据后 `MATCH ... AGAINST('连接池')` 命中——`WITH PARSER ngram` 生效 |
| FULLTEXT 缺 `WITH PARSER ngram` | schema 规则 7 拦截，两处索引都报错 |

**中文分词那条必须真提交才能验**：InnoDB 的 FULLTEXT 索引在事务提交时才更新，同事务内 `MATCH` 查不到自己刚插入的行。`TestChineseFullTextSearchTokenizes` 因此自带清理，不走回滚。

### 0003 迁移已验证的行为

| 验证项 | 结果 |
|---|---|
| 2 条 DDL 在 MySQL 8.0.36 上执行 | 通过，1.44s（含一次 `ALTER TABLE` 建全文索引）|
| `embeddings.vector` 可空 | 通过。它同时是索引队列，NOT NULL 会逼我们用空 BLOB 冒充「还没算」|
| `uk_emb` 是三列唯一键 | 通过。同一实体在同一模型下只有一行，换模型是新增行 |
| 三张表的 FULLTEXT 都带 ngram | 通过。**读的是 `SHOW CREATE TABLE`**，见下 |
| 向量 float32 小端往返 | 通过。用例含负数、0、最大 float32——正数往往编码错了也看着对 |

**ngram 那条检查第一版写错过**：本来查 `information_schema.STATISTICS.COMMENT`，8.0.36 上那一列是空串，于是三张本来正确的表全被报成「没用 ngram」。解析器名只出现在 `SHOW CREATE TABLE` 的 ``/*!50100 WITH PARSER `ngram` */`` 注释里，那是权威来源。**假阳性和假阴性一样坏**——一条会误报的检查，第二次被误报时就会有人去关掉它。

---

## 治理域已实现的规则（动 knowledge 域之前先读这段）

**权限规则只有一份，在 `governance.ResolveScope`。** SQL 层不做任何权限判断——`ListSpaceMemberships` 只负责「捞出与这个用户有关的空间行」，判断全在领域层。

三类空间的规则形态**不同**（这点上一版文档写错过）：

| 类型 | 读 | 写 |
|---|---|---|
| `personal` | 仅 owner，隐含 | 仅 owner，隐含；**不可授权他人**，要共享走 `promote` |
| `team` | 全租户，隐含 | 需显式授权 |
| `project` | 需显式授权 | 需显式授权 |

`project` 的 owner 视为拥有读写且**不写授权行**——创建者被自己建的空间锁在门外是没有恢复路径的故障，这条不该依赖一行可被误删的数据。

其他已固化的决定，改之前先想清楚为什么：

- **未知空间类型一律无权限**。默认放行会让一个拼写错误变成越权。
- **`ResolveReadTargets` 对不可读空间报错，不静默跳过**。静默过滤会让 agent 以为搜过了那个空间并得到「没有结果」——检索为空和无权检索是完全不同的两件事。
- **不存在与无权限返回同一个错误**。区分二者等于泄露空间是否存在。
- **`last_used_at` 有 5 分钟写入节流，且写失败不影响认证**。它是运维辅助信号，不是授权依据。
- **`checkAuthConsistency` 在正常情况下永远不触发**，它防的是数据损坏导致的跨租户越权。有测试专门盯着它还活着，别当成冗余代码删掉。

**密码哈希欠账已还清**：领域层仍不读取 argon2id 配置；`ports.PasswordHasher` 由 `adapters/crypto` 实现，`governance.Service` 统一调用。Web 登录、成员创建、重置密码和参数升级均走同一实现。

---

## Store 实现里已固化的决定（改 SQL 之前先读这段）

`internal/adapters/mysql/governance.go`，手写 `database/sql`，无 ORM。**SQL 只取行写行，不做任何权限或状态判断**——文件里看不到 `WHERE revoked_at IS NULL`（认证路径上）也看不到 `AND can_read = 1`，那些判断全在领域层，只此一份。

| 决定 | 为什么 |
|---|---|
| `FindAuthContext` 一条 LEFT JOIN 取回四张表 | 热路径。拆成四次 Find 就是四次往返 × 每次工具调用 |
| `FindAuthContext` 使用 `AuthToken/AuthUser/AuthAgent/AuthTenant` 专用投影 | token hash 只做索引谓词；认证结果结构不含 token/password hash，也不捎带名称、前缀、创建时间等决策无关字段 |
| JOIN 用 **LEFT** 而非 INNER | 库里没外键。INNER 会把「指向已删用户的 token」伪装成「token 不存在」，一次数据损坏被藏成一次普通认证失败 |
| JOIN 的 ON 只比主键，**不带 tenant 相等条件** | 加了就等于让 SQL 悄悄过滤掉跨租户的坏数据，而 `checkAuthConsistency` 正是为发现它才存在的 |
| `ListSpaceMemberships` 的 WHERE 只看 type / owner / 有无授权行 | 这是**相关性**筛选。绝不看 `can_read` 取值——那一刻权限规则就有了第二个副本。`WHERE s.tenant_id = ?` 一个都不能少，漏了就是跨租户泄露（team 在领域规则里全租户隐含可读） |
| `TouchToken` / `RevokeToken` 在 0 行时补一次存在性查询 | 驱动未开 `clientFoundRows`，**写同值时 affected rows 是 0**。直接判成「不存在」会在节流窗口边界上造出假的 `ErrTokenNotFound` |
| `RevokeToken` 带 `AND revoked_at IS NULL` | 这里它是幂等机制不是状态判断：保证重复吊销不覆盖第一次的时间戳（那是审计证据） |
| `GrantPermission` 用 upsert | 两个管理员同时改同一人权限时，SELECT-then-INSERT 会有一个报主键冲突 |
| `Atomically` 已在事务中时**复用当前事务** | MySQL 无真嵌套事务，用 SAVEPOINT 模拟会让「回滚」在内外层含义不同 |
| `Atomically` 里 fn 的错误**原样返回**不包装 | 领域层靠 `errors.Is` 认哨兵错误 |

**这套测试反向验证过**：注入了 8 条违规（INNER JOIN、漏 owner 条件、漏租户隔离、覆盖吊销时间、去掉 upsert、0 行当不存在、不提交、包装错误），全部被捕获。第一轮时「漏租户隔离」曾经漏网——因为 fixture 里只有一个租户，后来补了 foreign tenant 的 team 空间与同名用户才抓住。**新加 SQL 时照做一遍。**

## MCP Host 协议层已固化的决定（认证、传输、错误映射）

| 决定 | 为什么 |
|---|---|
| `verifyToken` 失败时返回 **`auth.ErrInvalidToken` 本身，一个字都不加** | **SDK 会把这里返回的错误原文写进 401 响应体**（`auth.verify`：`return nil, err.Error(), 401`）。包一层 `%w` 带上原因，就等于把「已吊销」「已过期」直接告诉调用方。这个坑真的踩过一次，被探针抓出来的 |
| 不给 SDK 填 `Expiration`，改开 `AllowMissingExpiration` | 过期判断只能有一份。它在 `governance.Token.Validate`，用可注入的 `platform.Clock`；交给 SDK 就有了两个判断者、两个时钟源，测试注入假时钟时立刻打架 |
| 身份走 `TokenInfo.Extra` 整体传递 | 拆成几个字符串字段再重组会丢类型，PoC 里那段 `asString/asStrings` 样板就是这么来的 |
| context key 用未导出的空结构体类型 | 外部包无法伪造身份键 |
| 所有请求共用一个 `*sdk.Server` | 工具集与身份无关（agent 继承所有者的全部权限），按请求重建纯属浪费 |
| 错误映射只在 `errors.go` 的 `toolError` 一处 | 散落的映射表会以两种方式坏掉：新工具忘了映射导致内部错误原文泄露，或两处对同一错误给出不同的码让 agent 分支失效 |
| 未识别的错误一律吞掉原文 | SQL 错误含表名列名，超时错误含内网地址 |
| 401 **不带** `WWW-Authenticate` | 已知且接受的偏差。SDK 只在配了 `ResourceMetadataURL`/`Scopes` 时才发，我们不走 OAuth 发现流程。有测试锁住现状，将来接 OAuth 时它会自动出现 |

**这套测试反向验证过**：注入 7 条违规（身份从 header 读、认证失败带原因、只对已吊销换措辞、内部错误原文外泄、少返回 `defaultSpaceId`、Host 自行判断权限、关掉 Stateless），全部被捕获。**探针脚本必须先 `go build` 确认能编译**——编译失败也会让 `go test` 非零，那是假阳性，证明不了测试抓住了什么（第一版探针就栽在这上面）。

## 核心域已固化的决定（动 knowledge 之前先读这段）

**`knowledge` 不 import `governance`**（由 `test/arch_test.go` 强制）。权限规则只有一份，在 `governance.ResolveScope`；应用层把它解析成确定的空间 ID 集合后传进来，knowledge 只回答「在这批空间里，这条内容是什么」。

这条不是形式上的解耦。一旦 knowledge 能看见 `Permission`，「顺手在这儿也判断一下 can_write」就会变成第二份权限规则，而两份规则永远只会改对其中一份。

**编排模式**（将来的 MCP 工具照这个写）：

```go
scope, _ := gov.Scope(ctx, identity)              // governance 解析权限
space, err := scope.ResolveWriteTarget(req.Space) // 写：解析出确定的目标空间
mem, err := know.CreateMemory(ctx, knowledge.CreateMemoryInput{
    TenantID: identity.TenantID, SpaceID: space.ID, ...,
})
// 读：scope.ResolveReadTargets(req.Spaces) → 传给 knowledge 的 spaces 参数
```

| 决定 | 为什么 |
|---|---|
| 写操作用**可写**空间集合定位，不是"可读定位 + 再判可写" | 对一条自己改不了的内容，「不存在」与「无权修改」给调用方的下一步是同一件事。分开返回等于确认「它存在，只是你够不着」 |
| 建/删关联要求两端**可读**（不是可写） | 关联是一句关于世界的陈述，不修改任何一端。但两端必须看得见——否则可以用猜来的 ID 把自己的记忆挂上去，再通过 `ListLinks` 确认那条内容存在 |
| `ExpectedVersion` 为 nil 时用**刚读到的版本号**作条件 | 不存在"跳过版本检查"的路径。传了就是端到端保护，没传也只是把冲突检测的窗口缩到本次读-改-写之间——绝不静默覆盖 |
| Update 的字段用 `*string` | `nil` = 不变，`""` = 清空。用 `string` 就无法表达"把这栏清空"，而那是合法意图 |
| `RecallTriggers` / `Labels` 是**整体替换** | 增量语义会逼每个调用点先查现状再算差集。**但 `nil` 必须表示"没传"**——把 nil 当空列表，一次改标题就会清掉整张卡的召回能力（有测试专门盯这条） |
| promote 副本的作者是**执行者**，不是原作者 | 「这条来自谁的哪次踩坑」由 `copied_from` 回答；「谁决定把它摆到团队面前」是一份新责任 |
| promote 副本一律 `active`，不继承源状态 | 把一条 archived 的记忆搬过去再让它继续隐身，等于这次动作什么也没发生 |
| promote 拒绝同空间 | 会得到两条内容一样、只差一个 `copied_from` 的记录，检索时并排出现而无从判断信哪条 |
| `Card.IsQualified()` 是**度量不是门槛** | 「理解」栏为空的卡只是摘要，但摄取来的存量文档天然没有这一栏，而摄取是第一版核心能力。它的用途是让"还有多少张卡只是摘要"成为可以催的数字 |
| Store 收到空 spaces 集合返回 `ErrNotFound` | `IN ()` 是 SQL 语法错误。领域层已拦一道（报 `ErrInvalidArgument`，因为"你没有任何可访问空间"和"找不到"排查方向不同），Store 再兜一次 |
| 空 section 写 NULL 而非空串 | 让 `WHERE body_understanding IS NOT NULL` 有确定口径，否则"有多少张卡只是摘要"会因写入路径不同给出两个数字 |

**这套测试反向验证过**：领域层注入 14 条违规（空间过滤失效、nil triggers 当空列表、promote 不快照召回条件 / 沿用原作者 / 继承源状态 / 不拦同空间、关联只校一端、忽略传入版本号、事务不回滚、状态不校验 ×2、触发条件不去重、标签不排序、自指放行），Store 注入 7 条（空间过滤失效、租户过滤失效、0 行不分冲突与不存在、Find 不填标签、ReplaceLabels 退化成追加、SQL 去掉版本条件、事务失败仍提交），**全部被捕获**。

---

## 检索层已固化的决定（2026-08-10，动 search / embedding 之前先读这段）

### 索引是异步的，写入路径一次外部调用都不做

```
写入事务
 ├─ INSERT memories / cards / card_recall_triggers
 ├─ INSERT labels
 └─ INSERT embeddings (status=pending, vector=NULL, source_text=已截断的待算文本)
         ↓  后台 worker，默认每 5 秒一轮，一轮抽干为止
   取 pending → 调 Embedder → UPDATE vector, status=ready
                              失败 → status=failed, attempts+1，超过 5 次不再重试
```

| 决定 | 为什么 |
|---|---|
| 入队与内容**同事务** | 分两步的话，中间崩一次这条内容就永远搜不到，而且没有任何地方会报错。有探针盯着这条 |
| 写入路径**不调** Embedder | Ollama 挂了照样要能记东西。代价是刚写完的内容语义命中会滞后一个 tick，但 ngram 全文粗筛立刻就能命中——降级是平滑的 |
| `embeddings` 表**同时是队列** | 单独建 jobs 表的话，「这条内容有没有向量」要查两个地方，而两个地方迟早不一致 |
| 存 `source_text` | worker 因此是这张表的纯函数：不必按 entity_type 回去 load 实体再重拼文本（多态分发写三遍），也不会出现「入队和索引之间实体被改了，算的到底是哪个版本」 |
| 文本按 rune 截到 2000 | 模型本身有上下文上限，超出部分是被丢弃不是被平均。与其让模型静默截断，不如显式截断并存证。按字节截会切碎汉字 |
| `content_hash` 未变则不重置向量 | 只改标签不该触发重算——那是真金白银的外部调用。判断在 Store 的 upsert 里**只有一份**，领域层不再算一次 |
| 多实例重复领取 | 已用 Redis token+TTL 短租约在 worker 外协调，不持有跨 HTTP 的数据库事务；释放时校验 token。Redis 未配置或故障时 fail-open，写回仍按 ID 幂等，最坏代价是临时重复一次外部调用 |

### 检索：语义与全文**不通约**

| 决定 | 为什么 |
|---|---|
| 结果分**两组**：语义命中在前，全文命中在后，每条标 `Semantic` | 余弦和 ngram 相关性不是一个量纲，加权融合出来的排序是假的。调用方看得出区别，才能判断「这条是真相关还是只是字面撞上了」 |
| `SemanticAvailable` 必须回传 | 「没搜到」和「语义那一半根本没跑」长得一样，下一步却完全不同。与「检索为空 ≠ 无权检索」同一哲学 |
| 算查询向量失败时**降级不报错** | embedding 服务挂掉应该让检索变差，不该让检索停摆——第一段本来就是完整可用的路径 |
| `MATCH` 只进 SELECT 排序，**不进 WHERE** | 进了 WHERE，候选集就只剩字面命中的行，第二段的向量再准也没用。唯一的例外是 `CountCardMatches`——那个数字回答「另有多少条命中」，把没命中的数进去就是骗人 |
| 空间只做 tie-break，且先量化到 epsilon 网格再比 | 「差值小于 epsilon 就算相等」不满足传递性（a≈b、b≈c 但 a<c），排序结果会依赖初始顺序。量化成网格才是一个真正的全序 |
| `SearchSpace` 只带 `ID` + `Rank` | knowledge 不认识 team/project/personal。跨过域边界的只是一个整数——一旦这里出现 `if space.Type == "team"`，权限语义就有了第二个副本 |
| 检索只返回默认可见状态 | 不给 `includeStatuses` 参数：那等于让每个调用点都有机会把摄取产生的未审草稿混进 agent 的上下文。审 draft 是 Web 治理面的列表查询，不是检索 |
| `card_recall` 折叠到卡片**之后**才截断 | 否则一张有三条触发条件的卡会占满 limit，把别的卡整个挤出去 |
| `card_recall` 粗筛**必须 JOIN cards** | 触发条件表上没有 status 列；而它那个 `space_id` 是冗余列，拿它当权限依据等于信任一份可能不同步的副本。0002 里「冗余 space_id 使粗筛无需 JOIN」那句话只对纯权限过滤成立，当时没想到状态过滤 |
| 标签是**交集**（`HAVING COUNT(*) = n`） | 并集可以靠调用两次得到，交集不行。「缩小范围」是这个参数唯一的用途 |
| Embedder 错误不得带底层 `error` | Go 的 http 错误默认带完整 URL（内网地址与端口），而这串文本会写进 `last_error` 列并显示在 Web 治理面上。有探针盯着 |

### 对 `02-data-model.md` §5.1 的一处更正：没有 goroutine 池

设计文档写的是「goroutine 池并行计算余弦」。落地时**没有照做**，理由是它误诊了 Desktop 的病：那边真正的故障是 **JS 单线程事件循环被一次大搜索独占，所有其他 agent 一起阻塞**，而 Go 里每个请求本来就跑在自己的 goroutine 上，这个病由 runtime 免费治好了。

一次检索内部的计算量是 500 候选 × 千余维 ≈ 五十万次乘加，不到 1ms；再开一个池只是给它加调度和内存屏障的开销。这一段真正的成本在 `LoadVectors`——从 MySQL 拉几百条 BLOB 并解码，那是 IO。将来要优化该动的是候选集大小和向量缓存，不是并行度。

### 已知的规模上限（不是参数问题）

粗筛把 `MATCH` 放在 SELECT 列表而不是 WHERE，意味着每次检索要为**空间内的全部可见行**算一次相关性。万级规模完全没问题，但这是这套检索第一个会撞墙的地方。

真撞上时该做的是把粗筛拆成「全文索引命中集 ∪ 最近内容」两条索引路径求并，**不是调 `candidateCap`**。MySQL 8.0 没有 ANN 索引——「向量相关但字面不沾边、又不新近」的内容进不了候选集是结构性的。

---

## MCP 工具层已固化的决定（2026-08-10，加新工具之前先读这段）

### 每个 handler 的形状必须一样

```go
r, err := h.begin(ctx)                 // 身份 + 权限，唯一入口
space, err := r.writeTarget(in.Space)  // 写：解析成一个确定的空间
spaces, err := r.readTargets(in.Spaces) // 读：解析成带权重的空间集合
result, err := h.know.Xxx(ctx, ...)    // 领域层做事
return nil, r.encodeXxx(result), nil   // 编码
```

中间**不出现任何 if 业务判断**。`begin` 抽成一个函数不只是省几行——它保证每个工具都走了同一条权限解析路径。少写一次 `Scope` 调用不会有编译错误，只会有一个不带权限判断的工具。

| 决定 | 为什么 |
|---|---|
| 写操作用**可写**集合定位（`writableSpaces`） | 不是「可读定位 + 再判可写」。对一条自己改不了的内容，「不存在」与「无权修改」给调用方的下一步是同一件事，分开返回等于确认「它存在，只是你够不着」 |
| 关联与 promote 的源用**可读**集合 | 建关联不修改任何一端，promote 也只是复制。但两端必须看得见——否则可以用猜来的 ID 反推内容是否存在 |
| 空间排序权重来自 `governance.Space.SearchRank()` | 定义只有那一份，`sortAccesses` 也用它。在 Host 里写 `if type == "team"` 就是第二份，而两份规则永远只会改对其中一份。注意方向：SearchRank **大者优先**，而排序里是降序 |
| Host **不补**领域层的默认值 | `card_create` 的 `status` 留空传下去，由 `CreateCard` 填 `active`。「手写默认 active、摄取显式 draft」是业务规则，在 Host 补一次就是复制了它 |
| 结果里每条内容都带 `space{id,type,name}` | agent 据此判断权威性：team = 已达成共识，personal = 个人经验。`knowledge` 只返回 `space_id`，映射成三元组是 Host 用治理域信息做的 |
| `semantic`（每条）与 `semanticAvailable`（整次）必须回传 | 语义命中与全文命中不通约；而「没搜到」和「语义那一半根本没跑」下一步完全不同 |
| 关联**不解析对端标题** | 对端可能落在调用方无权访问的空间里。只给 ID 和 kind，想看内容就拿 ID 去 get——那里会撞上正常的权限检查 |
| 空标签编码成 `[]` 不是 `null` | agent 侧 `for x in labels` 撞上 null 会炸，而「没有标签」是完全正常的状态 |
| 更新类工具的字段全用指针 | `nil` = 不改，`""` = 清空。用值类型就没法表达「把正文清空」这个合法意图 |

### 契约的一处偏差：VERSION_CONFLICT 的形态

`03-mcp-contract.md` §3.4 描述的是一个带 `currentVersion` 字段的 JSON 对象。SDK 的错误通道只能传字符串（tool 返回 `error` → `isError` + 文本），所以实际形态是一句格式稳定的话：

```
VERSION_CONFLICT: 内容已被他人修改，当前版本 7。请重新 get 后基于最新内容再改
```

版本号是真的要带上的，不是装饰：只说「撞车了」的话，agent 还得再发一次 get 才能重试，而那次 get 之后可能再撞一次——重试就不收敛。为此在领域层加了 `knowledge.VersionConflictError`（`errors.Is(err, ErrVersionConflict)` 仍然成立），MySQL 那边把本来就要发的存在性查询顺手多取了一列 `version`。

### 身份参数：由 SDK 的 schema 校验兜底

塞 `authorUserId` / `tenantId` 进 arguments 会被 SDK 直接拒掉（`additionalProperties`），连 handler 都进不去。`TestToolSchemasHaveNoIdentityParams` 扫全部工具的 inputSchema，盯的是**未来新增的工具**——谁哪天顺手加一个 `userId` 参数，那条会红。

### 工具清单（24 个，上限 24）

| 组 | 工具 |
|---|---|
| Memory 6 | `memoria_create` / `search` / `get` / `update` / `delete` / `link` |
| Card 5 | `card_create` / `recall` / `search` / `get` / `update` |
| 空间 3 | `space_list` / `promote` / `space_stats` |
| Continuity 5 | `shared_line_list` / `get` / `update` / `handoff` / `status` |
| 系统与上下文 5 | `whoami` / `server_status` / `gateway_auto_context` / `gateway_context_feedback` / `context_quality_status` |

Continuity 只保存可续接的当前位置、事实 ID 与里程碑交接，不复制 Memory / Card 正文，也不虚构 `space_id`；共享边界是 tenant。用户已在 2026-08-27 确认 Server 版保留这条线能力。

**维护类操作一律不进 MCP**（导入导出、合并建议、休眠归档、标签别名管理）：它们是人的运维动作，归 Web 治理面。有测试盯着工具总数不超过 24；Shared Line 生命周期和上下文反馈属于高频运行契约，因此保留在 MCP。

### 这一层缺的东西

- **Memory supersede 已实现，但入口在 `memoria_create.supersedes`**。创建当前事实、旧事实置 `superseded` 与建立 `supersedes` 链在同一事务；`memoria_search.timeView` 读取 current / historical / all。
- **Card evolution 已实现**。`card_create.evolvesFrom` 原子创建新卡、把旧卡置 `deprecated` 并建立 `evolved-from` 链。
- **`space_stats` 的合格判定有两份表达**。SQL 是 `body_understanding IS NULL`，领域是 `Card.IsQualified()`。等价靠的是 0002「空 section 写 NULL 而非空串」加写入侧的 `nullIfEmpty`。`TestSpaceStatsUnqualifiedMatchesIsQualified` 把两者写在了一起，但它挡不住「给 IsQualified 加一条判据却忘了改 SQL」。

---

## 摄取已固化的决定（2026-08-10，动 ingest 之前先读这段）

### 服务端不提炼

摄取只把文档变成一张**带 source 的 draft 卡**：标题 + 原文，别的都不填。
提炼（写「理解」与召回条件）交给 agent 走 `card_update`。

摄取链路不引入 LLM 提炼，是因为自动提炼质量不可控——一旦跑偏就是批量产出垃圾召回条件，
而垃圾召回条件比没有召回条件更糟：它会污染 `card_recall` 这个产品的核心能力。

自动上下文使用确定性最低分与差值门槛，只读选择最多一个 Memory 或 Card；两域同时高置信也会安全放弃。调用方明确 `lineId` 时可精确带入 active Shared Line，系统不猜测工作线。终端 Agent 的模型负责最终相关性判断。它不生成或修改内容，也不改变摄取的人工审核边界。

代价必须说清楚：**刚摄完那批卡，`card_recall` 一条都命不中**（没有触发条件）。
`card_search` 能找到它们（正文进了向量），但那不是这个产品的主路径。
摄取产出的是原料，不是卡片。

### 幂等靠数据库，不靠应用层先查一次

```
库里没有这个 source_key   → 新建一张 draft
有，但 content_hash 变了   → 更新标题与原文，其余不动
有，且 content_hash 没变   → 什么都不做
```

| 决定 | 为什么 |
|---|---|
| `uk_cards_source (space_id, source_key)` | 应用层 SELECT-then-INSERT 在并发下必然失效（0002 建 spaces 生成列时已记过同一条）。而摄取天生是批量且会被反复触发的 |
| `source_key` 为 NULL 而非空串 | MySQL 唯一索引允许多行 NULL。于是一个约束同时做到两件事：摄取产物被约束，手写卡片完全不受影响。与 0001 用生成列做「每用户一个 personal」是同一手法 |
| 第三分支「什么都不做」 | 没有它，重扫一个没改过的目录会把每张卡 version 推高一格、每个向量作废重算——真金白银的外部调用，而且版本历史会被无意义的自增淹掉 |
| 更新只覆盖标题与原文 | 提炼、理解、召回条件、标签**一律不动**。源文档改个错别字就清空它们，等于每次上游更新都惩罚一次做过功的人。代价是原文与提炼可能不同步——但不同步的提炼还能被看见并修正，被清空的谁也想不起来 |
| 摄取**不碰 status** | 一张被人改成 deprecated 的卡，重扫时仍被认出是同一份源，且状态保持。`FindCardBySourceKey` 因此**不做状态过滤**——过滤了就会新建一张，而「已经判过这份文档没用」的决定被悄悄推翻 |
| 一份坏文档不中断整批 | 保留成功项 + 列失败清单（`04` §6）。失败项必须带**源路径与原因**——只说「失败 3 条」等于没说 |
| 同批重复 SourceKey 直接拒 | 它们会在唯一键上互相覆盖，最终留下哪一份取决于遍历顺序。表现是「每次摄取结果都不一样」，极难排查 |

### 源的身份是**声明的名字**，不是它在磁盘上的位置

`SourceKey = SHA-256("markdown:" + label + "\x00" + 相对路径)`。

第一版我把 `root`（绝对路径）掺了进去，`TestSourceKeyIsStableAcrossRoots`
**当场抓住**——那意味着一次目录搬迁或换一台服务器，库里的卡会全部重建一遍。
`label` 同时解决另一件事：两个不同目录摄进同一个空间时，两边的
`docs/x.md` 不会撞成同一张卡。

改 `-ingest-source` 的值 = 换了一个源，会重新建一整套卡。这不是 bug，
是定义——但用的人得知道。

### 这一层缺的东西

- 删除检测和 Web 摄取管理页已经实现；扫描不完整时 fail closed，只报告缺失，不自动改卡。
- **只有 Markdown 目录一个源**。odoc 那类活系统按 `01` §8.1.3 是「连接而非纳入」，
  属于第二版的索引同步，路径完全不同。
- **整篇一张卡，不按 `##` 切分**。按章节拆是一种提炼判断，而提炼不归摄取管——
  但一份 500 行的设计文档变成一张卡，对召回未必是最优粒度。这条要等真实
  召回数据才能判断。

---

## Web 治理面已固化的决定（2026-08-10，动 internal/web 之前先读这段）

### 路由是三层门，不是三个 if

```
public              登录页、静态资源
requireUser         有效会话（写操作顺带校 CSRF）
requirePasswordSet  ↑ 且初始密码已改过
requireAdmin        ↑ 且角色是 admin
```

在每个 handler 里各判一次的话，**少判一次不会有编译错误，只会有一扇安静打开的门**。`TestUnauthenticatedIsRedirectedEverywhere` 与 `TestWritesRequireCSRF` 遍历全部路径，盯的正是「新加的路由忘了挂中间件」。

改密页只挂 `requireUser`，**不能**挂 `requirePasswordSet`——否则被强制改密的人会被自己的中间件挡在改密页外面，那是一个谁也解不开的死锁。

| 决定 | 为什么 |
|---|---|
| 会话**存库**，不用签名 cookie | 停用一个员工时，他浏览器里那张已签发的票必须当场失效。JWT 那一路做不到，除非再加一张吊销名单——那时它已经是一个更复杂的会话存储了 |
| `ResumeSession` **每次回查用户行** | 角色和状态不缓存进会话。冻在票里等于「停用/降级要等他下次登录才生效」，那是治理面最不能接受的延迟 |
| 会话表存令牌的 **SHA-256** | 与 tokens 表同一条纪律：数据库被读走时拿到的是一堆无法用于登录的哈希 |
| CSRF token 由会话令牌 **HMAC 派生**，不额外存储 | 无状态：不必为每个表单在库里多写一行，也没有「token 过期了但会话还在」的中间态。攻击者要伪造就得先拿到 HttpOnly 的 cookie |
| 登录表单**不**校 CSRF | 那时还没有会话，也就没有可派生的 token。「登录 CSRF」由 SameSite=Lax 挡住 |
| 登出**要**校 CSRF | 被跨站强制登出不致命，但是实打实的骚扰，而校验成本是一行 |
| 登录失败只有一句话 | 区分「账号不存在 / 密码错 / 已停用」等于提供账号枚举接口。详细原因进日志 |
| 普通成员访问管理页得到 **404 不是 403** | 不确认「这个管理页存在」。与领域层「不存在与无权访问返回同一个错误」同一条哲学 |
| 初始密码由**服务端生成** | 管理员自己想的那个大概率是 `Team2026!`，而它会被同一批人共用。字母表去掉了 0/O、1/l/I——这串密码要被人从屏幕上读出来再手输一次 |
| 管理员**不能停用自己** | 无自助注册、没有第二个入口，把自己关在门外之后没有恢复路径。这不是业务规则，是一道操作护栏 |
| 吊销 token 前先查归属 | `RevokeToken` 只按 tenant 过滤，所以「这个 token 是不是我的」必须由 Host 检查——否则拿到一个 ID 就能把同事的 agent 停掉 |
| `SecureCookie` 是配置不是常量 | 本地开发走 http 时置真会让 cookie 根本存不下来，症状是「登录成功但立刻跳回登录页」，没人第一次能猜对。默认值跟着 `CCS_ENV` 走 |
| `PublicMCPURL` 必须可配 | 服务监听 `127.0.0.1:51890`，用户要填的是 nginx 后面那个地址。拿监听地址拼一个出来，等于给每个人发一份连不上的配置 |

### 密码：哈希在 adapter，参数在 config

`ports.PasswordHasher` 的 `Verify` **和 `Hash` 在同一个接口里**，不是让调用方自己比对：① 常数时间比较只该有一处实现；② 哈希串里编码着当时的参数，「用 hash 里记的参数去验」与「用当前配置去算」必须写在一起，否则某次调参就会把所有存量用户静默锁在门外（`TestVerifyUsesEmbeddedParams` 盯着这条）。

`CreateUserInput` 从 `PasswordHash` 改成了明文 `Password`（Service 内部哈希）。原设计让调用方自己哈希，结果是集成测试里塞了一堆 `$argon2id$e2e$fake` 这种永远验不过的假值，而**没有任何地方能真正创建一个能登录的账号**。哈希本身仍然不在领域层。

### 模板：每个页面一套，不是一个大集合

全部文件 `ParseFS` 进同一个 `Template` 时，各页面的 `{{define "content"}}` 会互相覆盖——最后解析的那个赢。**这个坑不报错**：解析成功、页面 200，只是六个页面渲染出的都是同一份内容（0810 落地时踩到，全渲染成了成员管理页）。

`render` 先渲染到内存再写出去：模板执行到一半才出错时，前半页已经进了 ResponseWriter，那时既发不了 500 也回不了头。

### 列表查询：为什么不复用检索

`knowledge.ListContents` 是新加的，**不是** `SearchCards` 加个参数。三条理由都不是小毛病：

| 检索 | 列表 |
|---|---|
| 只返回默认可见状态 | 治理面必须看得到 draft 与 deprecated——那正是它存在的理由 |
| `candidateCap` 截断 + 相关性排序 | 确定的时间序，要能一页页翻到底 |
| 走向量精算 | 每翻一页算一次余弦是纯浪费 |

硬加个 `includeStatuses` 参数来复用会同时毁掉两边：检索多一个能把未审草稿混进 agent 上下文的开关，列表还是翻不到底。

**分页用 keyset 不用 OFFSET**：`(updated_at, id)` 复合比较。翻页期间有内容被更新时 OFFSET 会漏行/重复行，而审核队列恰恰是一边翻一边改的场景。游标是不透明的 base64——做成 `?after_id=xxx` 那种可读形式，迟早有人手工拼一个，而那时排序键与游标不一致的后果是静默漏行。

**多取一条判断有没有下一页**，而不是「取满 limit 就假设还有」：后者会在正好取满时给出一个点进去是空的下一页。

### 审核页：Web 上唯一的内容写入场景

它与「让人在库里随手写」的区别是三条硬条件（01 §10 的唯一例外）：只针对 draft、发生在明确的摄取流程内、**有原文可对照**。少任何一条，这个口子就变成「Web 上也能创建内容」，那意味着两套写入路径。

| 决定 | 为什么 |
|---|---|
| 队列用**可写**空间集合 | 能写就能审（04 §6）。用可读集合的话人会看到一堆自己按不动的按钮，而权限判断推迟到提交时——那时他已经填了一屏字 |
| 「标记不需要」落成 `deprecated` | 不新增枚举值。draft 队列必须能被清空，否则几千条永远挂着，「还有 N 条待审」那个数字就失去意义 |
| 审核时能改标题与 cardType | 摄取给的标题是文件名或第一个 `#`，cardType 一律「摄取原料」。不让改的话审完的卡类型全是一个值，`card_search` 的过滤就废了 |
| 确认与退回走**同一个** `UpdateCard` | 「审核」不是独立的实体或流程，就是一次带写权限校验的更新 |
| 校验失败把人填的内容放回表单 | 丢掉一屏字，他下次就不敢再点那个按钮——而审核本来就是最容易被拖延的事 |
| 召回条件用一行一条的文本域 | 比一堆 `<input>` 好用得多，而审核时最常做的动作正是「加两三条触发条件」 |

### 活动流与完整审计（2026-08-11 已实现）

- `internal/mcp/activity.go` 是 24 个 MCP 工具的唯一注册/审计边界；每个工具必须声明读写分类与强类型安全提取器，新工具不能悄悄绕过活动记录。
- `requireUser` 是 Web 已认证写操作的统一审计边界；业务响应先完成，普通审计失败只记服务端错误，不把已经成功的写改成失败。管理员完整审计例外：meta-audit 写不下时**拒绝查询**，不能先看再补记。
- 普通 `/activity` 只把治理层已解析出的 team/project 可读空间 ID 交给 `activity.ListVisible`；MySQL 在 SQL 中只返回「本人全部 + 他人共享空间写入」。个人空间与他人读取/召回不会先进入应用内存。
- `/admin/audit` 与日常活动页分离：GET 只展示风险说明和确认，POST 先持久化 `admin.audit.query` 的 meta-audit，再读取租户完整日志；翻下一页也要重新确认并留痕。
- MCP 元数据白名单只含实体/空间 ID、命中 ID、计数与语义标志；Web 写审计不序列化表单。query、situation、标题、正文、密码、token 与错误文本都不进 `audit_logs`。
- `trace` 与 `governance` / `meta_audit` 共用一张表但分类独立：后台 worker 启动时立即清一次，之后每 24 小时按 1000 条一批抽干 30 天前的 `trace`；治理与 meta-audit 不进入这条删除路径。服务关闭会等待这一 worker 退出。

**当前最远证据**：MySQL 已是 8 个迁移的最新结构；本轮 `make check` 全绿（domain 85.1%），完整 `make itest` 通过，包含真实 MySQL、MCP 端到端、语义召回、活动筛选与共同线 keyset 分页；`192.168.3.200` 上由代码提交 `b70c3d2` 构建的 `dev-20260828-ui-completion-v1` 已由 Supervisor 保持 RUNNING，release 为 `claracore-server_dev-20260828-ui-completion-v1_linux_amd64`，发布包 SHA-256 为 `481dec0382947209d641578778e4d9d9cfec80de8c7c538f78df5d9187d19137`，上一 release `dev-20260828-ui-polish-v1` 保留用于回滚。部署前备份 `predeploy-20260828-ui-completion-v1.sql.gz` 已用 `--no-tablespaces` 生成并通过 gzip 完整性、19 张表结构与 19 组数据段检查。服务器内部与开发机绕过本地代理后的外部部署探针均验证 MySQL、Web 和 MCP 401 边界；`/proc` 已确认实际进程指向新 release 且 `CCS_ENV=production`。这一版补齐草稿审核并发保护、高风险操作确认、共同线分页、概览 Agent/近期卡片、内容空间与作者信息、列表返回上下文、活动/审计多维筛选和管理员系统状态页；真实 bge-m3 + 确定性 Stage B 集成门禁两组正负样本均通过，单次约 107–131ms。线上 `tools/list` 的 24 工具证据沿用上一轮，本轮没有改变 MCP 工具面。登录后的主观视觉与完整业务操作验收仍待用户确认；当前内网 HTTP 按维护者决定暂不升级，公网部署时再验 HTTPS。

**2026-08-29 本地迭代证据**：Redis 已从“配置占位”接入为 embedding 两级缓存的共享层；key 只含 `model + text` 的 SHA-256，值是版本可隔离的二进制向量，不缓存原文。跨实例命中、批量读写、TTL、损坏值重算、Redis 故障回退与系统页实时状态均有自动化测试；`make check` 全绿（domain 85.1%），完整 `make itest` 通过，真实 bge-m3 自动上下文正负门禁约 110–122ms。当前验证层是本地测试 + 真实开发 MySQL/embedding 集成，尚未提交、推送或部署到 `192.168.3.200`，因此上段部署版本仍是线上真相。

**2026-08-29 看板迭代**：概览新增“运行与质量”首屏，四组信号分别回答共同线是否需要接手、active 卡片是否具备召回条件/理解、语义索引是否积压或耗尽重试、近 30 天自动上下文反馈是否出现未纠正错误。共同线新增租户级精确统计查询；卡片统计新增 active/active-unqualified/recall-ready 口径，避免用草稿与历史状态粉饰质量；反馈继续标注为自选择样本而非准确率。局部统计失败只提示并降级该卡，不拖垮首页。`make check` 全绿（domain 85.1%），完整 `make itest` 通过，新增真实 MySQL 用例覆盖共同线生命周期/租户隔离与卡片 active 质量口径；既有 MCP、Shared Line、召回评估和自动上下文正负门禁仍全绿（最新约 107–123ms）。当前验证层仍是本地测试 + 真实开发 MySQL/embedding 集成，未做用户页面视觉验收、提交、推送或部署。

**2026-08-29 多域自动上下文迭代**：`gateway_auto_context` 已从 Card-only 扩为保守的 Memory / Card 单证据竞争；两域分别执行同一 score/margin 门禁，任一域内歧义或跨域双命中都 abstain。调用方明确 `lineId` 时精确交付 active Shared Line 的当前位置且不再运行语义竞争；不会隐式猜测工作线。Memory 只取 current 状态，证据正文有字符上限与截断标记，`selectedMemory` 不复制全文；`gateway_context_feedback` 新增 `domain + id` 并兼容旧 `cardId`。`make check` 全绿（domain 85.0%），完整 `make itest` 通过；真实 bge-m3 的 Card 正负门禁约 107–123ms，新增 Memory 正负门禁约 88–96ms。当前验证层是本地测试 + 真实开发 MySQL/embedding 集成，未提交、推送、部署，也未做用户页面视觉验收。

**2026-08-29 多实例索引协调迭代**：新增基础设施 `ports.Locker` 与 Redis token+TTL 实现，索引 worker 每批按 embedding model 取得短租约，TTL 覆盖两倍模型超时与写回余量；Lua 释放只删除自身 token，重复释放不会误删后继租约。没有把外部 embedding HTTP 调用包进数据库长事务。Redis 未配置或暂时故障时 worker 告警并 fail-open，向量写回仍按 ID 幂等，因此可用性不依赖 Redis，最坏只临时重复一次模型调用。系统页与部署文档已同步真实装配。`make check` 全绿（116 个文件架构检查、domain 85.0%），完整 `make itest` 通过；本轮真实 bge-m3 Card 门禁约 112–123ms，Memory 门禁约 85–87ms。当前验证层是本地测试 + 真实开发 MySQL/embedding 集成；双实例、Redis 故障注入和滚动发布尚未实机验收，亦未提交、推送或部署。

**2026-08-29 审核队列规模化迭代**：队列新增显式勾选后的批量“标记不需要”，没有批量发布。领域层每批限制 1–50 张，在单个事务中逐项验证 tenant、可写空间、draft 状态与乐观锁版本；任一项不存在、越权、已非 draft 或版本冲突时整批不落库。列表 SQL 新增 version 回传，Web 只提交 `ID:version`，成功后保留空间筛选并显示处理数量；动作有 CSRF、二次确认与独立 `card.review_batch_deprecate` 治理审计。选择框使用原生 checkbox、逐项可访问名称、40px 点击区域和既有可见焦点样式。真实 MySQL 用例已证明先更新一张后遇到非草稿会整批回滚。`make check` 全绿（116 个文件架构检查、domain 85.0%），完整 `make itest` 通过；本轮真实 bge-m3 Card 门禁约 106–116ms，Memory 门禁约 90–91ms。当前验证层是服务端渲染/领域单测 + 真实开发 MySQL/embedding 集成；未做用户拥有的 1440/390px 页面视觉与键盘/读屏验收，亦未提交、推送或部署。

**2026-08-29 自动上下文可观测性迭代**：每次 `gateway_auto_context` 的 `deliver_one` 现在生成 128-bit 随机十六进制 `contextId`，不含 prompt、正文、实体或 actor；选择 trace 与后续 `gateway_context_feedback` 治理事件只用该 ID 关联。旧客户端不回传 ID 仍可提交 outcome，但不计入覆盖。`context_quality_status` 与概览看板新增近 30 天 `selected`、`abstained` 和 `feedbackCoveredSelections`；覆盖只计算能匹配同租户、本窗口真实选择的去重 ID，重复反馈、随机 ID、跨租户、过期和失败事件均不能放大分子。outcome 仍明确标成主动提交样本，不解释为准确率。`make check` 全绿（116 个文件架构检查、domain 85.0%），完整 `make itest` 通过；真实 MySQL 聚合用例验证了租户、时间窗、成功状态、关联真实性和去重，真实 bge-m3 Card 门禁约 111–131ms、Memory 门禁约 92–96ms。当前验证层是协议/领域/Web 单测 + 真实开发 MySQL/embedding 集成；未做用户拥有的页面视觉验收，亦未提交、推送或部署。

**2026-08-29 双实例索引装配门禁**：`make itest` 现在显式包含 `cmd/server` 集成包。新增用例用两个独立 `knowledge.Service`、两个 Redis client 和同一真实 MySQL pending 队列启动两个完整 indexer worker：Redis 正常时先确认两边都尝试租约，再放行阻塞的 embedding，最终同一批只调用一次；Redis 注入 `LOADING` 故障时两边均 fail-open，同时计算并并发写回同一行，最终数据库仍收敛为一条 `ready`、无 pending/failed。部署文档同步区分了这条自动化证据与两台 Linux、真实网络分区、负载均衡和滚动发布验收。`make check` 全绿（117 个文件架构检查、domain 85.0%）；完整 `make itest` 全绿，新门禁分别约 0.15s / 0.50s，真实 bge-m3 Card 门禁约 104–124ms、Memory 门禁约 86–91ms。当前最远层是双 worker 进程内装配 + 独立 Redis client + 真实开发 MySQL；尚未做部署侧双实例或滚动发布，也未提交、推送或部署。

**2026-08-29 Web 登录防爆破迭代**：登录在 Argon2 校验前按“客户端 IP + 账号”和客户端 IP 两个维度计数，默认分别为 15 分钟 10 次与 60 次；达到上限返回通用 429 页面和 `Retry-After`，成功尝试也计数，避免攻击者把昂贵哈希当作匿名算力入口。账号与地址只参与 SHA-256 key，不写进 Redis；应用仅信任 loopback 直接上游设置的 `X-Real-IP`，非 loopback 伪造头无效。Redis 用 Lua 原子固定窗口跨实例共享；未配置或故障时退回最多 20,000 个 key 的有界进程内保护，配置校验不允许把保护设为 0。Redis 连接现在不再依赖 embedding 是否启用。单测覆盖窗口边界、容量耗尽恢复、Redis TTL/命名空间、可信代理、密码哈希前阻断、身份不泄露、429 重试时间和基础设施故障降级；`make check` 全绿（124 个文件架构检查、domain 85.0%），完整 `make itest` 全绿，真实 bge-m3 Card 门禁约 100–119ms、Memory 门禁约 88–101ms，既有双 worker 门禁约 0.16s / 0.50s。当前最远层是协议/适配器/Web 单测 + 真实开发 MySQL/embedding/双 worker 集成；nginx 模板和运维文档已更新，但尚未在目标服务器实测 429、多实例共享、代理地址或页面体验，亦未提交、推送或部署。

**2026-08-29 生产配置 fail-fast 迭代**：`internal/config` 不再把拼错的整数、布尔值或小数静默退回默认值；非空类型化值严格解析，持续时间乘法先检查溢出，未知的 `CCS_*` 变量名也会被当作潜在拼写错误拒绝。MySQL 端口/连接池、Redis 超时、Argon2 内存/迭代/并行度、embedding 超时/节奏/批次/缓存容量及自动上下文分数均限制在可运行范围。`.env` 的缺少等号、非法或重复变量名、未闭合引号和扫描错误会带行号阻止启动。`CCS_ENV=production` 现在强制 Secure Cookie 与完整 HTTPS `CCS_PUBLIC_MCP_URL`，开发环境仍允许 loopback HTTP。系统状态页同时纠正旧口径：明确展示登录保护预算；Redis 正常时区分“多实例基础协调已装配、部署侧仍待验收”，故障时显示各实例本地保护与幂等写回降级，未配置时不再声称已启用跨实例能力。`make check` 全绿（124 个文件、domain 85.0%），完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 117–133ms、Memory 门禁约 85–95ms，双 worker 门禁约 0.17s / 0.51s。当前最远层是严格配置/模板单测 + 真实开发 MySQL/embedding/双 worker 集成；尚未用目标服务器生产环境文件做启动前检查、刷新系统页或完成多实例实机验收，亦未提交、推送或部署。

**2026-08-29 可恢复备份工具链迭代**：发布包新增 `backup-mysql.sh`、`rehearse-mysql-restore.sh`、公共安全检查与 0600 client file 模板。备份使用 owner-only defaults file，密码不进参数/日志；同一目录的原子锁阻止任务重叠，`mysqldump --single-transaction` 流式压缩到隐藏临时文件，gzip、SHA-256 与 metadata 全部就绪后才把 `.sql.gz` 作为最后且不可覆盖的原子发布标记，失败不留伪成功产物。dump 不含 `--databases`；恢复端在任何 MySQL 调用前还会拒绝含 `USE` / `CREATE DATABASE` / `DROP DATABASE` 的 SQL，避免传入外部 dump 后绕过隔离目标。演练必须显式传 `--confirm-isolated-server`，只导入脚本本次随机创建的 `ccs_restore_*` 数据库，再逐表 `CHECK TABLE QUICK`、精确计数并记录 migration checksum，报告存在即拒绝覆盖。默认成功或失败都只清理本次创建的隔离库，`--keep-target` 可留给人工抽查。伪 MySQL/mysqldump 门禁覆盖凭据权限、参数顺序、秘密不泄露、并发锁、dump 失败清理、篡改/源库定向 SQL 建库前阻断、校验失败清理与报告内容；发布测试真实跨编译 Linux amd64 并检查归档内容/执行位。ShellCheck、`make check`（124 个文件、domain 85.0%）与完整 `make itest` 全绿，真实 bge-m3 Card 门禁约 117–124ms、Memory 门禁约 88–97ms，双 worker 门禁约 0.19s / 0.51s。当前主机没有 mysql/mysqldump CLI，因此尚未在真实 MySQL 隔离实例执行新脚本；这一层不能把伪工具测试或已有手工 gzip 检查解释成恢复验收。未提交、推送或部署。

**2026-08-29 流量 readiness 迭代**：普通服务启动现在会只读比较二进制内置迁移与真实 `schema_migrations`；缺少版本、已发布 checksum 漂移，或数据库含有旧二进制不认识的新版本都会在监听端口前 fail-fast，不再让不兼容实例接受 Web/MCP 请求。新增 `/livez` 与 `/readyz`：前者完全不访问外部依赖，避免 MySQL 短暂故障触发进程重启风暴；后者用 3 秒预算验证 MySQL 和 schema 精确一致，Redis/embedding 因已有降级语义不作为硬依赖。旧 `/healthz` 保留但收紧为 readiness 语义。部署脚本真实调用四个边界：live、ready、Web 登录页和 MCP 401；单元门禁覆盖依赖隔离、数据库失败跳过 schema、迁移不一致 503 与错误细节不泄露。`make check` 全绿（126 个文件、domain 85.0%），完整 `make itest` 全绿，真实开发 MySQL 证明迁移 `0001`–`0008` 的 checksum 与当前二进制一致；真实 bge-m3 Card 门禁约 116–125ms、Memory 门禁约 84–97ms，双 worker 门禁约 0.16s / 0.55s。另以临时本地监听实际启动完整服务，启动日志确认 `latest=0008 applied=8 expected=8`，三个探针均返回预期状态，部署验证脚本通过后服务已优雅退出。当前最远层是本地真实运行 + 真实开发 MySQL/Redis/embedding；尚未部署到目标服务器或完成负载均衡摘流/滚动发布验收。

**2026-08-29 发布预检与优雅摘流迭代**：新增 `claracore-server -check`，只加载严格配置、连接 MySQL 并只读核对 schema 后输出 machine-readable JSON；不会监听端口、连接可选 Redis/Embedding 或启动后台 worker，且拒绝与 migrate/bootstrap/ingest 混用。SIGTERM 路径新增原子 readiness gate：先把 `/readyz` 与兼容 `/healthz` 切成 `503 draining`，跳过依赖检查并返回覆盖摘流窗口的 `Retry-After`；新 Web/MCP 请求返回可重试 503，已经进入 handler 的请求继续，`/livez` 保持 200；等待 `CCS_SHUTDOWN_DRAIN_SEC` 后才关 listener。配置严格限制 0–60 秒，默认 5 秒，systemd/Supervisor 停止预算提高到 30 秒。单测覆盖预检 JSON 不含连接信息、draining 不碰依赖、业务请求拒绝、探针例外与配置边界；本地完整服务用真实开发 MySQL/Redis/Embedding 收到 SIGINT 后实测 `/livez=200`、`/readyz=503 draining`、`/browse=503`，10 秒后 listener 关闭并完成退出。`make check` 全绿（126 个文件、domain 85.0%），当前精确代码的完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 99–128ms、Memory 门禁约 83–84ms，双 worker 门禁约 0.17s / 0.54s。只读复核团队实例仍返回旧版 health JSON（登录 200、MCP 401 正常），说明这一批本地代码尚未部署；不能把本地摘流证据当成真实负载均衡滚动发布验收。

**2026-08-29 运行容量看板迭代**：管理员“系统状态”不再只显示依赖通不通。页面新增实例运行时间、SIGTERM 摘流策略、实时 schema 一致性，以及 `database/sql` 连接池的当前使用/上限、打开、空闲、累计等待次数/时长和累计回收；不暴露 DSN、主机、账号、SQL 或业务内容。schema 检查复用 `/readyz` 的 `Migrator.Check`，运行中发生缺失、checksum 漂移或数据库领先时明确提示实例不能安全接流量；不是只展示启动时缓存的版本。连接占用达到上限的 80% 才给当前注意态，累计等待明确是本次进程历史而非请求延迟。生产/开发配置模板显式给出 25/5 的池参数，`MAX_OPEN_CONNS` 新增 1–10000 严格上限。SSR 测试覆盖容量正常/接近上限摘要、累计指标、运行时间、摘流策略和 schema 漂移错误态。`make check` 全绿（126 个文件、domain 85.0%），当前精确代码的完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 111–119ms、Memory 门禁约 88–94ms，双 worker 门禁约 0.16s / 0.50s。当前验证层是服务端渲染/装配单测 + 真实开发 MySQL/Embedding/双 worker 集成；未做用户拥有的目标服务器系统页视觉验收，也未提交、推送或部署。

**2026-08-29 共享协作脉搏迭代**：概览看板新增近 7 个 UTC 自然日的共享写入序列、去重活跃成员/Agent 和最近贡献。它不是租户全库调用量：领域入口只接收治理层已解析的可读 team/project 空间，MySQL 在聚合前同时限定 tenant、成功结果、write 类型、窗口与 space 集合，因此个人空间、不可读项目、他人读取/召回、失败和窗口外事件不会先进入应用内存。空共享范围返回完整的 7 天零序列且不查存储；稀疏 SQL 日桶由领域层补零，避免时间轴错位。该组只描述采用情况，不解释成知识质量；局部查询失败不拖垮首页。领域、真实 MySQL 与 SSR 契约测试覆盖 UTC 窗口、空间去重、零填充、租户/权限/结果/访问类型/时间隔离和固定 7 天页面装配。`make check` 全绿（136 个文件架构检查、domain 85.1%），当前精确代码的完整 `make itest` 全绿；真实 MySQL 协作聚合用例通过，真实 bge-m3 Card 门禁约 112–134ms、Memory 门禁约 90–98ms，双 worker 门禁约 0.16s / 0.56s，既有 MCP、Shared Line、备份恢复、Prometheus 规则结构与发布包门禁均保持通过。`promtool` 仍因当前主机未安装而显式跳过。当前最远层是领域/SSR 自动化 + 真实开发 MySQL/Embedding/双 worker 集成；未做用户拥有的 1440/390px 页面视觉验收，亦未提交、推送或部署。

**2026-08-29 备份调度与新鲜度门禁迭代**：发布包新增 `verify-mysql-backup.sh` 和 systemd oneshot/timer。timer 每天 UTC 02:15 运行、随机延迟最多 30 分钟，并用 `Persistent=true` 补跑错过的计划；oneshot 使用 `claracore` 身份、0077 umask、受限写目录和 owner-only MySQL client file，备份完成后立即执行 26 小时新鲜度门禁。检查器按数据库选择最新原子发布归档，拒绝符号链接或缺少 companion 的产物，交叉验证文件名/metadata/字节数/SHA-256/gzip/UTC 创建时间，并以 machine-readable JSON 明示 `recoverability=not_proven`。模板不含任何历史清理命令，避免在异机副本和真实恢复尚未证明前自动删除唯一数据。自动门禁覆盖新鲜、过期、篡改、缺 metadata、符号链接、timer catch-up/随机延迟、服务权限和发布包内容；Bash syntax、ShellCheck、`make check`（126 个文件、domain 85.0%）与完整 `make itest` 全绿，真实 bge-m3 Card 门禁约 112–123ms、Memory 门禁约 95–102ms，双 worker 门禁约 0.16s / 0.44s。尚未在目标服务器安装/启用 timer、生成真实备份、接入告警或完成隔离恢复，因此当前只证明本地脚本与发布配置行为，不证明目标 RPO 或可恢复性；未提交、推送或部署。

**2026-08-29 备份状态看板迭代**：`verify-mysql-backup.sh` 可在完整性与年龄全部通过后，把同一 machine-readable JSON 通过同目录 0600 临时文件原子替换为最新成功回执；失败不会覆盖上一份成功回执，因此旧证据会随时间自然转成过期，而不是伪造新成功。服务新增 `CCS_BACKUP_STATUS_FILE` 与独立只读 adapter：只接受最大 8 KiB 的普通非符号链接文件，严格拒绝未知字段、路径型归档名、归档/创建时间不一致、非法年龄预算、额外 JSON 和 `recoverability` 越界。管理员系统页按当前实例时钟重新计算年龄，区分按期、过期、缺失/损坏、语义无效与时钟异常；只展示归档名和时间，不读取备份正文、扫描目录、解压或在请求路径哈希大文件，并始终提示“完整性通过不等于可恢复”。脚本→原子回执→Go reader→SSR 的契约、失败保留旧回执、0600 权限、配置路径和 systemd 装配均有自动门禁。Bash syntax、ShellCheck、`make check`（129 个文件、domain 85.0%）与完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 103–129ms、Memory 门禁约 89–90ms，双 worker 门禁约 0.17s / 0.54s。尚未部署到目标服务器、生成真实回执、接入外部告警或完成用户拥有的系统页视觉验收；未提交、推送或部署。

**2026-08-29 HTTP 资源边界迭代**：应用 listener 不再只设置请求头超时；`config.HTTP` 统一并严格校验 read-header/read/write/idle timeout、最大 header、Web 表单与 MCP 请求体。默认边界为 10s/30s/300s/120s、64 KiB header、1 MiB URL-encoded Web form 和 4 MiB MCP body；embedding timeout 不能超过 write budget。Web 在所有写 handler 前统一执行 `MaxBytesReader + ParseForm`，413/415 时不会进入登录、CSRF 或治理动作，避免 `FormValue` 忽略错误后按部分字段继续；MCP 复用 SDK 对 Content-Length/chunked/HTTP2 的限制并把请求取消传播给 tool handler。nginx 两份模板同步 body、header/body slow-client、keepalive 和 upstream timeout，应用自身仍是直连时的最终边界。`make check` 全绿（130 个文件架构检查、domain 85.0%），完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 112–124ms、Memory 门禁约 90–96ms，双 worker 门禁约 0.17s / 0.47s，既有 MCP E2E、真实 MySQL、备份脚本与发布包门禁均通过。当前最远层是本地协议/配置/装配单测 + 真实开发 MySQL/Redis/embedding 集成；未在目标 nginx 执行 `nginx -t` 或实测超大/慢客户端，未执行用户拥有的页面操作，亦未提交、推送或部署。

**2026-08-29 HTTP 可观测性与 panic 隔离迭代**：两个 Host 外新增纯传输中间件，每个请求覆盖不可信入站值并返回服务端生成的 `X-Request-ID`；结构化完成日志只记录 request ID、method、ServeMux route pattern、status、response bytes 与 duration，不记录 query、原始未匹配路径、Authorization、Cookie、正文、身份或 panic 值。正常健康探针降到 debug，429 为 warn，5xx/panic 为 error，避免探针和普通 4xx 制造告警噪声。未提交响应的 panic 返回通用 no-store 500；已经提交/流式响应的 panic 写脱敏 stack 后转成 `http.ErrAbortHandler`，让 `net/http` 关闭连接且不再打印可能含任意值的默认 panic。wrapper 保留 `http.Flusher`、`Unwrap` 和 `io.ReaderFrom`，MCP event-stream 不因可观测层退化。Go server 自身错误也接入同一 slog sink。`make check` 全绿（132 个文件架构检查、domain 85.0%），完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 105–122ms、Memory 门禁约 86–97ms，双 worker 门禁约 0.17s / 0.56s，既有 MCP E2E、真实 MySQL、备份/恢复与发布包门禁均通过。当前最远层是本地传输/日志/异常单测 + 真实开发 MySQL/Redis/embedding 集成；未在目标服务器检查 journald/Supervisor 日志、真实流式中断或反代响应头，未提交、推送或部署。

**2026-08-29 HTTP 健康看板迭代**：请求可观测层新增固定 5 分钟滚动聚合，管理员系统页一眼显示本实例已完成业务请求、4xx、5xx、panic、平均耗时、P95 直方图上界与当前在途；健康探针排除，避免探针频率粉饰业务流量。任一 5xx 或 panic 触发注意态，4xx 只作为调用边界信号陈列。实现严格使用 5 个分钟桶和 10 个固定延迟桶，不保存 route、request ID、身份、租户或正文；耗时最多按 30 分钟入桶，内存不会随流量/租户增长。窗口重启清空且不跨实例聚合，文档明确不能冒充外部时序监控。collector 轮转/过期/探针排除/延迟上限/P95/在途与系统页错误态已有单测；`go test -race ./cmd/server`、`make check`（134 个文件架构检查、domain 85.0%）与完整 `make itest` 全绿，真实 bge-m3 Card 门禁约 104–127ms、Memory 门禁约 83–94ms，双 worker 门禁约 0.15s / 0.53s，既有 MCP E2E、真实 MySQL、备份/恢复与发布包门禁均通过。当前最远层是并发安全 collector/SSR 单测 + 真实开发 MySQL/Redis/embedding 集成；未做用户拥有的系统页视觉验收、目标服务器流量观察、提交、推送或部署。

**2026-08-29 外部活源契约迭代**：真实服务复核推翻了旧文档里“odoc/Atlas 都已可枚举”的假设。odoc 当前 MCP 只有搜索、主题和来源接口，不具备稳定文档 ID、版本与正文读取，暂不接入；Atlas 的 `atlas_catalog(domain=source_documents)` + `atlas_source_document` 已具备分页枚举和按 ID 取文契约。代码新增独立 `domain/external.Source` 与 Atlas Streamable HTTP 只读适配器，服务 bearer 只走请求头，错误不回显 endpoint/token；页大小、响应线长、正文大小、稳定 ID、系统边界、内容哈希、来源 URL 与详情身份都有 fail-closed 校验。镜像没有被伪装成 Card/Memory，正文也没有落本地。契约测试覆盖 Atlas 当前 JSON 文本结果、SDK 结构化结果、凭证、越界来源、身份漂移和超大正文。当前最远层仅为真实 Atlas 契约探测 + 本地适配器单测；元数据/无正文向量表、同步 worker、空间权限与统一排序仍是下一刀，未提交、推送或部署。

**2026-08-29 Atlas body-free 同步迭代**：`0009_external_sources.sql` 新增来源、同步回执、文档元数据与独立向量四表，外部向量表没有 `source_text` 或正文列。领域同步按完整分页快照运行：相同 revision/model 只 touch 元数据，变化项才按页取正文并批量 embedding；枚举总数漂移、重复 ID、取文时 revision 改变、向量数量/维度异常都会 fail closed。元数据与对应向量同事务发布，失败快照不执行 missing 收尾；成功快照才软标记未见项。多实例不是沿用内部索引的 Redis fail-open 租约——完整快照互相覆盖会误标 missing，因此来源行使用带过期时间的 MySQL 独占 run ID，进程崩溃后可自动接管。部署配置支持服务 token、明确 space、同步周期、整轮/单请求超时与响应/正文上限，worker 启动即跑并周期重试。当前仍缺统一候选检索、Web 来源状态/手动重试和 odoc 枚举契约；尚未提交、推送或部署。

**2026-08-29 Atlas body-free 同步最终收据**：系统页现已只读展示 Atlas 配置、同步状态、最近成功时间、活动镜像数和过期租约；仍缺 Web 手动重试与统一候选检索。最终验证为 `make check` 通过（架构检查 145 个文件、domain 覆盖率 85.0%）、`make itest` 通过（真实 MySQL schema `0009` / 9，含无正文列、租约、失败快照保留与成功 missing 收尾）、`go run ./cmd/server -check` 返回 `ready` 且 schema current。没有运行真实 Atlas 文档同步，没有浏览器视觉验收，也没有提交、推送或部署。

**2026-08-29 统一知识检索迭代**：在 24 工具硬上限内扩展兼容的 `memoria_search`，旧 `hits` 保持 Memory-only，新 `matches` 把同模型 Memory / Card / Atlas 语义命中按余弦、空间 rank、新近度统一排序；两个独立全文索引的分数不通约，明确落入 `memory_text` / `card_text` 组。外部候选在 MySQL 返回标题、来源元数据或向量前同时下推 tenant 与可读 space，领域层再校验 Store 没有逃逸；结果只带来源链接、revision、上游/索引/最近成功时间和 freshness，不含正文。失败刷新保留项标 `last_success_preserved_after_failure`，连接关闭后仍可 `mirror_only` 检索；labels 与 historical 视图不会被外部镜像绕过。自动上下文仍不把无本地正文的外部标题冒充证据块。当前仍缺 Web 手动同步与真实 Atlas 文档/目标部署验收。

**2026-08-29 外部来源治理迭代**：新增管理员 `/admin/sources`，只读展示来源健康、租约与最近 20 次 body-free 同步回执；手动“开始同步”经过 admin、CSRF、租户校验和统一 Web 治理审计，只把请求放进容量为一的进程内队列。定时与手动请求由同一个 Atlas worker 串行消费，重复请求被合并，多实例继续用 MySQL 来源租约防止重叠完整快照；页面明确区分“已入队”与“同步成功”。Store 的回执查询在 SQL 下推 tenant 并有 100 条硬上限，不返回外部正文。`make check` 全绿（148 个文件架构检查、domain 覆盖率 85.2%），完整 `make itest` 全绿；真实开发 MySQL 的回执查询覆盖成功计数、排序所需时间与跨租户隐藏，schema 仍为 `0009` / 9，真实 bge-m3 Card 门禁约 97–123ms、Memory 门禁约 97–103ms。`go run ./cmd/server -check` 返回 `ready`、MySQL reachable、schema current；`promtool` 因本机未安装仍按既有规则跳过。当前最远层是 SSR/权限/CSRF/审计单测 + 真 MySQL 回执与既有 MCP/embedding 集成；仍未运行真实 Atlas 同步、浏览器视觉验收、目标部署、提交或推送。

**2026-08-29 Atlas 手动请求持久化迭代**：`0010_external_sync_requests.sql` 把手动请求从进程内提示升级为 MySQL 权威状态。pending 与 claimed 分开：重复点击只保留一轮等待，当前处理期间可排一轮 follow-up；领取使用 owner token + 到期时间，实例退出或未取得完整快照租约时可由另一实例重领。worker 启动和定时 tick 都优先消费持久请求，同实例 channel 只负责即时唤醒，其他实例最迟在 5 秒轮询发现。页面显示“没有等待请求 / 等待处理 / 正在处理 / 等待其他实例接管”，成功文案只说“同步请求已记录”。真实开发 MySQL 已应用 `0010`，Store 集成测试证明 pending 合并、claim 排他、follow-up 保留、retry 释放和跨实例重领；body-free schema 与 97 个定长 CHAR 的 `ascii_bin` 门禁继续通过。`make check` 全绿（148 个文件架构检查、domain 覆盖率 85.3%），完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 109–129ms、Memory 门禁约 96–109ms，既有真 token/MCP/权限/统一检索、双 worker、备份恢复和发布包门禁均通过。`go test -race ./cmd/server ./internal/web ./internal/domain/external` 通过，`go run ./cmd/server -check` 返回 `ready`、MySQL reachable、schema `0010` current；`promtool` 因本机未安装仍按既有规则跳过。当前最远层是 worker/SSR/权限/CSRF/审计自动测试 + 真 MySQL 持久请求/租约与既有 Redis/embedding/MCP 集成；仍未调用真实 Atlas、执行浏览器操作、目标部署、提交或推送。

**本轮最终验证收据**：`make check` 全绿（架构检查 146 个文件、domain 覆盖率 85.2%）；`make itest` 全绿，新增真 token → MCP → 权限解析 → Memory/Card/Atlas 统一排序 → 真 MySQL → 真 bge-m3 的外部命中链路，且真库外部 Store 继续覆盖不可读空间过滤、失败刷新保留与 body-free schema；开发库仍为 `0009` / 9。`go run ./cmd/server -check` 返回 `ready`、MySQL reachable、schema current。`promtool` 因本机未安装仍按既有规则跳过。没有调用真实 Atlas 文档同步，没有浏览器视觉验收、目标部署、提交或推送。

**2026-08-29 浏览器与响应缓存安全迭代**：Web CSP 从宽泛同源默认收紧为 `default-src 'none'` 后逐类开放，新增 object/form/frame/base/manifest/worker 边界、DENY frame fallback、no-referrer、Permissions Policy、COOP/CORP、cross-domain policy 与 noindex。所有非静态 SSR/重定向/错误均 `no-store, private`，覆盖一次性 token、初始密码、CSRF 与租户内容；同源 CSS/JS 因文件名未指纹化只缓存 300 秒，不冒充 immutable。MCP 成功、401 与协议错误也 no-store；定向测试先真实发现 SDK 在中间件之后覆盖为 `no-cache, no-transform`，最终改为 response writer 在提交头时强制 `no-store, no-transform`，同时保留 Flush/Unwrap/ReadFrom 流式语义。nginx 两份模板关闭版本号。`make check` 全绿（134 个文件架构检查、domain 85.0%），完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 110–125ms、Memory 门禁约 87–88ms，双 worker 门禁约 0.16s / 0.41s，既有 MCP E2E、真实 MySQL、备份/恢复与发布包门禁均通过。当前最远层是 Web/MCP 响应单测 + 真实开发 MySQL/Redis/embedding 集成；未从目标公网 HTTPS 检查实际反代头、浏览器 CSP 控制台、页面视觉或缓存行为，未提交、推送或部署。

**2026-08-29 MCP 调用滥用保护迭代**：MCP 新增两层不可关闭的固定窗口预算。认证前按可信来源计数，默认 1 分钟 1800 次，限制随机无效 token 对 MySQL 的查询；仅 loopback 直连时信任 nginx 覆盖的 `X-Real-IP`，非 loopback 伪造头无效。认证后按 agent 默认 1 分钟 300 次，同一 agent 的多个 token 共用预算，不能靠轮换 token 绕过；任一上限返回 429 与向上取整的 `Retry-After`。Redis 可用时跨实例共享，故障时退回独立的 20,000-key 有界本地池，不与登录限流争抢容量。持久 key 只含来源或 agent ID 的 SHA-256，不含随机 token、用户、租户或 agent 文本；系统页显示预算与降级形态。`go test -race ./internal/mcp ./internal/adapters/ratelimit`、`make check`（134 个文件架构检查、domain 85.0%）与完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 100–123ms、Memory 门禁约 87–88ms，双 worker 门禁约 0.16s / 0.46s，既有 MCP E2E、真实 MySQL、备份/恢复与发布包门禁均通过。当前最远层是协议/配置/系统页单测 + 真实开发 MySQL/Redis/embedding 集成；尚未在目标服务器用隔离 agent 实测跨实例共享、429 与 Redis 故障退回，未提交、推送或部署。

**2026-08-29 本机 Prometheus 指标迭代**：新增可选 `CCS_METRICS_ADDR`，只接受数字 loopback 地址且端口不得与业务 listener 相同；第二个 HTTP server 只暴露 Prometheus text format 0.0.4 的 `GET /metrics`，复用 listener 超时/头部边界，并与业务服务同进程优雅关闭。指标无用户、租户、agent、route 或 request ID 标签，常量基数覆盖 build/uptime/accepting、5 分钟 HTTP 请求/4xx/5xx/panic/延迟、`database/sql` 连接池累计等待与回收，以及有界备份回执的新鲜度；抓取不执行 SQL、不访问 Redis、不扫描归档，也不宣称恢复已证明。nginx 两份模板显式对公网 `/metrics` 返回 404；部署脚本可用第二个 loopback URL 验证指标。系统页同步显示本机装配状态。已用真实开发 MySQL/Redis/embedding 启动 `127.0.0.1:51980` + `127.0.0.1:51982` 双监听，部署脚本通过；SIGINT 后实际观察 `claracore_instance_accepting_requests` 从 1 变 0、`/readyz` 同时 503，2 秒摘流后两个 listener 正常关闭。`go test -race ./cmd/server ./internal/adapters/backupstatus`、`make check`（136 个文件架构检查、domain 85.0%）与完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 112–131ms、Memory 门禁约 101–106ms，双 worker 门禁约 0.17s / 0.41s，既有 MCP E2E、真实 MySQL、备份/恢复与发布包门禁均通过。当前最远层是本地真实双监听/摘流 + 真实开发 MySQL/Redis/embedding 集成；未在目标服务器接 Prometheus/告警、验证公网 404 或观察真实流量，未提交、推送或部署。

**2026-08-29 Prometheus 默认告警迭代**：发布包新增 `claracore-server.rules.yml` 与只用于合并的 `prometheus.yml.example`。10 条规则覆盖 target down、异常长摘流、备份回执缺失/过期、panic、持续 5xx、P95 超桶、连接池统计缺失、持续 ≥80% 占用和持续新增等待；每条都有 1–15 分钟 `for` 窗口，单次 4xx、瞬时 5xx、正常短发布和单次等待不通知。down 与仍可抓取的摘流用 `up` 互斥，备份缺失与过期用 receipt available 互斥；滚动 HTTP gauge 不错误套 `rate()`。结构测试把规则引用的全部 `claracore_*` 名称与当前 exporter 源码对照，发布包门确认两个文件随包交付；Ruby Psych 已确认两份 YAML 可解析。本机未安装 `promtool`，因此测试在工具存在时自动执行，部署文档把 `promtool check rules` 设为加载前硬门。`make check` 全绿（136 个文件架构检查、domain 85.0%），完整 `make itest` 全绿；真实 bge-m3 Card 门禁约 108–121ms、Memory 门禁约 92–93ms，双 worker 门禁约 0.16s / 0.59s，既有 MCP E2E、真实 MySQL、备份/恢复与发布包门禁均通过。当前最远层是 YAML 解析 + 指标契约/低噪声结构测试 + 真实发布包；未由 `promtool` 或真实 Prometheus/Alertmanager 加载、触发或验证通知路由，未提交、推送或部署。

**2026-08-29 外部活源生产监控迭代**：metrics listener 的抓取路径继续保持无 SQL；新增独立后台采样器，每 30 秒以 3 秒超时读取一次 tenant-scoped、body-free `SourceHealth`，只把固定字段的总量、失败/过期、新鲜度和持久请求/claim 状态写入并发安全内存快照。exporter 不含 tenant、source、URL、error、run 或 user 标签，基数不随团队、来源或流量增长；来源 stale 预算取 `max(15m, 2 × sync interval + sync timeout)`。默认告警由 10 条扩至 15 条：健康快照失败或超过 120 秒未更新、同步持续失败、镜像持续 stale、claim 过期和无活动/过期 claim 的 pending backlog；外部症状全部由可用快照门控，失败会抑制 stale，活动或已知过期 claim 会抑制 backlog。`go test -race ./cmd/server`、规则契约、Ruby YAML 解析、`make check`（150 个文件、domain 85.3%）、完整 `make itest` 与 `go run ./cmd/server -check` 全绿；真开发 MySQL schema 为 `0010` / 10，真实 MCP/权限/统一 Atlas 镜像检索与 bge-m3 门禁继续通过。本机仍无 `promtool`，且本轮没有调用真实 Atlas、启动真实 Prometheus/Alertmanager、部署目标服务器、执行浏览器操作、提交或推送；当前最远证据是并发安全采样/exporter/规则自动门禁 + 真实开发 MySQL/MCP/embedding 回归。

**2026-08-29 可自验证发布物迭代**：发布二进制新增无副作用 `-version`，在读取 `.env` 或连接 MySQL/Redis/Embedding 前输出应用版本、Go 工具链和 `GOOS/GOARCH` JSON；未知 `CCS_*` 环境变量存在时仍可使用，且与 migrate/check/bootstrap/ingest 冲突时 fail closed。打包脚本改为先在输出目录生成隐藏临时归档，再原子替换正式 tar 与 checksum，避免中断留下同名半包；包内新增覆盖二进制、部署模板、脚本、文档与许可证的 `MANIFEST.sha256` 以及 `verify-release.sh`。校验器拒绝非法/重复/缺失路径、普通文件被符号链接替换、内容篡改与额外文件，发布测试真实跨编译 Linux amd64、解包运行校验器并分别注入 README 篡改和额外文件证明失败。文档明确两层 SHA 只证明传输/目录一致性，不自行证明未知来源的发布者身份。Bash syntax、ShellCheck、`go test -race ./cmd/server ./test -run 'Version|ReleasePackage'`、`make check`（152 个文件、domain 85.3%）、完整 `make itest`、`go run ./cmd/server -version` 与 `-check` 全绿；真开发 MySQL schema 仍为 `0010` / 10，真实 MCP/权限/检索/embedding 回归保持通过。当前最远层是本机运行时版本自检 + 真实 Linux amd64 发布包逐文件完整性/篡改门禁 + 真开发依赖回归；没有发布签名、目标服务器解包验收、提交、推送或部署。

**2026-08-29 升级与回滚真实性迭代**：审计确认原顺序“旧实例服务中 → 新二进制迁移 → 切 current”与精确 schema readiness 冲突：迁移后旧实例必然因数据库领先退出 ready，而上一版二进制也不能仅靠切符号链接回滚。新增只读 `-check-upgrade`：在停服务前验证数据库账本是新二进制迁移集合的连续可信前缀，逐一核对已应用 checksum，拒绝未知版本、已发布迁移漂移和“后版本存在但前版本缺失”的账本空洞；JSON 给出 current/target、pending、下一迁移、`requiresSchemaMaintenance` 与 `rollbackMode`。迁移执行器现在也在任何 pending DDL 前复用同一完整前缀门禁，不会自动补账本洞。部署契约改为：pending=0 才允许 schema-neutral 的逐实例二进制滚动；pending>0 必须先停全部 Server/摄取 writer，再生成无写入缺口的维护窗口备份、迁移、精确 `-check`、启动新实例；迁移后的旧版回退是数据库恢复决策，不是假装只切 binary。自动测试用真实开发 MySQL + 合成 `0011` 证明计划报告一个 pending 且没有执行 ALTER；另有真实 CLI 集成证明当前 `0010/10` 返回 `pending=0`、`rollbackMode=binary_only`。`go test -race` 定向门禁、`make check`（153 个文件、domain 85.3%）、完整 `make itest`、发布包重建门禁、`go run ./cmd/server -check-upgrade` 与 `-check` 全绿；真实 MCP、权限、检索、embedding、备份/恢复脚本回归保持通过。当前最远层是升级计划/迁移前缀单测 + 真开发 MySQL 只读升级计划/CLI + 全量集成；尚未在目标服务器执行维护窗口、真实 pending migration、数据库恢复、提交、推送或部署。

**2026-08-29 自动上下文共享语料质量门禁迭代**：原真实 bge-m3 门禁把每个正样本放在只有一张卡的独立租户里，只能证明“相关内容存在时能返回”，没有证明团队语料竞争或歧义安全。现在 Card 门禁复用固定检索评估集的四类标注卡，全部放进同一租户/空间：四个正样本都必须交付对应卡，无关问题必须 `low_relevance`；另用完全相同向量输入模拟重复摄取，Card 与 Memory 都必须 `ambiguous_top_results`。新增完整真 token → MCP → 权限解析 → Memory/Card → 真 MySQL → 真 bge-m3 用例，两域同时高置信时必须 `ambiguous_domains`，不生成 block 或 `contextId`。阈值 `0.55 / 0.03` 未调整。定向 integration race 通过；`make check` 全绿（153 个文件、domain 85.3%），完整 `make itest` 全绿，其中共享 Card 正负约 101–137ms、重复 Card 约 131ms、Memory 正负/重复约 84–116ms、跨域 MCP 用例约 1.44s（含租户装配与索引）。`-check-upgrade` 返回开发库 `0010/10`、pending=0、`binary_only`，`-check` 返回 ready。当前最远层是固定共享语料 + 真实开发 MySQL/embedding/MCP 全量集成；未提交、推送、部署，也未做用户页面视觉验收。

**2026-08-29 自动上下文放弃原因看板迭代**：每条成功的 `gateway_auto_context` trace 现在额外记录服务端生成的固定 `reason`，仍不记录 prompt、证据正文、实体或 actor。近 30 天租户聚合把 abstain 归入六个低基数桶：无相关证据、域内/跨域歧义、语义不可用、检索超时、Shared Line 未就绪、其他/旧记录；旧 trace 缺少 reason 时进入“其他”，六类总和必须与 `abstained` 一致。`context_quality_status.abstainReasons` 与概览看板同时暴露该分布，语义不可用或超时触发注意态；分类只用于语料治理和运行排障，不解释为准确率。真实 MySQL 用例证明租户、30 天窗口、成功 trace 与历史兼容边界；完整真 token → MCP → 跨域拒答 → 审计 → MySQL → `context_quality_status` 用例证明一次 `ambiguous_domains` 精确进入唯一歧义桶。定向 race、`make check`（153 个文件、domain 85.3%）、完整 `make itest` 全绿；开发库仍为 `0010/10`，`-check-upgrade` 返回 pending=0 / `binary_only`，`-check` 返回 ready。当前最远层是协议/领域/Web 单测 + 真实开发 MySQL/embedding/MCP 全量集成；未提交、推送、部署或执行用户页面视觉验收。

**2026-08-29 多实例入口验收迭代**：新增可选严格配置 `CCS_INSTANCE_ID`，仅接受 1–64 字节公开安全标签；配置后 `/livez`、`/readyz` 与兼容 `/healthz` 返回 `instanceId`，单实例留空时保持省略。发布包新增只读 `verify-cluster.sh`：先直连至少两个显式实例核对 live/ready、MySQL/schema 与版本一致性，再以 no-cache、connection-close 请求采样公开负载均衡入口，任何未知实例、版本/schema 漂移或预期实例从未命中都会失败。模拟门禁同时证明双实例轮转通过、入口只返回一个实例失败；Bash syntax、ShellCheck、定向 race、`make check`（153 个文件、domain 85.3%）与完整 `make itest` 全绿，发布包真实跨编译门禁证明脚本随包且可执行。开发库仍为 `0010/10`，`-check-upgrade` 返回 pending=0 / `binary_only`，`-check` 返回 ready。当前最远层是严格配置/探针单测 + 模拟负载均衡覆盖 + Linux 发布包 + 真实开发 MySQL/Redis/embedding/MCP 全量回归；尚未在两台 Linux、真实负载均衡或故障/滚动发布场景运行，也未提交、推送、部署或执行用户 UI。

**2026-08-29 发布源码身份迭代**：修正多实例验收只比较人工版本号的证据漏洞。`package-release.sh` 现在按稳定路径顺序散列 `go.mod`、`go.sum` 与 `cmd/`、`internal/`、`migrations/` 的 Go/SQL/内嵌 Web 资源，把当前实际工作树的 64 位 `sourceDigest` 链入 Linux 二进制；发布测试独立重算摘要并证明产物包含精确值。`-version`、`-check-upgrade`、`-check`、live/ready/health 探针、`claracore_build_info` 与管理员系统页暴露同一摘要。`verify-cluster.sh` 现在同时核对版本、源码摘要、环境与 schema；自动门禁证明同版本异摘要、生产环境 `sourceDigest=development`、入口漏实例都会失败。源码摘要明确不冒充二进制可复现、签名或发布者认证。Bash syntax、ShellCheck、定向 race、`make check`（153 个文件、domain 85.3%）和完整 `make itest` 全绿；开发库仍为 `0010/10`，本地 `-version/-check-upgrade/-check` 均按开发构建显示 `sourceDigest=development`，升级计划仍为 pending=0 / `binary_only`。尝试推进真实恢复证据时确认本机没有 mysql/mysqldump，Docker CLI 29.1.3 存在但 daemon 未运行；未越权启动 UI。当前最远层是确定性源码身份 + Linux 发布包 + 模拟混合构建拒绝 + 真实开发 MySQL/Redis/embedding/MCP 全量回归；仍无目标恢复、两台 Linux/真实 LB、发布签名、提交、推送、部署或用户 UI 验收。

**2026-08-29 实际制品身份迭代**：补上“同版本、同源码仍可能由不同工具链或参数生成不同二进制”的证据缺口。Server 启动时只计算一次当前可执行文件的 SHA-256，读取失败即 fail closed；`-version`、`-check-upgrade`、`-check`、live/ready/health 探针、`claracore_build_info` 和管理员系统页统一暴露 `binaryDigest`。单实例部署校验现在要求 live/ready 的版本、源码摘要、二进制摘要与环境一致，并拒绝生产环境 `sourceDigest=development`；多实例/LB 校验进一步拒绝任一二进制摘要漂移。默认告警增至 17 条，新增生产开发构建和同一 Prometheus job 内混合制品身份告警，后者等待 15 分钟以容纳正常短滚动。独立临时构建证明程序自报摘要与宿主 `shasum -a 256` 完全一致；定向 race、Bash syntax、ShellCheck、Ruby YAML 解析、`make check`（153 个文件、domain 85.3%）和完整 `make itest` 全绿，真实开发 MySQL 仍为 schema `0010/10`，本地三个只读命令均返回同一 64 位二进制摘要。`promtool` 本机未安装，规则仍需在监控主机加载前验证；二进制摘要只证明精确制品身份，不证明签名或发布者。当前最远层是本机实际制品哈希 + 单/多实例模拟拒绝 + 真开发 MySQL/Redis/embedding/MCP 全量回归；没有目标服务器、真实 LB/Prometheus、签名、提交、推送、部署或用户 UI 验收。

**2026-08-29 Ed25519 发布者认证迭代**：外层 SHA-256 不再只能依赖人工从另一渠道抄录。`package-release.sh` 可选接收第 4 个 Ed25519 私钥绝对路径；私钥必须是当前用户拥有的普通非符号链接文件，且 group/other 无任何权限。脚本对最终 `.tar.gz.sha256` 生成 64 字节 detached signature，私钥和公钥都不进入归档；未提供私钥时仍允许开发包，但明确提示不能作为签名生产发布，并拒绝用未签名构建覆盖已有同名签名。新增 `verify-release-signature.sh`：只接受同目录、安全文件名、普通非符号链接的 archive/checksum/signature，用预先固定的 Ed25519 公钥先认证 checksum，再核对归档 SHA-256；正确签名通过，错误公钥、篡改 checksum、篡改 archive、篡改 signature 和 group-readable 私钥均由真实 OpenSSL 攻击路径门禁证明失败。文档明确公钥与未知归档同源下载不构成信任根，生产首次发布仍需维护者在受控发布机生成并保管私钥、通过独立组织渠道固定公钥 DER 指纹，并演练先分发新公钥再轮换。定向 race、Bash syntax、ShellCheck、`make check`（153 个文件、domain 85.3%）和完整 `make itest` 全绿；真实开发 MySQL 仍为 schema `0010/10`，既有 MCP/权限/检索/Redis/embedding/恢复脚本回归保持通过，`promtool` 仍因本机未安装跳过。当前最远层是临时 Ed25519 密钥 + 真实 Linux amd64 打包/签名/认证/篡改拒绝 + 真开发依赖全量回归；尚未生成生产密钥、固定真实公钥指纹、签署正式版本、提交、推送、部署或执行用户 UI。

**2026-08-29 技术恢复回执可观测性迭代**：隔离恢复不再只留下运维人员手里的 TSV。`rehearse-mysql-restore.sh` 可在默认清理路径生成 0600 原子 JSON 回执，但只有归档导入、逐表 `CHECK TABLE QUICK`、精确行数与 migration 账本枚举、不可变报告发布，以及本次随机目标库成功删除全部完成后才替换；失败保留旧回执并自然过期，`--keep-target` 明确不能生成回执。Server 通过最大 8 KiB、拒绝 symlink/非普通文件/未知字段/路径名/尾随 JSON/时钟异常/语义越界的 reader，把技术恢复状态贯通管理员系统页、部署校验和无数据标签的 Prometheus 指标；默认规则增至 19 条，新增“备份健康但恢复回执缺失”和“恢复回执存在但过期”两个互斥门控告警。页面与文档固定说明这只证明技术导入、表检查、枚举与清理，不读取恢复数据/报告/归档，也不证明代表性 Memory、Card、Shared Line 人工语义抽查或 RPO/RTO。定向 race、Bash syntax、ShellCheck、Ruby YAML/19 条规则和 `git diff --check` 通过；`make check` 全绿（155 个文件、domain 85.3%），完整 `make itest` 全绿，真实开发 MySQL schema 仍为 `0010/10`，MCP、Redis、embedding、恢复脚本与 Linux 发布包回归保持通过；只读 `-check` 返回 ready。`promtool` 因本机未安装仍按既有规则跳过。当前最远层是安全 parser/SSR/指标/告警/脚本的自动门禁 + 真实开发 MySQL/Redis/embedding/MCP 全量集成；没有真实隔离 MySQL 回执、人工语义抽查、目标部署、浏览器操作、提交或推送。

**2026-08-29 生产依赖传输安全迭代**：严格配置现在把“本机 loopback 可明文、跨主机必须验证身份”变成启动前硬门。production 中非 loopback MySQL/Redis 必须使用 `verify-full`；客户端固定 TLS 1.2+，支持系统或私有 CA、证书名覆盖与可选 mTLS，不提供 skip-verify、preferred/fallback。非 loopback Embedding/Atlas 不论是否带 token 都必须 HTTPS，因为请求本身可能包含团队知识正文；依赖 URL 中的用户名/密码也被拒绝。共享 TLS loader 对每个 PEM 限制 1 MiB，拒绝 symlink/非普通文件/打开时变化、可被 group/other 写入的证书，以及非当前用户拥有或非 0600 边界的私钥。MySQL connector 与 Redis client 均接入同一验证配置，管理员系统页展示四类依赖的配置传输形态而不泄露主机、证书路径或 server name。定向 race、配置/文件攻击路径和真实 miniredis TLS 握手读写通过；`make check` 全绿（157 个文件、domain 85.3%），完整 `make itest` 全绿，Linux 发布包重建、真实开发 MySQL schema `0010/10`、MCP、Redis、embedding、备份/恢复与发布签名回归保持通过。`-check-upgrade` 返回 pending=0 / `binary_only`，`-check` 返回 ready；当前开发环境连接远程 MySQL/Redis/Embedding 仍按 development 规则允许明文，不能解释为生产 TLS 证据。当前最远层是 TLS 材料安全/真实 Redis TLS/系统页自动门禁 + 真实开发依赖全量集成；尚未在目标网络验证真实 MySQL TLS、企业私有 CA/mTLS、远程 Embedding/Atlas 证书与轮换，未部署、未执行浏览器操作、未提交或推送。

**2026-08-29 团队数据可移植导出迭代**：管理员 `/admin/export` 经过 admin、CSRF 与明确范围确认后，必须先持久写入敏感读取 meta-audit，审计失败时不会查询内容。MySQL adapter 在 repeatable-read 只读快照中按稳定顺序导出成员公开身份、Agent、空间权限、Memory、Card/召回条件/标签/关联与 Shared Line 当前/位置/交接历史；密码/Token 哈希、会话、embedding、审计、外部镜像和运行队列固定排除。应用先在 0600 临时文件中完成 ZIP、逐文件 SHA-256 和最终 `manifest.json`，成功落盘后才用精确长度下载；失败不发送半包，同一实例只允许一份，`CCS_EXPORT_MAX_BYTES` 默认 1 GiB 且严格限制 1 MiB–16 GiB。真实开发 MySQL 门禁同时写入本租户与外租户正文、测试密码/Token/audit 标记，证明归档含代表性 Memory/Card/Shared Line、逐文件 hash 全匹配且四类敏感/跨租户标记均不存在；大小边界返回 typed error。定向 race、`make check`（162 个文件、domain 85.3%）与完整 `make itest` 全绿；Linux 发布包、schema `0010/10`、MCP、检索、Redis/embedding、备份/恢复回归保持通过，`-check-upgrade` 返回 pending=0 / `binary_only`，`-check` 返回 ready。它是可移植与离线检查格式，不是自动回灌或灾备恢复，内嵌 hash 也不提供攻击者篡改下的真实性。当前最远层是 SSR/审计/临时文件边界自动测试 + 真实开发 MySQL 租户隔离导出 + 全量集成；尚未在目标服务器下载真实团队归档或完成人工页面验收，未提交、推送或部署。

**2026-08-29 团队导出离线校验迭代**：v1 文件名、顺序、固定排除项和 manifest 结构从 MySQL adapter 的隐含实现抽成 `internal/portableexport` 单一契约，导出端与验证端共用。发布包内 Server 新增 `-verify-export`：它在配置加载与可执行文件摘要之前运行，不读取 `.env`，不连接 MySQL、Redis 或 Embedding，只对本地普通非符号链接 ZIP 输出机器可读 JSON。验证器严格检查 15 个条目的完整唯一顺序、manifest 未知字段/尾随 JSON、格式/schema/租户/UTC 时间、固定排除项、逐文件 SHA-256、JSONL 每行单一对象与记录数，并用 16 GiB 归档/单文件、64 GiB 总解压、32 MiB 单行和 1 亿记录硬预算拒绝 ZIP bomb；路径穿越、链接/特殊文件、异常压缩、内容篡改、计数伪造和非对象记录均有攻击路径测试。真实开发 MySQL 生成的租户隔离归档已通过同一验证器，Linux amd64 发布包门禁确认命令随二进制交付；定向 race、`make check`（168 个文件、domain 85.3%）、完整 `make itest` 与 `git diff --check` 全绿，schema 仍为 `0010/10`，`-check-upgrade` 仍为 pending=0 / `binary_only`，`-check` 为 ready；`promtool` 仍因本机未安装跳过。该收据证明格式、自洽性与有界可读，不提供来源真实性、自动回灌、代表性语义抽查或灾备恢复。当前最远层是离线攻击路径 + 真实开发 MySQL 归档 + Linux 发布制品 + 全量集成；尚未用目标服务器下载的真实团队归档运行发布二进制，未完成人工内容抽查、提交、推送或部署。

**2026-08-29 空部署团队导入迭代**：v1 manifest 增加租户创建时间，离线 `-plan-import` 在不加载配置或外部依赖的情况下，对 13 个 JSONL 的严格字段、唯一性、角色/状态以及成员、Agent、空间权限、Memory、Card、标签、关联和 Shared Line 全历史引用做语义预检，并列出或验证接管管理员。`-import-export` 必须同时给出接管管理员、0600 绝对密码文件与 `-confirm-import-empty`；归档在同一个保持打开的已验证文件描述符上重做预检后，只连接当前 schema 的 MySQL，不启动 Redis、Embedding、worker 或 listener。导入使用 serializable 单事务并在任何既有租户存在时拒绝，保留源租户、成员、内容、作者与历史 ID；接管管理员以新密码激活且必须首登改密，其他成员保留身份/角色但禁用并使用随机不可恢复凭据。源凭据、Token、会话、embedding、旧审计、外部镜像和运行队列不导入；成功事务同时写一条 `source=cli`、`admin.import.apply` 治理审计，存在 Memory/Card/召回条件时结果明确要求后续索引回填。真实开发 MySQL 集成通过 session-local 临时表复刻当前表形状，证明 17 条跨全部数据集的导出可完整导入、密码接管/成员禁用、ID/作者/Shared Line 历史保持、排除表为空、成功审计存在、重复导入拒绝，且故意删除末端交接列时整事务连同审计回滚；数据库账号没有 CREATE DATABASE 权限，因此这不是独立目标库验收。定向 race、`make check`（171 个文件、domain 85.3%）、完整 `make itest`、Linux 发布包命令存在门禁与 `git diff --check` 全绿；一次对已发布 `0006` 注释的误改被真实 migration checksum 门禁拒绝并已撤回，schema 保持 `0010/10`，`-check-upgrade` 为 pending=0 / `binary_only`，`-check` 为 ready，`promtool` 仍因未安装跳过。当前最远层是离线攻击/关系预检 + 真实 MySQL 临时表事务/回滚 + Linux 发布制品 + 全量集成；尚未在目标服务器用真实团队归档完成独立空库接管、索引回填、人工语义/页面验收，未提交、推送或部署。

**2026-08-29 团队导入索引原子性迭代**：修正空部署迁移后仍需管理员手工发现并触发首次语义回填的体验与可靠性缺口。Memory 标题/正文、Card 标题/提炼/理解、每条召回条件的拼接、trim、2000 字截断与 SHA-256 现在由知识领域暴露同一组 pending 构造器，日常写入和批量导入不再各自复制语义。`-import-export` 读取严格配置但仍不连接 Redis/Embedding 服务；启用模型时，会在同一个 serializable 导入事务中只为 active Memory、active Card 及其召回条件生成当前模型的 pending 行，队列写入或后续任何数据/审计失败都会连同正文一起回滚。成功 JSON 和 CLI 审计元数据新增 `indexingStatus/indexingQueued/indexModel`：`queued` 等待正常 worker，`backfill_required` 明示导入时模型禁用，`not_required` 表示没有可索引 active 内容；`CCS_EMBEDDING_MODEL` 同时在配置层和 importer 层限制为无首尾空白且最多 64 字符，避免落库时才撞 `VARCHAR(64)`。真实开发 MySQL 临时表用例证明一条 Memory、一张 Card、一条 trigger 精确生成 3 条 `bge-m3` pending，source_text/hash/entity/model/status/attempts/vector 全部正确；禁用模型导入保持 0 条并返回 backfill_required，末端 continuity 写入故障会把已生成的 3 条队列行一并回滚。定向 race、`make check`（171 个文件、domain 85.3%）、完整 `make itest`、Linux 发布包与 `git diff --check` 全绿；schema 仍为 `0010/10`，`-check-upgrade` 为 pending=0 / `binary_only`，`-check` 为 ready，`promtool` 仍因本机未安装跳过。当前最远层是领域契约 + 真实 MySQL 导入/队列/回滚 + 全量集成；尚未在独立目标空库用真实团队归档启动 worker 清空队列并完成人工 Memory/Card/Shared Line 抽查，未提交、推送、部署或操作 UI。

**2026-08-29 团队导入歧义结果恢复迭代**：补上“事务已提交，但 SSH/终端/stdout 在成功 JSON 出现前断开”的生产歧义窗口。发布二进制新增只读 `-check-import -import-admin <账号>`，必须在正常 Server 启动前运行；它只加载严格配置、连接 MySQL 并核对当前 schema，不连接 Redis/Embedding 或启动 listener/worker。checker 在 repeatable-read 只读快照中要求恰好一个 active 租户、唯一匹配的 `admin.import.apply` 成功审计、接管 admin 仍为 active/admin/必须改密、其余成员全部 disabled/必须改密、13 个数据集计数与审计 records 一致、Token/Session/外部运行态/其他审计为零，并严格比对当前配置模型与 pristine pending 队列；成功返回 `status=verified`、数据集计数、排除行数、审计 ID/时间和索引状态。它刻意不是常驻健康检查：登录、创建 Token、worker 消费队列或任何正常审计之后都会拒绝，避免把变化后的库伪装成原始导入证据。真实 MySQL session-local 临时部署证明启用模型与禁用模型两种纯净回执都可重建，成员一旦激活即返回 `ErrTenantImportReceiptNotProven`，末端失败整事务回滚后也没有可伪造回执；当前开发库只读 CLI 负向探针因存在 2 个 active 租户正确拒绝，未连接可选依赖、未写库。定向 race、`make check`（172 个文件、domain 85.3%）、完整 `make itest`、Linux 发布包命令存在门禁与 `git diff --check` 全绿；schema 保持 `0010/10`，`promtool` 仍因本机未安装跳过。当前最远层是机器可读 CLI/真实 MySQL 纯净与变化后拒绝/发布制品/全量集成；尚未在独立目标空库制造真实 stdout 丢失并以发布二进制恢复回执，也未启动 worker、人工抽查、提交、推送、部署或操作 UI。

**2026-08-29 数据完整性巡检迭代**：迁移明确坚持全库无外键、由领域写入路径保证引用关系；为长期运行、异常恢复与绕过应用写入补上独立的只读证据。发布二进制新增 `-check-integrity`：严格配置与 schema 通过后，在 repeatable-read read-only 快照中执行固定 29 条规则，覆盖治理身份/空间权限、Memory/Card/召回/标签/关联/embedding、Shared Line 与外部镜像的关键跨表关系。回执只输出 schema、快照模式、规则名和违规计数，不含实体 ID、账号、标题或正文；健康返回 `verified`/0，违规仍先输出 `violations_found` JSON 再以非零状态退出。命令在 Redis/Embedding/listener/worker 装配前结束，不修复数据。真实 MySQL session-local 隔离表证明空部署 29 条 SQL 全部可执行且零违规，并注入孤立 user 与 label 精确得到两类各 1 条违规；检查前后行数不变。开发库只读实跑在 `0010` 上发现 2 条既有 Memory 同时缺失 tenant、space、author user 与 author agent 关系（4 个规则合计 8 次违规计数）；没有读取或输出内容/ID，也没有自动修复。`make check`（175 个文件、domain 85.3%）全绿；首次 `make itest` 的既有 MCP E2E 索引批次偶发报告 2/3，定向复跑通过，随后未过滤的完整 `make itest` 全绿；新 checker、发布包与 CLI 输出用例均通过。当前最远层是代码契约 + 一致性只读事务 + 真实 MySQL 健康/违规/无写入 + 发布制品 + 全量集成；开发库既有两条孤立 Memory 仍待备份后另行授权调查/修复，目标部署尚无回执，未提交、推送、部署或操作 UI。

**2026-08-29 数据完整性回执与告警迭代**：`-check-integrity` 结果现在带 UTC 检查时间；配置 `CCS_INTEGRITY_STATUS_FILE` 时，健康与违规结果都会通过同目录 0600 临时文件、文件 `fsync` 和原子替换发布严格的 `claracore-data-integrity/v1` 回执，基础设施/schema/SQL/发布失败则保留旧回执并让其过期。普通服务只安全读取最大 16 KiB、当前 schema 的回执，管理员系统页展示新鲜度、规则数、违规总数和最多三个固定规则名；Prometheus 增加常量基数的 available/fresh/verified/age/rules/violations 指标，抓取和页面请求都不重跑关系 SQL。发布包新增每日 UTC 03:15 的 systemd oneshot/timer；部署探针要求回执可用、新鲜、已验证且零违规，默认告警增至 22 条，覆盖缺失、过期和违规并用门控避免重复通知。定向 race、Bash syntax、ShellCheck、Ruby YAML/22 条规则、`git diff --check`、`make check`（178 个文件、domain 85.3%）和未过滤的完整 `make itest` 全绿；真实开发 MySQL 仍为 `0010/10`，升级计划为 pending=0 / `binary_only`，`-check` 为 ready。真实开发库成功生成 mode 0600 的违规回执，精确记录 `memory_tenant`、`memory_space`、`memory_author_user`、`memory_author_agent` 各 2 次、总计 8 次；没有读取业务内容、修复数据或覆盖持久生产路径。本机仍无 `promtool`，规则只完成结构/YAML/指标契约门禁；目标服务器尚未安装 timer、生成健康回执或接入 Prometheus/Alertmanager，既有两条孤立 Memory 仍待备份后另行授权调查，未提交、推送、部署或操作 UI。

**2026-08-29 租户 MCP 聚合容量保护迭代**：认证后的单 agent 固定窗口之外新增不可关闭的租户聚合预算，默认每 agent 300 次/分钟、每 tenant 合计 3000 次/分钟；先检查 agent 再检查 tenant，使已越界 agent 不继续消耗团队预算。同一 agent 的 token 轮换共享 agent key，同一租户新增 agent 仍共享 tenant key；三类持久 key 都只含带 scope 的 SHA-256，不含 token、用户、租户或 agent 文本。配置校验要求 `CCS_MCP_TENANT_RATE_LIMIT >= CCS_MCP_RATE_LIMIT` 且不超过 100 万；Redis 正常时跨实例共享，故障时沿既有 failover 退回每实例 20,000-key 有界保护。管理员系统页同时展示两层预算并明确不是存储/成员/计费配额；Prometheus 新增无身份标签的 agent limit、tenant limit 与 window 三个配置指标。协议单测证明换 token、换 agent、agent 先拒绝与 429/`Retry-After`，Redis 适配器门禁证明两个独立 client 共享同一 tenant bucket；真实开发 Redis 的两个独立客户端进一步验证第 4 次请求被共同预算拒绝，临时 key 使用唯一前缀并自动过期。最初尝试把跨实例测试放进 MCP 包时被架构门禁正确拒绝，随后拆回“协议层证明 key 语义、适配器/装配层证明共享状态”，没有增加例外。定向 race、`git diff --check`、`make check`（179 个文件、domain 85.3%）与未过滤完整 `make itest` 全绿；真实开发 MySQL 保持 `0010/10`，`-check-upgrade` 为 pending=0 / `binary_only`，`-check` 为 ready。存储、成员与计费配额仍没有可信统一口径，目标多实例上的隔离 agent 压测仍待部署侧验收；未提交、推送、部署或操作 UI。

**2026-08-29 租户逻辑容量基线迭代**：管理员系统页新增按需生成的 tenant-scoped 容量快照，在 3 秒 deadline 内用 repeatable-read read-only 事务聚合成员、Agent、空间、Memory、Card、召回条件、标签、关联、共同线当前位置/历史/交接、embedding、外部镜像和活动审计的精确行数与应用逻辑载荷；adapter 只返回计数和字节，不返回正文、实体或 actor 标识。逻辑字节用 `OCTET_LENGTH` 统计应用 payload，明确排除 InnoDB 行/索引开销、空闲页、binlog、备份和副本，因此是增长与未来配额设计的可信基线，不是物理磁盘、成本或账单用量；当前只观测、不拒绝写入。容量 SQL 不接入 `/metrics`，超时只让卡片降级，不影响核心读写或 readiness。`0011_tenant_capacity_indexes.sql` 为 Memory、Card、召回条件、关联和 embedding 补齐 tenant-leading 索引；开发 MySQL 在确认 51890 无 listener 后完成迁移，5 条 ALTER 共约 381ms，当前 `0011/11`、pending=0、`binary_only`，`-check` 为 ready。真实配置库门禁确认五个索引首列均为 `tenant_id` 且可生成快照；隔离真实 MySQL 用例精确证明分类计数、UTF-8 字节、总量求和和跨租户排除。定向 race、`git diff --check`、`make check`（183 个文件、domain 85.3%）与未过滤完整 `make itest` 全绿；首次完整集成只因两处合成“下一迁移”夹具仍使用 `0011` 失败，顺延为 `0012` 后定向与完整回归通过。没有执行浏览器/页面视觉验收、目标部署、提交或推送；开发库既有 2 条孤立 Memory / 8 次关系违规仍未修复。

**2026-08-29 治理列表有界化迭代**：成员、Token、共享空间和单个空间的成员授权改为按 ULID 倒序的 keyset 分页，空间页一次只展开一个选中空间，不再构造完整的空间数 × 成员数矩阵；概览的 Agent 接入改为 MySQL 分组后最多返回 6 个。Web 使用独立 `UserSummary` / `TokenSummary` 投影，SQL 不选择 `password_hash` / `token_hash`，吊销前通过 tenant + user + token ID 精确校验所有权。`0012_governance_list_indexes.sql` 新增 `(tenant_id,id)`、`(tenant_id,user_id,id)` 与 `(tenant_id,id)` 三条访问路径；开发 MySQL 在只读 `-check-upgrade` 明确 `restore_required` 且确认 51890 无 listener 后完成迁移，当前 `0012/12`、pending=0 / `binary_only`，`-check` ready。领域测试证明插入新行不会让下一页重复/跳行、非法游标 fail closed、个人空间不进入授权面；真实 MySQL 契约与索引门禁证明租户隔离、授权投影、Token 所有权和 Agent 聚合上限。`go test -race ./...`、`make check`（187 个文件、domain 85.5%）、未过滤完整 `make itest` 与 `git diff --check` 全绿；`promtool` 仍因本机未安装跳过。尚未执行目标部署、真实浏览器视觉/业务验收、提交或推送；开发库既有 2 条孤立 Memory / 8 次关系违规仍未修复，复跑 `-check-integrity` 仍精确得到原四类各 2 条。

**2026-08-29 MCP 认证凭据最小化迭代**：MCP bearer token 认证热路径改用专用 `AuthToken` / `AuthUser` / `AuthAgent` / `AuthTenant` 投影，JOIN 只返回认证决策与身份上下文实际需要的字段；`token_hash` 仅作为唯一索引查找谓词，不进入 SELECT 结果，`password_hash` 也不再从数据库读出。SQL 形状门禁锁定两类凭据哈希不得进入投影，反射测试进一步保证认证结构本身无法携带它们；真实 MySQL 契约与 MCP 端到端门禁通过。同轮修正两个共享集成环境的假阴性：MCP 索引验收改为等待唯一租户的持久就绪结果，不再假设目标行都由当前 worker 领取；Redis 固定时窗测试在边界附近先跨入新窗口，并可独立解析项目配置。`go test -race ./...`、`make check`（189 个文件、domain 85.5%）与未过滤完整 `make itest` 全绿；`promtool` 因本机未安装仍显式跳过。当前最远层为结构/领域/SQL 契约 + 真实开发 MySQL/Redis/MCP 端到端；尚未执行目标部署、真实浏览器/业务验收、提交或推送，开发库既有 2 条孤立 Memory / 8 次关系违规未被修复。

**2026-08-31 Web 会话并发吊销迭代**：审计发现改密/管理员重置与并发登录之间存在只靠 `DELETE sessions` 无法封住的窄窗口：旧密码可能先完成校验，重置随后删除已知会话，而旧登录最后才插入一张新会话。`0013_session_auth_version.sql` 为 `users` 与 `sessions` 增加非空、默认 1 的 `BIGINT UNSIGNED auth_version`；改密、重置和账号转为停用都会推进用户版本，同状态幂等更新不重复推进，每张会话绑定签发时版本并在每次恢复时回查比对。领域探针显式在重置的 DELETE 之后重新插入旧版本会话，验证下一请求拒绝并清理；真实 MySQL 用例证明初始 1、重置后 2、停用后 3 且重复停用仍为 3，迁移列形状和并发旧会话拒绝均通过。开发 MySQL 在只读计划明确 `0012→0013`、pending=1 / `restore_required` 且 51890 无 listener 后显式迁移，当前 `0013/13`、pending=0 / `binary_only`、`-check` ready。`go test -race ./...`、`make check`（190 个文件、domain 85.6%）、未过滤完整 `make itest` 与 `git diff --check` 全绿；`promtool` 因本机未安装仍显式跳过。同轮修正架构文档中“外部源 Web 手动重试未实现”的过时口径；实际代码已有可审计持久入队、重复合并与多实例租约领取。当前最远层是领域竞态探针 + 真实 MySQL 版本/幂等/旧会话拒绝 + 全量回归；尚未执行目标部署、真实浏览器/业务验收、提交或推送；开发库既有 2 条孤立 Memory / 8 次关系违规未被修复。

**2026-08-31 Argon2id 渐进升级迭代**：`PasswordHasher` 新增 `NeedsRehash`，PHC 解析先限制 memory、iterations、盐和输出长度，避免畸形存量串在验证前触发无界 Argon2 资源消耗。登录只对 memory、iterations、parallelism、盐长度与输出长度逐项不低于存量且至少一项提高的配置生成新哈希；更强或互有高低的旧哈希不会因运维误配自动降级。写入以旧哈希和 `auth_version` 做 compare-and-swap，不推进认证版本也不撤销现有会话；并发重置/停用导致 CAS 失败后会重新读取并重新验证，旧密码不能覆盖新凭据，另一个实例已经完成等价升级则允许继续登录。领域竞态测试覆盖弱参数升级、并发重置拒绝和多实例良性竞争，真实开发 MySQL 验证成功 CAS 只改派生哈希、认证版本与强制改密标志保持不变，过期哈希或错误版本均无法写入。`go test -race ./...`、`make check`（191 个文件、domain 85.5%）、未过滤完整 `make itest` 与 `git diff --check` 全绿；真实 bge-m3 Card 门禁约 98–113ms、Memory 门禁约 71–94ms，双 worker 门禁约 0.15s / 0.46s，`promtool` 因本机未安装仍显式跳过。开发 MySQL 保持 `0013/13`，`-check-upgrade` 为 pending=0 / `binary_only`，`-check` 为 ready；`-check-integrity` 仍报告既有 2 条孤立 Memory 导致四类规则各 2 次、合计 8 次违规，本轮未修复数据。当前最远层是哈希边界/领域竞态 + 真实开发 MySQL 条件更新 + 全量真实依赖回归；未执行目标部署、浏览器操作、提交或推送。

**2026-08-31 Go 供应链漏洞门禁迭代**：首次用 Go 官方 `govulncheck v1.7.0` 对当前 Go 1.26.3 源码做可达符号扫描，真实命中 9 个标准库漏洞，涉及 `net/url`、`html/template`、`crypto/tls`、`net/http`、`encoding/asn1`、`net/textproto` 与 `crypto/x509`，修复基线集中在 Go 1.26.6。项目、PoC 与贡献文档统一提升到 Go 1.26.6，直接依赖 `golang.org/x/crypto` 从 v0.54.0 升到修复 SSH 问题的 v0.55.0。`make check` 新增 fail-closed `make security`：扫描器版本固定、漏洞库实时读取，库不可达或有可达项均失败；升级后源码为 0 个可达漏洞，仅剩 1 个依赖模块中的不可达通告。发布脚本在交叉编译后再次扫描实际 Linux 二进制，成功归档 `BUILDINFO.txt` 与 `VULNERABILITY_SCAN.txt` 并纳入内层 manifest/外层签名链；不再使用 `-s -w`，因为剥离实测会让二进制扫描丢失调用关系，把仅存在于依赖模块的 SSH/OpenPGP 符号误报为制品可达。路径门禁在联网前拒绝缺失、相对路径与 symlink 二进制，发布测试真实构建 amd64 制品并核对 Go 1.26.6、模块清单和无漏洞回执。Bash syntax、ShellCheck、`go test -race ./...`、`make check`（191 个文件、domain 85.5%）、未过滤完整 `make itest` 与 `git diff --check` 全绿；真实 bge-m3 Card 门禁约 89–112ms、Memory 门禁约 74–100ms，双 worker 门禁约 0.13s / 0.50s，`promtool` 仍因本机未安装跳过。`-version` 确认 go1.26.6，开发 MySQL 保持 `0013/13`、pending=0 / `binary_only`、`-check` ready；既有 2 条孤立 Memory / 8 次关系违规未变化。当前最远层是官方实时源码扫描 + 最终 Linux 制品扫描/签名清单绑定 + 全量真实依赖回归；扫描回执仍是构建时快照，未执行目标部署、浏览器操作、提交或推送。

**2026-08-31 第三方许可证分发迭代**：此前发布包只有项目自身的 Apache-2.0 `LICENSE` 与模块构建信息，不能完整覆盖运行时依赖的再分发材料。新增固定 `google/go-licenses v2.0.1` 的 fail-closed 门禁，分别在 `linux/amd64`、`linux/arm64`、`CGO_ENABLED=0` 条件下检查实际服务入口；未知或不允许的许可证直接失败，当前工具报告的非 Go 文件只有纯汇编 `.s`，脚本会拒绝新出现的 C、对象文件或其他原生材料，要求人工复核。当前 16 个运行时库均被识别，其中 `github.com/go-sql-driver/mysql v1.10.0` 为 MPL-2.0；每个 Linux 发布包现包含 `THIRD_PARTY_LICENSES.csv`、不泄露构建机绝对路径的 `THIRD_PARTY_REVIEW.txt`、依赖许可证/NOTICE，以及 MySQL 驱动的完整对应源码，并全部进入内层 manifest 与外层 checksum/签名链。自动分类与收集是构建证据，不代替法律意见；新增 reciprocal、restricted、unknown 或 native 依赖仍需人工检查。路径测试证明非法架构、相对目录和 symlink stage 会在安装工具/联网前被拒绝，真实 amd64 打包测试核对报告、MPL 标识、许可证和源码；`make licenses`、`make check`（191 个文件、domain 85.5%）、未过滤完整 `make itest`、`go test -race ./...`、ShellCheck、Bash syntax 与 `git diff --check` 全绿。集成回归同时暴露并修正了活动统计测试在 UTC 午夜附近错误假设 `now-1h` 与 `now-2h` 同日的时区边界，现按实际事件 UTC 日动态核对。开发 MySQL 仍为 `0013/13`、pending=0 / `binary_only`、`-check` ready，既有 2 条孤立 Memory 仍造成四类规则各 2 次、合计 8 次违规，本轮未修复数据。当前最远层是双架构静态许可证门禁 + 真实 Linux amd64 分发包材料/清单验证 + 本地真实依赖回归；未执行目标部署、浏览器操作、人工法律复核、提交或推送。

**2026-08-31 CycloneDX SBOM 迭代**：此前 `BUILDINFO.txt` 可供人工查看模块，但发布包没有标准机器可读组件清单。新增固定 `cyclonedx-gomod v1.10.0` 的 binary 模式，为 linux/amd64 与 linux/arm64 最终制品生成 CycloneDX 1.6 `SBOM.cdx.json` 和有界 `SBOM_REVIEW.txt`。首次探针真实发现工具若不显式设置目标环境，会把发布机的 `darwin/arm64` 写进 Linux 二进制 PURL；生成脚本现在固定 `GOOS=linux`、实际 `GOARCH` 与 `CGO_ENABLED=0`，独立 Go 校验器直接读取目标二进制 build info，不执行它，逐项核对主组件版本、精确二进制 SHA-256、15 个链接模块、Go 1.26.6 标准库、目标 PURL、依赖图与 composition。随机 serial/timestamp、构建机 `/Users`/`/tmp` 路径、组件缺失/多出、错误架构或哈希漂移都会阻断归档。两个文件已进入内层 manifest 和外层 checksum/Ed25519 签名链；SBOM 只是一份制品库存快照，不替代实时漏洞扫描、许可证义务判断、可复现构建或发布者认证。路径测试证明非法架构、相对路径、symlink 和 stage 外二进制会在工具安装/联网前失败；真实签名 amd64 发布包独立核对 MySQL/MCP/标准库组件、PURL、SHA 属性，并篡改架构证明校验器拒绝。`make check`（191 个文件、domain 85.5%）、未过滤完整 `make itest`、`go test -race ./...`、双架构 `make sbom`、ShellCheck、Bash syntax 与 `git diff --check` 全绿；真实 bge-m3 Card 门禁约 97–116ms、Memory 门禁约 87–95ms，双 worker 门禁约 0.15s / 0.44s。开发 MySQL 仍为 `0013/13`、pending=0 / `binary_only`、`-check` ready；既有 2 条孤立 Memory / 8 次关系违规未变化，本轮未修复数据。当前最远层是目标二进制级双架构 SBOM 门禁 + 真实 Linux amd64 发布包清单/签名绑定 + 本地真实依赖全量回归；`promtool` 仍未安装，且未执行目标部署、浏览器操作、正式发布、提交或推送。

**2026-08-31 Prometheus 规则真实门禁迭代**：此前 Go 测试只在系统恰好安装 `promtool` 时真实解析规则，本机长期缺失导致该层反复 skip，只剩字符串契约与 Ruby YAML 结构证据。新增 `check-prometheus-rules.sh`：通过 Go 校验和数据库固定 `github.com/prometheus/prometheus@v0.314.0` 的模块哈希与 release revision，在 `GOWORK=off` 下用 Go 1.26.6 构建 Prometheus 3.14.0 `promtool`；直接 `go install module@version` 因上游 `go.mod` 的 replace 指令被 Go 正确拒绝，没有绕过该保护。`make monitoring`、`make check` 和正式打包现在都会无条件执行 `promtool check rules`，确认当前 22 条规则；新增随包 `claracore-server.rules.test.yml`，由 `promtool test rules` 实际验证 4 个低噪声场景、7 个 firing/抑制断言：target down 与 draining、备份 missing 与 stale、外部 failed 与 stale、活动 claim 与 pending backlog 均不会重复报警。`PROMETHEUS_RULES_CHECK.txt` 绑定工具/模块校验和/revision、Go 工具链、规则 SHA、测试 SHA、22 条规则与单元结果，并进入内层 manifest 和外层 checksum/Ed25519 签名链；真实 amd64 发布测试独立重算两份 SHA。相对目录、symlink stage 或缺规则文件会在下载/构建工具前失败；同名签名旁的未签名覆盖也前移到任何编译/下载前拒绝，避免空耗完整发布链。`make check`（191 个文件、domain 85.5%）、未过滤完整 `make itest`、`go test -race ./...`、ShellCheck、Bash syntax 与 `git diff --check` 全绿；真实 bge-m3 Card 门禁约 106–125ms、Memory 门禁约 88–104ms，双 worker 门禁约 0.14s / 0.59s。开发 MySQL 保持 `0013/13`、pending=0 / `binary_only`、`-check` ready，既有 2 条孤立 Memory / 8 次关系违规未变化，本轮未修复数据。当前最远层从“YAML/字符串结构”提升为固定官方 CLI 解析 + PromQL 时间/抑制行为 + 发布包 SHA 回执；仍未在目标监控主机用其实际版本 `promtool`、reload Prometheus、制造 pending/firing 或验证 Alertmanager 通知，未部署、操作浏览器、提交或推送。

**2026-08-31 确定性发布归档迭代**：此前发布内容和内外层哈希都受校验，但最终仍由宿主机 `tar -czf` 写入 staging mtime 等元数据；相同文件集合在不同打包时刻可能产生不同归档字节和签名。新增仓库内 `scripts/release-archive` 写入器并接入 `package-release.sh`：只接受绝对普通目录，预检整棵树并拒绝根/树内符号链接与特殊文件；按归档路径字典序使用 PAX tar，固定目录/可执行/普通文件权限为 0755/0755/0644，清零 uid/gid 与用户名/组名，统一 Unix epoch tar 时间，并固定 gzip BestCompression、空 name/comment、零时间与 OS=255。单元门禁覆盖长 PAX 路径、顺序、owner、权限、tar/gzip 时间、相对目录和 symlink 拒绝；真实签名 amd64 发布测试在解包并通过内层 manifest 后，用同一写入器重新装配并与原 `.tar.gz` 逐字节比较，再把整棵解包目录 mtime 改到 2040 年/不同时区，仍复现完全相同字节。`make check`（191 个文件、domain 85.5%）、未过滤完整 `make itest`、`go test -race ./...`、Bash syntax、ShellCheck 与 `git diff --check` 全绿；真实 bge-m3 Card 门禁约 99–109ms、Memory 门禁约 84–90ms，双 worker 门禁约 0.13s / 0.49s。开发 MySQL 保持 `0013/13`、pending=0 / `binary_only`、`-check` ready，既有 2 条孤立 Memory / 8 次关系违规未变化，本轮未修复数据。当前最远层是“相同 staging 目录名、文件内容和可执行位 → 相同归档字节”的本地自动证据；它不证明不同源码/工具链/实时漏洞库能够跨主机产出相同二进制或同一完整发布内容，也不是 SLSA 声明。未部署、操作浏览器、提交或推送。

**2026-08-31 发布 provenance 迭代**：每个 Linux 发布包新增 `SOURCE_MANIFEST.sha256`、`BUILD_INPUTS.sha256` 与 `BUILD_PROVENANCE.intoto.json`。源码清单展开并精确复现已通过 ldflags 链入二进制的 sourceDigest；输入清单绑定发布脚本、各供应链门禁、确定性归档器、部署模板和发布文档。provenance 使用标准 in-toto Statement v1 / SLSA provenance v1 predicate：subject 是最终 `claracore-server` 字节，buildDefinition 固定版本、linux/架构、Go 1.26.6、CGO disabled、trimpath、sourceDigest 和两份输入清单，local builder 的 7 个 byproduct 分别绑定 BUILDINFO、漏洞、许可证、SBOM 与 Prometheus 回执。生成路径严格限制绝对普通目录/文件、release 内部路径、大小、manifest 行格式/顺序/重复和 JSON 未知/尾随字段，不记录 invocation ID、时间或构建机绝对路径；新 `-verify-provenance` 由同一发布二进制在加载 `.env`、MySQL、Redis 或 Embedding 前离线重算全部内容。自动门禁既用真实签名 amd64 包运行独立 validator、篡改 subject SHA 证明拒绝，也真实构建当前平台发布二进制并执行其内嵌 CLI 得到纯净 verified JSON；provenance 仍随确定性归档逐字节复现并进入 inner manifest / outer checksum / Ed25519 签名链。`make check`（193 个文件、domain 85.5%）、未过滤完整 `make itest`、`go test -race ./...`、Bash syntax、ShellCheck 与 `git diff --check` 全绿；真实 bge-m3 Card 门禁约 109–124ms、Memory 门禁约 86–98ms，双 worker 门禁约 0.16s / 0.52s。开发 MySQL 保持 `0013/13`、pending=0 / `binary_only`、`-check` ready，既有 2 条孤立 Memory / 8 次关系违规未变化，本轮未修复数据。该 statement 由已认证归档间接认证而非 DSSE envelope，本地 builder 的信任边界仍包含维护主机、OS、网络、实时漏洞库、工具链与操作者；没有托管或加固 builder 证据，因此不声明任何 SLSA Build Level。未部署、操作浏览器、提交或推送。

**2026-08-31 有界可复现构建迭代**：正式 Linux amd64/arm64 二进制现在由固定 Go 1.26.6、`CGO_ENABLED=0`、`-trimpath`、`-buildvcs=false` 构建两次；两个新建且分离的 `GOCACHE` 同时清空 GOENV/GOFLAGS/workspace 并禁用外部 cache program，字节不一致即发布失败。匹配后生成无时间、调用 ID 或宿主路径的 `BUILD_REPRODUCIBILITY.json`，其 schema/范围/版本/sourceDigest/目标/构建设置及 primary/rebuild SHA 会被独立校验器重算，并作为第 8 份 provenance byproduct 进入 inner manifest、确定性归档、outer checksum 与可选 Ed25519 签名链。同一二进制的 `-verify-provenance` 在配置/依赖加载前同时验证该回执。构建前还固定 `SOURCE_MANIFEST.sha256` 与 `BUILD_INPUTS.sha256`；全部长耗时门禁结束后逐文件复算源码，provenance 生成时重算发布脚本、门禁、模板和文档输入，发布期间任一漂移都会 fail closed，避免给混合调用生成声明。单元负例覆盖二进制字节不一致、VCS stamp、篡改回执、输入快照漂移、符号链接与路径边界；真实签名 Linux amd64 发布包实际双编译、独立验证并篡改 SHA 拒绝，最终精确代码的 `make check` 全绿（195 个文件、domain 85.5%、0 个可达漏洞、双架构许可证/SBOM、22 条 Prometheus 规则）、未过滤 `make itest` 全绿（真实包约 48 秒，Card 约 93–110ms、Memory 约 72–97ms、双 worker 约 0.14s/0.52s）、`go test -race ./...` 全绿（buildrepro 约 42 秒、test 约 72 秒），Bash syntax、ShellCheck 与 diff 检查通过。这个回执只证明同一维护主机、同一工具链/模块缓存与同一输入下，分离空 build cache 的重复构建；它不是 hermetic、独立重建者、跨主机 reproducibility、托管/加固 builder、DSSE 或 SLSA Build Level 证据。本轮未部署、操作浏览器、修复既有数据、提交或推送。

**2026-08-31 开发数据基线备份与隔离恢复证据**：对当前 `0013/13` 开发库生成新的原子逻辑备份三件套 `claracore-server_claracore_server_20260831T061639Z.sql.gz{,.sha256,.metadata}`，归档 SHA-256 为 `ed384cac80fb6a0327526fc7c990a2213996385b5afb078404e63e97cc956f51`，bundle verifier 确认 checksum、metadata、gzip 与新鲜度。备份在无 host 端口、独立 internal Docker network 的一次性 MySQL 8 容器中真实导入，23 张表逐表 `CHECK TABLE QUICK` 和精确行数通过，迁移账本为 13 条、最新 `0013`；不可覆盖报告 `claracore-server_claracore_server_20260831T061639Z.sql.gz.restore-report.tsv` 为 0600，SHA-256 为 `67fd136ebb3e311269ee59a6836a2dd876976fe61571d322d37ff6ccd8a3ac4f`。演练首次暴露 `mysql --batch` 会转义 `CONCAT` 内制表符、导致 migration 回执解析失败；`rehearse-mysql-restore.sh` 已为该查询增加 `--raw`，fake MySQL 同步模拟真实转义作为回归门禁。第二次隔离恢复通过并自动删除临时库；另一次保留式验证只在数据库内计算两条孤立 Memory 的完整行哈希，源库与恢复库逐条一致、正文未输出，随后显式删除并确认目标库不存在。精确修复后的 `make check`（195 个文件、domain 85.5%、0 个可达漏洞、双架构许可证/SBOM、22 条 Prometheus 规则）、未过滤 `make itest`、`go test -race ./...`、`git diff --check`、`-check-upgrade` 与 `-check` 全绿；`-check-integrity` 仍准确报告两条孤立 Memory 造成四类规则各 2 次、合计 8 次违规，数据尚未修复。本证据是同一物理主机上独立容器的技术可恢复性与精确行回收证明，不是异机灾备、RPO/RTO、生产部署或业务验收；未提交、推送、部署或操作 UI。

**2026-08-31 P1 开发数据完整性基线清理**：在用户明确授权仅删除这两条开发库 Memory 后，删除前重新验证上述备份 checksum/gzip、真实隔离恢复、两条完整行哈希逐条一致与隔离目标清理；同一 serializable 事务内对候选行加锁，再次要求它们同时命中 `memory_tenant`、`memory_space`、`memory_author_user`、`memory_author_agent` 四类孤立条件，并确认 audit、label、embedding、双向 link 以及 Memory/Card `copied_from` 依赖均为 0。只按两个精确 ID 执行一次删除，`RowsAffected=2`，Memory 总行数从 13 变为 11，事务内四条件孤立数从 2 变为 0；没有更新或删除其他表。提交后 `-check-integrity` 在 repeatable-read read-only 快照上通过全部 29 条规则，`violations=0`、status=`verified`；`-check-upgrade` 仍为 `0013/13`、pending=0 / `binary_only`，`-check` 为 ready。临时测试、隔离数据库、容器、网络和远端临时目录均已清理；本轮没有部署、浏览器/UI、业务验收、提交或推送。该证据只关闭开发数据完整性基线 P1，不代表目标环境灾备或生产验收。

**2026-08-31 P2 签名制品与目标主机离线预检**：当前仓库与 `origin/main` 都为 `ec52af0809dab584984f64ac35a56a64ff7e32a3`，尚无本地/远端 tag；在 `/secure/offline`、ClaraCore 配置/受控候选目录和仓库范围内未发现生产 Ed25519 私钥，因此没有冒充生产信任根。使用一次性 0600 Ed25519 测试密钥对精确当前源码生成 `p2-preflight-ec52af0` Linux amd64 包，正式打包链通过双编译有界复现、实时漏洞扫描、许可证、CycloneDX SBOM、22 条 Prometheus 规则、8 份 provenance byproduct、确定性归档、外层 SHA 与 64-byte detached signature；归档 SHA-256 为 `fd663682999fedf514613554b61c296c85d01ba930bcd782639745dd3d783ee3`，大小 14,909,293 bytes。包在目标主机临时目录重新通过签名认证、99 文件 inner manifest、provenance 与 `-version`：sourceDigest=`560af249a529120038b43e66f5a1dac326c04b5c0222d24f406e9d279d3b832f`，binaryDigest/provenance subject=`b721cc76e67e0f927ee26f28a950b878a0a6edd7dee861965857ba43fc66ce87`，Go 1.26.6、linux/amd64。随后重新构建并逐字节得到同一 binaryDigest，把该精确二进制放入目标主机临时目录、加载目标现有 `.env` 做切换前只读检查：`-check-upgrade` 为 `0013/13`、pending=0 / `binary_only`，`-check` 为 ready，`-check-integrity` 在不发布状态文件的 repeatable-read 快照上通过 29 条规则且 `violations=0`。两轮预检前后 `current` 均为旧 `dev-20260828-ui-completion-v1` release，Supervisor PID 均为 2810349 且 RUNNING；没有切换、重启或部署，临时私钥、制品、二进制和目标目录均已删除。目标主机有 220,029,832 KiB 可用空间及 openssl/sha256/tar/gzip/curl/systemctl，缺少 jq 但现有验证脚本不依赖它；当前 `51891/healthz=200`、`livez=404`、`readyz=404`，继续证明运行实例仍是旧版本。维护者随后授权正式版本 `0.2.0-rc1`、提交本交接文档并在部署前备份、保留旧 release 回滚入口后部署到 `192.168.3.200:51891`；但签名私钥仍填写为占位符 `<绝对路径>`，该路径不存在，也没有授权生成生产密钥，因此正式签名与部署继续停在密钥边界。本预检不是生产签名、发布、部署或业务验收。

### 这一层还缺的东西

- **审核队列的安全批量能力已完成**。只支持把用户逐项勾选的 1–50 张 draft 原子标记为 deprecated；逐项校验租户、可写空间、draft 状态和版本，任何冲突整批回滚。没有批量发布，理解与召回条件仍需逐张补齐。
- **内容浏览全文查找已实现**。它扩展治理列表 SQL，在当前类型/空间/状态内筛标题与正文，保留 draft / deprecated 等状态和 keyset 分页；不复用 agent 的语义检索，避免候选截断与默认 active 状态破坏治理列表。
- **真实演示数据已有近重复知识卡**。自动上下文会因 top-2 margin 不足而安全放弃；审核与浏览详情现在复用现有两阶段检索展示 active 重复候选，精确内容无 embedding 也能识别，高相似语义候选阈值为 0.90。系统不自动合并或删除，人工比较后沿用 `deprecated` 保留历史；不能靠降低 Stage B 阈值掩盖数据质量问题。
- **过期 Web session 已有独立 worker**：启动即清一次，之后每日清理；领域层仍负责过期判断与删除语义。
- 项目空间与权限管理页已经实现；后续缺口只按真实使用反馈补充，不再把它列为待做。

### Web 视觉设计节点（2026-08-11 已验收，2026-08-27 已同步 SSR）

- Figma 文件：[ClaraCore Server — Product Direction 01](https://www.figma.com/design/6ObFIgv8rQVrZnEX5WdaMr)。
- `03 · Web Workspace` 是工作台基准；生产页是 `04 · Activity Feed`、`05 · Card Review`、`06 · Content Browse`；`07 · Responsive & States` 覆盖 390px 窄屏与加载、空、错误、焦点等状态。`00`–`02` 只保留方向过程与被否决方案。
- 这是 **Web 治理工作台**，不是独立桌面 App：1440 桌面视口、持久侧栏、顶栏上下文、页面级标题/动作、筛选与表格是基本骨架。
- 保留现有温暖、可信、低装饰的色彩语气；不要恢复巨型编辑式标题、居中舞台、三张等权大卡，也不要套紫色 AI 渐变或通用 SaaS 圆角卡片墙。
- 设计系统现有 3 个变量集合、61 个变量、9 个文字样式、1 个效果样式；14 个组件/组件集共 61 个形态，覆盖按钮、表单、导航、分页、表格行、反馈状态与业务列表项。
- 结构验收已过：生产页与组件没有默认命名、占位文案或未绑定填充/描边；统一 Noto Sans SC；390px 根容器无横向溢出；交互控件有焦点态，状态不只靠颜色；关键对比度不低于 5.0:1。
- 生产视觉已同步进 SSR；后续视觉调整必须来自 1440/390px 的真实部署验收回执，不继续凭空探索方向。Code Connect 等到代码组件与 Figma 组件稳定一一对应后再接。

---

## 缺口总览（接手前先看这张表）

各层的详细说明在各自的「已固化的决定」段里，这里是一张能一眼扫完的清单。

| 缺口 | 影响 | 在哪一段 |
|---|---|---|
| **多实例尚未部署侧验收** | 代码侧已有唯一实例标签、正式发布源码摘要、实际二进制摘要、逐实例版本/源码/制品/环境/schema 校验和公开入口覆盖采样；双 worker + 真实 MySQL 门禁覆盖 Redis 租约、故障 fail-open 与幂等写回，单进程 SIGTERM 摘流已实测。仍需两台 Linux、真实负载均衡摘流、网络分区和滚动发布验证 | §检索层、`07-deployment.md` |
| **Shared Line 不做语义猜测** | 自动上下文已覆盖 Memory / Card 单证据竞争；Shared Line 需调用方明确 `lineId`，避免多线 scope 猜错 | `08-usage-guide.md` §4、§8 |
| **外部源连接（odoc 等）** | 第二版。`01` §8.1.3 定的是「活系统连接、死文档纳入」 | `01` §8 |
| **反馈覆盖可验证，质量仍是主动样本** | `context_quality_status` 已有 30 天选择分母与去重关联覆盖，但 outcome 由调用方主动提交，仍不能解释成独立准确率 | `08-usage-guide.md` §8 |
| **自动质量样本仍需扩充** | 真实 embedding + Stage B 已有正负与延迟门禁，但样本量仍小，需结合 wrong/ignored 反馈演进 | `08-usage-guide.md` §8 |
| **开源分发材料已自动化** | 项目采用 Apache License 2.0；Linux 双架构 CycloneDX 1.6 SBOM、依赖许可证检查、第三方报告、许可证/NOTICE 与 MPL-2.0 MySQL 驱动源码进入发布清单。SBOM 和自动分类不替代漏洞扫描、发布者认证或新 reciprocal/restricted/native 依赖人工复核 | 根目录 README / LICENSE、`07-deployment.md` |
| **公开生产与依赖 TLS 尚未验收** | 公网模板已安全默认 HTTPS，非 loopback MySQL/Redis/Embedding/Atlas 也会 fail-closed；但当前团队实例仍是内网 HTTP，且尚未用目标私有 CA/mTLS 与真实远程依赖验证证书、轮换和失败行为 | `07-deployment.md` |
| **目标侧备份调度与真实恢复尚未验收** | 原子产物、定时/补跑模板、新鲜度/篡改门禁、技术恢复回执、系统页/指标/告警与发布包已自动验证；仍需目标侧安装 timer、生成一次备份，在独立 MySQL 生成真实回执/报告，并人工抽查 Memory / Card / Shared Line | `07-deployment.md` §3 |

---

## 检索层剩余的东西（详）

- **backfill 与失败观测已完成**。`/admin/indexing` 可查看状态、最近失败并触发补索引/失败重试；真实 MySQL 的租户隔离和重试路径有集成测试。
- **两级 embedding 缓存已完成**。进程内有界 LRU 使用 `hash(model+text)`，TTL 与容量可配；配置 Redis 后相同 key 可跨实例与重启复用，Redis 读写失败或损坏值按 miss 快速降级。`server_status` 可看本进程累计 hits/misses，系统页显示 Redis 实时连通性。
- **检索结果不带 labels**。粗筛刻意不填（N+1 或让候选集膨胀的 JOIN），按 ID 取单条时才有。MCP 工具编码时注意这一点。

---

## 下一步

**0. 当前工作流：P2 可信签名制品与目标实例部署。** P0 发布候选已固化并推送，P1 开发数据完整性基线已清零，`0.2.0-rc1` 的正式签名和目标实例部署也已获授权；唯一未决权限是生产 Ed25519 私钥的真实绝对路径，或在明确受控目录生成新密钥的授权。拿到该边界后，先生成并验证 Linux amd64 包的签名、checksum、inner manifest、SBOM、许可证、漏洞、provenance 与有界可复现构建回执，再于切换前生成新的数据库备份、保留旧 release 回滚入口并使 `192.168.3.200:51891` 与仓库候选一致。部署后只把 live/ready/health、MCP、版本/source/binary/schema、metrics、备份/恢复与完整性回执记为运行证据；真实身份页面、1440/390px、键盘、高风险确认和完整业务链仍由用户验收。在生产密钥边界明确前不签名或部署，也不把本地或开发库验证写成生产/业务验收。

Continuity、项目空间权限、Web 摄取、删除检测、embedding backfill/失败观测、部署包、Supervisor 与 Nginx 代理均已完成。使用与验收方法统一见 `docs/08-usage-guide.md`；下一轮只按人工验收回执或缺口总览中的真实需求继续，不再根据旧文档重复建设已完成能力。

---

## 铁律（破坏即回退，不加例外）

1. **依赖方向单向**：`domain` 不依赖 `adapters`，`adapters` 反向实现 `domain` 定义的接口。
2. **接口在消费方定义**：涉及领域类型的 `Store` 接口写在各 `domain` 包内；`ports/` 只放不认识领域概念的纯基础设施接口（Cache/Locker/Embedder）。**原设计把 Store 放 ports 会造成循环 import，已修正。**
3. **两个 Host 不互相依赖**，也不直接碰 `adapters`。
4. **身份只来自 token**。绝不读客户端声明的 header——Desktop 的 `X-ClaraCore-Agent-ID` 是可伪造的，不可沿用。
5. **时间一律 UTC**，列类型 `DATETIME(3)`，领域层不直接调 `time.Now()`（用 `platform.Clock`）。
6. **作用域字段无默认值**，缺失即报错。
7. **零 SQL 触发器**，逻辑全在应用层。
8. **发现结构被破坏时修结构，不加豁免**。第一个 `// nolint` 是腐化的起点。
9. **过度设计也是债**：为已知变化点留接口，为未知的不留。只有一个实现且看不到第二个的，直接写具体类型。

---

## 已知的坑

| 坑 | 说明 |
|---|---|
| `2026-07-28` 协商不上 | 不是 bug。该版本废弃了 `initialize` 握手，走 initialize 的客户端上限就是 `2025-11-25`。服务端 SDK 已就绪，无需处理 |
| stateless 下 GET/DELETE 返回 405 | 符合预期，没有 SSE 长连接。nginx **不需要** `proxy_read_timeout 3600s`，但 POST 响应体仍是 event-stream，**仍需** `proxy_buffering off` |
| tool handler 里读不到真实协议版本 | stateless 下 `InitializeParams` 是 SDK 默认值（`2025-03-26`）。业务逻辑不得依赖它 |
| `auth.RequireBearerToken` 默认要求 `Expiration` 非零 | token 必须有过期时间，否则显式设 `AllowMissingExpiration` |
| 中文全文检索 | `FULLTEXT` 必须带 `WITH PARSER ngram`，否则中文不分词 |
| Redis 不要用 db index | 用 key 前缀 `ccs:`。Cluster 不支持多 DB，`SELECT n` 将来会废 |
| `RowsAffected() == 0` ≠ 行不存在 | 驱动默认不开 `clientFoundRows`，UPDATE 写同值时返回 0。别拿它当存在性判断 |
| upsert 的 `VALUES()` 已废弃 | MySQL 8.0.20+ 用行别名：`INSERT ... AS v ON DUPLICATE KEY UPDATE c = v.c`。库是 8.0.36，可用 |
| InnoDB 的 FULLTEXT 索引在**提交时**才更新 | 同事务内 `MATCH` 查不到自己刚插入的行。要验证全文检索就必须真提交并自己清理（`TestChineseFullTextSearchTokenizes` 就是这么做的） |
| `make itest` 曾用 `-run` 白名单 | 新增的集成测试因为没被写进名单而**静默不跑**，全绿骗了一次（2026-08-07）。已改成跑整个包，别再加回过滤 |
| 多态引用（labels / links）无外键 | 引用完整性由领域层保证——`Service.requireVisible` 是唯一的落点。硬删 memory/card 会留下悬空引用，所以 `memoria_delete` 是软删 |
| `information_schema.STATISTICS.COMMENT` 查不到全文解析器 | 8.0.36 上那一列是空串。要确认 `WITH PARSER ngram` 生效只能读 `SHOW CREATE TABLE`（2026-08-10，第一版检查因此误报了三张正确的表）|
| ODKU 里赋值**从左到右依次生效** | `ON DUPLICATE KEY UPDATE` 中等号右边引用的是赋值那一刻的值。`content_hash = v.content_hash` 必须放最后一行，提前一行就会让所有「内容变了没有」的比较退化成恒等式——不报错，只是每次入队都白算一次向量 |
| Ollama 的 `/api/embeddings` 一次只能算一段 | 字段是 `prompt` 不是 `input`，一批 32 条就是 32 次往返。用 `/api/embed`（批量），代价是要求 Ollama 版本够新；不存在时适配器会明确报 404 并提示查版本与 BaseURL |
| embedding 服务的错误会进数据库并被展示 | `last_error` 列显示在 Web 治理面上。适配器**不把底层 error 包进去**——Go 的 http 错误带完整内网 URL。排查连接问题要看服务日志，这是有意的取舍 |
| 真实模型的余弦挤在窄带里 | bge-m3 实测相关与不相关的差距只有 0.01 量级，不是铺满 0~1。`scoreTieEpsilon = 0.01` 相当于「一个身位」而不是「几乎相等」。要动它先攒一组带标注的召回评估集 |

---

## 已拍板（**目前没有未决问题**）

2026-08-06 一次性清空了全部 16 条未决项，2026-08-07 落地核心域时补了三条（Card 三态、域间不 import、embeddings 延后）。各文档末尾的「待确认」清单已同步勾选，下面是速查；每条的完整理由在对应文档里。

**权限与内容模型**

| 决定 | 口径 | 出处 |
|---|---|---|
| 管理员分级 | **单一 admin**，`role` 只有 admin / member。空间内部不再分级 | `04` §6 |
| 项目空间成员 | **显式加入**，不预置授权行 | `02` §9 |
| promote 权限 | **任意有目标空间写权限者**。质量用 Card 的 `status` 挡，不用权限挡 | `01` §11.2 |
| promote 校验点 | **两端都校**：源可读 + 目标可写。只校目标 = 拿到 ID 就能把无权读的内容搬进自己能读的空间 | `03` §6 |
| Card `status` | **三态 `draft` / `active` / `deprecated`**，按来源分：手写默认 `active`（作者自管），摄取一律 `draft`（不进默认检索）。0807 落地时砍掉了原设计的 `verifying` / `confirmed`——既然没有独立审核实体，就没有任何机制推动它们流转 | `02` §3.2 |
| 审核 draft | **能写就能审**，含摄取发起人自己。无独立审核实体 | `04` §6 |
| recall_triggers | **只给 Card，单态表**（持 `card_id`）。Memory 不给 | `02` §9 |
| Records 时序 | **不进团队版**，且在 `03` §5 对照表里显式标注缺口 | `03` §5/§6 |

**检索与 Web**

| 决定 | 口径 | 出处 |
|---|---|---|
| `memoria_search` 与 Card | **分离，但返回「另有 N 条卡片命中」的计数** | `03` §6 |
| 空间权重 | **只做 tie-break，不可配**。相关性优先，分数接近才按 team > project > personal | `01` §11.2 |
| `card_recall` 自动注入 | **二期**。第一版显式调用，先跑出命中率数据 | `03` §6 |
| 摄取部分失败 | **保留成功项 + 失败清单**。前提：**摄取必须幂等**，重试即整批重跑 | `04` §6 |
| trace 保留期 | **拆两类**：治理审计长期保留，调用 trace 30 天滚动删除。不可配 | `04` §6 |
| 团队近期动态 | **只展示共享空间的卡片**，不含 memory，个人空间一律不进 | `04` §6 |

**工程**

| 决定 | 口径 | 出处 |
|---|---|---|
| SQL 方式 | **手写 `database/sql`** | `05` §7 |
| 迁移工具 | **自实现**（`cmd/server -migrate`） | `05` §8 |
| 适配器测试 | **真实 MySQL + `integration` tag**，不用 testcontainers / SQLite | `05` §6.4 |
| sqlite 适配器 | **不做**。本地用 Docker 跑 MySQL | `05` §7 |
| `domain/` 覆盖率门槛 | **85%，低于即 CI 失败**。只卡 domain | `05` §7 |
| 唯一键冲突 | **加 `ErrAlreadyExists`**，adapters 识别 MySQL 1062 后映射。governance 与 knowledge **各有一个哨兵、各有一个 `writeErr`**，不合并成带参数的通用函数——传错了不会有编译错误，只会某天返回一个属于另一个域的错误 | 本文 |
| knowledge 与 governance 的关系 | **不 import，走应用层编排**。权限解析成空间 ID 集合后作为入参传入（2026-08-07） | 本文 §核心域 |
| embeddings 表 | **已建**（0003，2026-08-10）。留到这时才建是对的：落地时改掉了草案三处——vector 可空、增加 source_text、不建 idx_emb_hash | `migrations/0003` |
| embedding 时机 | **纯异步**（2026-08-10）。写入事务里落一行 pending，后台 worker 算。写入路径永不依赖外部服务 | 本文 §检索层 |
| 语义与全文的关系 | **分组不通约**（2026-08-10）。两种分数不是一个量纲，不加权融合；每条命中标 `Semantic`，结果带 `SemanticAvailable` | 本文 §检索层 |
| 并行精算 | **不做**（2026-08-10）。更正 `02` §5.1：Desktop 那个病是 JS 事件循环被独占，Go 里每请求一个 goroutine 已经解决 | 本文 §检索层 |

### 拍板带出的两笔代码欠账 —— **2026-08-06 已还清**

- [x] **覆盖率门槛进了 `make check`**：新增 `make cover`，跑 `./internal/domain/...`，低于 `COVERAGE_MIN`（85）退出码非零。当前 87.1%。验证过它真会红（门槛提到 95 会红；移走 `space_test.go` 让覆盖率掉到 71.5% 也会红）。
- [x] **`ErrAlreadyExists` 已落地**：`governance` 加哨兵，`adapters/mysql` 的 `writeErr` 识别 MySQL 1062 后翻译，五种冲突（第二个个人空间 / 第二个团队空间 / 账号重名 / agent slug 重复 / token hash 重复）都有集成测试。**领域层的 fake store 同步加了对应的唯一性检查**——它是行为基准，原先重名时 map 直接赋值会静默顶掉原用户，而真库会拒绝，那种 fake 会让领域测试跑在数据库不可能达到的状态上。

---

## 项目背景速记

- 这是**团队版**，与 `apps/claracore-desktop`（个人版）是**两条独立产品线**。desktop 继续正常演进，不冻结不废弃。
- 两边**共享契约不共享代码**：MCP 工具名/参数/语义、数据模型概念保持一致（`03-mcp-contract.md` 是共同基准）。
- **InnerLife 不进团队版**：企业场景不需要，且 stateless 下服务端无法主动推送。
- 产品定位：**让团队的踩坑经验和工程约定，能被所有人的 agent 在正确的时机自动召回**。核心载体是知识卡片（Card），不是零散记忆。
- Card 是**独立一等公民**，不是 Memory 的子类型。它的「未来什么时候该想起它」是作者显式写下的结构化召回条件，每条单独 embedding——这是本产品的核心技术优势。
- **冷启动是生死问题**：团队已有存量知识（odoc、Atlas、各类 md），摄取能力是第一版核心，不是次要运维功能。
