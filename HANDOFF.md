# ClaraCore Server — 接手指南

> 最后更新：2026-08-28
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

**活动流与完整审计已落地到代码**：`0006_audit_logs.sql`、独立的 `domain/activity` 与 MySQL Store、24 个 MCP 工具的统一装饰器、Web 写中间件、`/activity`，以及需要二次确认且先写 meta-audit 才能查询的 `/admin/audit`。Shared Line 审计只保留 line ID、版本和计数，不落摘要、下一步或交接正文；自动上下文反馈只记录卡 ID 与 outcome，并可按租户汇总近 30 天 outcome。

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
curl http://127.0.0.1:51890/healthz
```

`.env` 已由毛仔创建（含真实密码，已 gitignore，**不要读取、不要复制其内容到任何文件或对话**）。模板见 `.env.example`。

### 环境（已就绪，已验证连通）

| 组件 | 地址 | 备注 |
|---|---|---|
| MySQL | `192.168.3.200:3316` | 8.0.36，库 `claracore_server`，专用账号（非 root），`ngram_token_size=2` 已确认 |
| Redis | `192.168.3.200:6381` | 7.0.15，key 前缀 `ccs:`，**可选**（未配置时系统降级仍完整可用） |
| Go | 1.26.3 | 国内需 `GOPROXY=https://goproxy.cn,direct` |
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
| 多实例可能重复算 | **已知且接受**。用 `FOR UPDATE SKIP LOCKED` 领活要持有事务跨越一次外部 HTTP 调用，长事务比重复算危险得多。写回是幂等的按 ID 覆盖，重复的代价只是一次外部调用。真到要省这个钱的规模，正确的修法是 `ports.Locker` |

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

自动上下文使用确定性最低分与差值门槛，只读选择最多一张卡；终端 Agent 的模型负责最终相关性判断。它不生成卡片、不修改召回条件，也不改变摄取的人工审核边界。

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

### 这一层还缺的东西

- **审核页没有批量操作**。几千条 draft 一条条点是不现实的，但「批量确认」与「必须有人真的看过」直接冲突——这个取舍要等真实的审核负担出现再定。
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
| **缓存尚未跨实例共享** | 单实例已有 `hash(model+text)` 有界 LRU；Redis decorator 尚未实现 | §检索层 |
| **自动上下文只召回 Card** | 还不是 Desktop Memory Controller 的完整移植，也不组合 Memory + Shared Line | `08-usage-guide.md` §8 |
| **审核页没有批量操作** | 几千条 draft 一条条点不现实，但与「必须有人真看过」冲突 | §Web 治理面 |
| **外部源连接（odoc 等）** | 第二版。`01` §8.1.3 定的是「活系统连接、死文档纳入」 | `01` §8 |
| **回召反馈只有基础计数** | `context_quality_status` 可看 30 天 submitted/outcome，但样本未覆盖所有曝光，不能解释成准确率 | `08-usage-guide.md` §8 |
| **自动质量样本仍需扩充** | 真实 embedding + Stage B 已有正负与延迟门禁，但样本量仍小，需结合 wrong/ignored 反馈演进 | `08-usage-guide.md` §8 |
| **开源许可证已完成** | 根目录与二进制发布包均携带 Apache License 2.0，贡献指南同步说明默认贡献许可 | 根目录 README / LICENSE |
| **公开生产 HTTPS 尚未验收** | 模板已安全默认 HTTPS，但当前团队实例仍是内网 HTTP，不能作为公网生产证据 | `07-deployment.md` |

---

## 检索层剩余的东西（详）

- **backfill 与失败观测已完成**。`/admin/indexing` 可查看状态、最近失败并触发补索引/失败重试；真实 MySQL 的租户隔离和重试路径有集成测试。
- **单实例 embedding 缓存已完成**。进程内有界 LRU 使用 `hash(model+text)`，TTL 与容量可配，`server_status` 可看 hits/misses；多实例共享与跨重启保留仍需要 Redis decorator。
- **检索结果不带 labels**。粗筛刻意不填（N+1 或让候选集膨胀的 JOIN），按 ID 取单条时才有。MCP 工具编码时注意这一点。

---

## 下一步

**0. 当前工作流：人工视觉与业务验收。** 团队测试实例已有拟真演示数据、共同线列表/详情/历史/归档恢复、内容全文查找、重复候选、选择即提交筛选、活动/审计筛选、系统状态和新的上下文/生命周期能力；由用户刷新后用真实身份检查登录、概览、内容、共同线、活动、管理页及 1440/390px 布局，并实际确认一次高风险操作提示。视觉与业务回执回来之前，不宣称生产验收。许可证已确定并落地为 Apache License 2.0。

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
