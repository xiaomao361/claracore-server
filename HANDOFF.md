# ClaraCore Server — 接手指南

> 最后更新：2026-08-06 15:45
> 给下一次接手的人或 agent（Clara / Codex / 其他）

---

## 一句话状态

**治理域从领域逻辑到 MySQL 存储已经打通，对着真实库跑通契约测试。下一步是 `internal/mcp` —— 先 `whoami`。**

`governance.Store` 的 MySQL 实现已落地（`internal/adapters/mysql/governance.go`），11 个方法全部有对着真实 MySQL 的测试。**但还没有任何东西调用它**：`cmd/server/main.go` 没装配 `governance.Service`，因为目前没有消费者——装一个没人用的服务只会让人以为它在跑。`internal/mcp`、`internal/web` 仍是空目录。

---

## 30 秒上手

```bash
cd /Users/zhouwei/Documents/ClaraCore/apps/claracore-server

make check     # 全部检查：fmt + vet + 架构约束 + domain 覆盖率门槛 + 测试。必须全绿才提交
make migrate   # 应用数据库迁移（幂等，已是最新时 no-op）
make itest     # 集成测试，需真实 MySQL。改完迁移或 adapters 里的 SQL 必须手跑一次
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
test/schema_test.go            迁移文件 schema 规则（6 条）+ 编号连续性
test/schema_integration_test.go 库里的实际结构（make itest，需真实 MySQL）
internal/config/               唯一读环境变量的地方。启动时校验，失败即 fatal
internal/platform/             Clock（可测试的时间）+ ULID（6 个测试全绿）
internal/adapters/mysql/       连接池（启动即 Ping）+ 迁移执行器（12 个拆分用例）
  └─ governance.go             governance.Store 的 MySQL 实现，11 个方法
  └─ *_integration_test.go     对着真实 MySQL 的契约测试（make itest）
internal/domain/governance/    身份认证 + SpaceScope 解析 + token 生命周期（90.0% 覆盖，门槛 85）
migrations/0001_governance.sql 治理域 6 张表，已应用到 192.168.3.200 的库
cmd/server/main.go             最小入口，装配 + 优雅关闭 + -migrate flag
Makefile                       make check 是提交前的门槛
poc/                           丢弃型验证代码，独立 go.mod，不参与主模块构建
```

**约束已反向验证过**：造过违规探针，9 + 3 + 8 + 4 条违规全部被捕获（依次是：架构与 schema 规则、`ascii_bin` 规则、Store 实现、`ErrAlreadyExists` 映射与 fake 的唯一性）。新增约束或新写一层适配器后请用同样方式验证一次——**从不失败的检查等于没有检查**。

### 0001 迁移已验证的行为

| 验证项 | 结果 |
|---|---|
| 6 条 DDL 在 MySQL 8.0.36 上执行 | 通过，215ms |
| 重复执行 | no-op，不报错 |
| 篡改已应用的迁移文件 | 拒绝执行，报出两边校验和 |
| 21 个 CHAR 列的排序规则 | 全部 `ascii_bin` |
| 每用户一个 personal / 每租户一个 team | 数据库层拦截，且不误杀多个 project |

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

## 下一步：按这个顺序

### 1. `internal/mcp/`

PoC（`poc/main.go`）可作参考但**不要直接复用**。要点：

- `auth.RequireBearerToken` + 自定义 `TokenVerifier`，内部就是 `governance.Service.Authenticate`
- `Stateless: true`
- 身份用标准 `context.WithValue` 传递即可——PoC 已验证 HTTP 中间件 context 能贯通到 tool handler，**不需要** `getServer(*http.Request)` 闭包方案
- 先实现 `whoami`，它是 agent 判断"我能写哪个空间"的前提。返回内容直接来自 `Identity` + `SpaceScope.All()`，Host 层只做编码
- 错误映射表集中一处：`ErrTokenNotFound` / `ErrTokenRevoked` / `ErrTokenExpired` **必须映射为同一种响应**，区分它们等于告诉攻击者哪些 token 曾经有效

之后：knowledge 域（memory → card → recall_trigger）→ 两段式检索 → Web Host → 摄取。

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
| 唯一键冲突目前是裸 SQL 错误 | `governance` 还没有 `ErrAlreadyExists`，重名账号/重复 slug 会以 `Error 1062` 冒到上层。**已拍板要加**，尚未落地，见 §已拍板「代码欠账」 |

---

## 已拍板（**目前没有未决问题**）

2026-08-06 一次性清空了全部 16 条未决项。各文档末尾的「待确认」清单已同步勾选，下面是速查；每条的完整理由在对应文档里。

**权限与内容模型**

| 决定 | 口径 | 出处 |
|---|---|---|
| 管理员分级 | **单一 admin**，`role` 只有 admin / member。空间内部不再分级 | `04` §6 |
| 项目空间成员 | **显式加入**，不预置授权行 | `02` §9 |
| promote 权限 | **任意有目标空间写权限者**。质量用 Card 的 `status` 挡，不用权限挡 | `01` §11.2 |
| promote 校验点 | **两端都校**：源可读 + 目标可写。只校目标 = 拿到 ID 就能把无权读的内容搬进自己能读的空间 | `03` §6 |
| Card `status` | **按来源分**：手写默认 `active`（作者自管），摄取一律 `draft`（不进默认检索） | `02` §9 |
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
| 唯一键冲突 | **加 `governance.ErrAlreadyExists`**，adapters 识别 MySQL 1062 后映射 | 本文 |

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
