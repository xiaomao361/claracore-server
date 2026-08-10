# ClaraCore Server — 接手指南

> 最后更新：2026-08-10 14:30
> 给下一次接手的人或 agent（Clara / Codex / 其他）

---

## 一句话状态

**检索已经在真库 + 真模型上跑通：写入入队 → 后台 worker 算向量 → 两段式检索（SQL 粗筛 + 余弦精算）→ `card_recall` 按情境召回整卡。治理域的 token → 身份 → 空间权限 → MCP `whoami` 仍然全绿。下一步是把 MCP 工具接上去。**

端到端的证据是 `test/recall_e2e_integration_test.go`（`make itest` 每次真跑，需要 Ollama 在线，不在线则 skip）：一张召回条件与情境**没有字面重叠**的卡，加一张有字面重叠的干扰卡——纯全文路径把干扰卡排第一，两段式检索把正确的那张纠正回第一位（bge-m3 实测 0.6195 vs 0.6051）。这是这套检索唯一能证明「确实在起作用」而不只是「跑通了」的地方，别让它烂掉。

`make run` 起来就能用真 token 调 `whoami`。装配在 `cmd/server/main.go`：MySQL Store → `governance.Service` → MCP Host 挂在 `/mcp`，另加 `knowledge.Service` 与后台索引 worker。

**核心域仍然没有任何 MCP 工具接上去**——`knowledge.Service` 目前唯一的消费者是索引 worker。写入侧与检索侧都已就绪，缺的只是 Host 层的编排与结果编码。`internal/web` 仍是空目录。

---

## 30 秒上手

```bash
cd /Users/zhouwei/Documents/ClaraCore/apps/claracore-server

make check     # 全部检查：fmt + vet + 架构约束 + domain 覆盖率门槛 + 测试。必须全绿才提交
make migrate   # 应用数据库迁移（幂等，已是最新时 no-op）
make itest     # 集成测试（schema + Store 契约 + MCP 端到端），需真实 MySQL。改完迁移或 SQL 必须手跑
make run       # 启动服务（需要 .env，已存在）
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

**动手前必读 `05-code-structure.md`**，其余按需查。

| 文档 | 什么时候看 |
|---|---|
| `docs/01-architecture.md` | 想知道"为什么这么设计"、部署形态、nginx 配置 |
| `docs/02-data-model.md` | **写迁移文件前必读**。表结构草案、三层空间、检索模型、并发控制 |
| `docs/03-mcp-contract.md` | 写 MCP 工具前必读。20 个工具的清单与语义 |
| `docs/04-web-scope.md` | 写 Web 前必读。页面清单 + 可见性边界 |
| `docs/05-code-structure.md` | **动手前必读**。目录结构、依赖方向、约束机制 |
| `docs/06-poc-findings.md` | 遇到 MCP 协议/传输层问题时查。已验证的结论与已知限制 |

各文档末尾的「待确认」清单已于 2026-08-06 全部拍板，速查表见本文 §已拍板。

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
                               （domain/ 合计 88.6% 覆盖，门槛 85）
internal/ports/embedder.go     Model() + Embed()。纯基础设施接口，不认识领域概念
internal/adapters/embedding/   Ollama（/api/embed 批量）+ OpenAI-compatible（按 index 归位）
  └─ embedding_test.go         用 httptest 造真服务不会给你的坏响应：数量对不上、
                               维度不一致、空向量、404、错误信息泄露内网地址
internal/mcp/                  MCP Host：bearer 认证 + 身份注入 + 错误映射 + whoami
  └─ host_test.go              8 个协议层测试，用 fake 治理服务，不碰数据库
test/mcp_e2e_integration_test.go  完整装配：真库 + 真 token 打通 whoami（make itest）
migrations/0001_governance.sql 治理域 6 张表，已应用到 192.168.3.200 的库
migrations/0002_knowledge.sql  核心域 5 张表，已应用
migrations/0003_embeddings.sql 运行时域 embeddings 表 + 给 card_recall_triggers 补 ngram 全文索引
cmd/server/main.go             最小入口，装配 + 优雅关闭 + -migrate flag
Makefile                       make check 是提交前的门槛
poc/                           丢弃型验证代码，独立 go.mod，不参与主模块构建
```

**约束已反向验证过**：造过违规探针，9 + 3 + 8 + 4 + 7 + 21 + 14 条违规全部被捕获（依次是：架构与 schema 规则、`ascii_bin` 规则、governance Store、`ErrAlreadyExists` 映射与 fake 唯一性、MCP Host 的安全属性、knowledge 领域层与 Store、检索与索引队列）。新增约束或新写一层适配器后请用同样方式验证一次——**从不失败的检查等于没有检查**。

0810 那 14 条的清单（漏网 0）：领域层 6 条（检索空间过滤失效、语义与全文命中混排、查询向量算不出时报错而不降级、错位向量按下标硬配、替换召回条件不清旧向量、入队不与内容同事务）；Embedder 适配器 2 条（按数组顺序取向量而非 index、错误信息带上底层 error 泄露内网地址）；MySQL 5 条（粗筛漏租户隔离、召回权限信冗余列、upsert 里 content_hash 提前赋值、MATCH 进 WHERE 丢掉字面没命中的候选、标签过滤退化成并集）；schema 1 条（造一张没有 ngram 的 FULLTEXT 探针表，确认那条检查真的会红）。

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

**这一层缺的东西**：领域层不做密码哈希——argon2id 的参数（memory/time/parallelism）属于配置，而领域层不读配置。`CreateUserInput.PasswordHash` 目前由调用方传入，等实现 Web 登录时在 `ports/` 定义 `PasswordHasher` 接口，Host 层调用后传进来。在那之前没有任何地方能真正创建用户密码。

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

## MCP Host 已固化的决定（加新工具之前先读这段）

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

### 这一层缺的东西

- **没有 backfill**。未配 Embedder 时不入队（没有模型名可写），所以**后来才打开 embedding 的存量内容不会被自动补索引**，换模型灰度重建同理。要补的形态大致是 `ListUnindexed(model, limit)` 把没有当前模型向量行的实体捞出来、由领域层拼文本再入队——放在 Web 治理面或一个 `-reindex` flag 后面。
- **没有向量缓存**。`ports.Cache` 还不存在，查询向量每次都真算一次。`hash(text+model)` 的 Redis 缓存是 `01-architecture.md` §6.2 里价值最高的一项，等 Cache 端口落地时一起做。
- **没有失败可观测性**。`status=failed` 的行只能靠翻库看，没有计数接口。Web 治理面要用。
- **检索结果不带 labels**。粗筛刻意不填（N+1 或让候选集膨胀的 JOIN），按 ID 取单条时才有。MCP 工具编码时注意这一点。

---

## 下一步

**按这个顺序：**

1. **MCP 工具**（现在是大头）：`memoria_create/search/get/update/delete/link` + `card_create/recall/search/get/update` + `promote` + `space_list`。领域层全部就绪，缺的只是 Host 层编排（模式见上）与结果编码。三件事要注意：
   - 检索结果**必须标注来源空间**（`03-mcp-contract.md` §3.2），而 `knowledge` 只返回 `space_id`——把它映射成 `{id, type, name}` 是 Host 用 governance 那侧的信息做的。
   - 空间 Rank 由 Host 从空间类型算出来（team > project > personal），knowledge 只收整数。
   - `SemanticAvailable=false` 要在返回里体现，别让 agent 以为语义那一半跑过了。
2. **Web Host** → **摄取**。摄取一定要复用同一条入队路径，别自己写第二份 embedding 逻辑。

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

- [x] **覆盖率门槛进了 `make check`**：新增 `make cover`，跑 `./internal/domain/...`，低于 `COVERAGE_MIN`（85）退出码非零。当前 90.0%。验证过它真会红（门槛提到 95 会红；移走 `space_test.go` 让覆盖率掉到 71.5% 也会红）。
- [x] **`ErrAlreadyExists` 已落地**：`governance` 加哨兵，`adapters/mysql` 的 `writeErr` 识别 MySQL 1062 后翻译，五种冲突（第二个个人空间 / 第二个团队空间 / 账号重名 / agent slug 重复 / token hash 重复）都有集成测试。**领域层的 fake store 同步加了对应的唯一性检查**——它是行为基准，原先重名时 map 直接赋值会静默顶掉原用户，而真库会拒绝，那种 fake 会让领域测试跑在数据库不可能达到的状态上。

---

## 项目背景速记

- 这是**团队版**，与 `apps/claracore-desktop`（个人版）是**两条独立产品线**。desktop 继续正常演进，不冻结不废弃。
- 两边**共享契约不共享代码**：MCP 工具名/参数/语义、数据模型概念保持一致（`03-mcp-contract.md` 是共同基准）。
- **InnerLife 不进团队版**：企业场景不需要，且 stateless 下服务端无法主动推送。
- 产品定位：**让团队的踩坑经验和工程约定，能被所有人的 agent 在正确的时机自动召回**。核心载体是知识卡片（Card），不是零散记忆。
- Card 是**独立一等公民**，不是 Memory 的子类型。它的「未来什么时候该想起它」是作者显式写下的结构化召回条件，每条单独 embedding——这是本产品的核心技术优势。
- **冷启动是生死问题**：团队已有存量知识（odoc、Atlas、各类 md），摄取能力是第一版核心，不是次要运维功能。
