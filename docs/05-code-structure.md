# ClaraCore Server 代码结构与工程约束

> 创建日期：2026-08-05
> 状态：设计中，无代码
> 前置文档：`01-architecture.md` ~ `04-web-scope.md`

---

## 0. 本文档的前提

**目标不是"设计得漂亮"，是三个月后改代码时结构还在。**

因此本文档包含两部分：

1. **结构**（§2–§4）——目录怎么分，依赖怎么走
2. **约束机制**（§5）——**结构靠工具守，不靠自觉**

只有第 1 部分的项目，无一例外会烂。Desktop 已有 `*-boundary-smoke.js` 这类边界测试，那个方向是对的，Server 版从第一天就要有。

### 0.1 反向原则：过度设计也是债

**为已知的变化点留接口，为未知的不留。**

| 已知变化点（留接口） | 未知变化（不留） |
|---|---|
| 数据库：MySQL / SQLite 都要支持 | "将来可能做插件系统" |
| 缓存：有无 Redis 两种形态 | "将来可能支持多种协议" |
| Embedding：Ollama / OpenAI-compatible | "将来可能换 Web 框架" |
| 外部知识源：多种 MCP 源 | "将来可能多语言" |

只有一个实现、且看不到第二个实现的东西，**直接写具体类型**。抽象等到第二个实现真的出现时再提取——那时你才知道抽象边界该划在哪。

---

## 1. 顶层布局

```
claracore-server/
├── cmd/
│   └── server/main.go          # 唯一入口，只做装配
├── internal/                   # Go 编译期强制封装，外部无法 import
│   ├── config/
│   ├── domain/                 # 领域层：不依赖任何基础设施
│   │   ├── governance/
│   │   ├── knowledge/
│   │   └── continuity/
│   ├── ports/                  # 接口定义（领域层的出口）
│   ├── adapters/               # 接口实现（基础设施）
│   ├── mcp/                    # Host A：MCP 服务
│   ├── web/                    # Host B：Web 管理面
│   ├── ingest/                 # 摄取与提炼
│   └── platform/               # 横切：日志/错误/ID/时钟
├── migrations/                 # SQL 迁移，编号顺序
├── docs/                       # 01–05 设计文档
├── test/                       # 集成测试与架构测试
├── Makefile
└── go.mod
```

**`internal/` 不是可选的。** Go 的 `internal` 机制是编译期强制的封装——放在里面的包外部模块无法 import。这是免费拿到的边界保护，全部实现代码都放进去。

---

## 2. 领域层：按域划分，不按技术层

### 2.1 为什么不按 handler/service/repository 分

按技术层划分（`handlers/` + `services/` + `repositories/`）在项目变大后，**改一个功能要在三个目录之间跳**，且每个目录都会膨胀成几十个不相关的文件。

按域划分则让一个功能的代码集中在一起：

```
internal/domain/
├── governance/              # 谁是谁、谁能干什么
│   ├── tenant.go
│   ├── user.go
│   ├── agent.go
│   ├── token.go             # 签发/校验/轮换
│   ├── space.go
│   ├── permission.go        # 「我能读哪些空间」的唯一权威
│   └── service.go           # 域内用例编排
├── knowledge/               # 知识本身
│   ├── memory.go
│   ├── card.go
│   ├── recall_trigger.go    # 结构化召回条件
│   ├── label.go
│   ├── link.go
│   ├── promote.go           # 提升（复制）
│   ├── search.go            # 两段式检索编排
│   └── service.go
└── continuity/
    ├── shared_line.go
    └── service.go
```

对应 `02-data-model.md` §1 的三域划分（运行时域不是领域概念，落在 `adapters/` 与 `platform/`）。

### 2.2 域之间不互相 import

`knowledge` **不 import** `governance`。

权限判断在应用层（Host）完成后，以**值**的形式传入领域层：

```go
// knowledge 层只接受已解析的可读空间集合，不认识 user/permission 概念
func (s *Service) Search(ctx context.Context, q Query, scope SpaceScope) ([]Result, error)

type SpaceScope struct {
    Readable []SpaceID   // 由 governance 解析后传入
    WriteTo  SpaceID
}
```

**收益**：`knowledge` 可以独立测试，不需要构造整套用户权限；权限逻辑只有一处，不会散落在各个查询里。

### 2.3 领域层禁止出现的东西

- `net/http` 的任何类型
- SQL 语句、`database/sql`
- JSON tag（那是传输层的事）
- 全局变量、`init()` 副作用
- 直接读环境变量

违反其中任何一条，说明抽象漏了。

---

## 3. 端口与适配器

### 3.1 接口放在哪：两类分开

> **本节于 2026-08-05 实现骨架时修正。** 原设计把 `Store` 接口也放进 `ports/`，
> 但 Store 的方法签名必然涉及领域类型（`*knowledge.Card` 等），会造成
> `domain → ports → domain` 的循环 import——Go 不允许。

按 Go 惯例「**接口在消费方定义**」，分成两类：

**① 纯基础设施接口 → `internal/ports/`**

签名只用标准库类型（string / []byte / time.Duration），不认识任何领域概念：

```
internal/ports/
├── cache.go          # Get/Set/Del，键值都是 string/[]byte
├── locker.go
├── ratelimiter.go
├── sessionstore.go
├── embedder.go       # []string → [][]float32
└── eventstore.go     # MCP resumability，第一版不实现
```

**② 涉及领域类型的接口 → 定义在各自的 domain 包内**

```
internal/domain/governance/store.go   → type Store interface { ... }
internal/domain/knowledge/store.go    → type Store interface { ... }
internal/domain/knowledge/source.go   → type ExternalSource interface { ... }
```

**依赖方向因此变成**：

```
adapters/mysql ──implements──▶ domain/governance.Store
                               domain/knowledge.Store
```

即 `adapters` 依赖 `domain`，而 `domain` 不依赖任何人——这才是干净的依赖倒置。装配在 `cmd/server/main.go` 完成。

**接口按使用方需要定义，不按实现方能力定义。** 不要一个 `Store` 塞 80 个方法：`governance` 需要什么就在 `governance` 里定义什么。

### 3.2 适配器实现在 `adapters/`

```
internal/adapters/
├── mysql/            # 主实现
├── sqlite/           # 本地开发 / 轻量部署
├── redis/            # Cache/Locker/RateLimiter/SessionStore 的 Redis 实现
├── inmem/            # 降级实现：无 Redis 时的默认
├── embedding/
│   ├── ollama.go
│   └── openaicompat.go
└── mcpsource/        # 外部 MCP 源连接器（第二版）
```

**降级是默认路径不是异常路径**（`01-architecture.md` §6.1）：未配置 Redis 时装配 `inmem` 实现，系统功能完整。这一点在装配处（`cmd/server/main.go`）体现，领域层完全无感。

### 3.3 SQL 写在哪

**SQL 只允许出现在 `adapters/mysql/` 和 `adapters/sqlite/`。**

不引入 ORM。理由：本项目的查询有明确的性能要求（两段式检索的粗筛、权限过滤下推），ORM 生成的 SQL 不可控，而手写 SQL 在 30 张表规模下完全可维护。

用 `sqlc` 或手写 + `database/sql` 均可，但**不用 GORM 这类运行时反射 ORM**。

---

## 4. 两个 Host

```
internal/mcp/                  internal/web/
├── server.go                  ├── server.go
├── auth.go     ← TokenVerifier├── middleware/
├── context.go  ← 身份注入 ctx ├── handlers/
└── tools/                     ├── templates/   //go:embed
    ├── memory.go              └── static/      //go:embed
    ├── card.go
    ├── space.go
    ├── continuity.go
    └── system.go
```

**铁律（`04-web-scope.md` §1.4）：两个 Host 都只调用 `domain/`，不允许任何一方持有对方的业务逻辑，也不允许 Host 直接触碰 `adapters/`。**

Host 层的职责严格限定为三件事：

1. 协议解码（MCP 参数 / HTTP 表单）
2. 身份解析与权限装配（token/session → `SpaceScope`）
3. 结果编码（JSON / HTML）

任何 `if` 业务判断出现在 Host 层，都是应该下沉到 domain 的信号。

---

## 5. 约束机制（本文档的另一半）

> 光有目录结构挡不住债务。以下每条都必须是**可执行的检查**，进 CI。

### 5.1 依赖方向检查

允许的依赖方向**单向**：

```
cmd → mcp/web/ingest → domain → ports
                 ↓                 ↑
              platform         adapters
```

具体禁止规则：

| 规则 | 检查方式 |
|---|---|
| `domain/**` 不得 import `adapters/**` | import 扫描 |
| `domain/**` 不得 import `net/http`、`database/sql`、数据库/Redis 驱动 | import 扫描 |
| `domain/knowledge` 不得 import `domain/governance`（及反向） | import 扫描 |
| **`ports/**` 不得 import `domain/**`** | import 扫描 |
| `mcp/**` 与 `web/**` 不得互相 import | import 扫描 |
| `mcp/**`、`web/**` 不得 import `adapters/**` | import 扫描 |
| `domain/**` 不得出现包级可变变量（`ErrXxx` 哨兵除外） | AST 扫描 |

**已实现**（2026-08-05）：`test/arch_test.go`，纯标准库 `go/parser` + `go/ast`，**不依赖第三方架构 lint 工具**——那类工具停更之日就是约束失效之时。

`test/schema_test.go` 实现 §5.2 的迁移规则，并额外检查迁移文件编号连续、无重复。

> **约束已被反向验证**：构造过一个故意违规的探针文件（domain 里 import adapters + 跨域 import + `net/http` + 包级 var）与一个违规迁移（触发器 + `TIMESTAMP` + `tenant_id DEFAULT` + 缺 `created_at` + 无 utf8mb4），**9 条违规全部被捕获**。
>
> 从不失败的检查等于没有检查——新增约束后务必用同样方式验证一次。

### 5.2 Schema 约束检查

对应 `02-data-model.md` 的硬规则，写成 migration 的 lint：

| 规则 | 理由 |
|---|---|
| 禁止 `CREATE TRIGGER` | 逻辑必须在应用层，保证跨库可移植 |
| 禁止 `TIMESTAMP` 列类型（只允许 `DATETIME(3)`） | 避开时区自动转换与 2038 |
| `tenant_id` / `space_id` 列禁止带 `DEFAULT` | 缺失即报错，不让 bug 静默 |
| 新表必须有 `created_at` | 可追溯 |

### 5.3 其他必须进 CI 的检查

- `go vet` + `staticcheck`
- `gofmt -l` 结果必须为空
- 单元测试覆盖 `domain/`（领域层无外部依赖，天然易测，**覆盖率门槛只对 `domain/` 设**）
- 迁移文件编号连续、不可修改已发布的迁移

**覆盖率门槛已落地**（2026-08-06）：`make cover` 跑 `./internal/domain/...`，低于 `COVERAGE_MIN`（85）即退出码非零，已挂进 `make check`。验证过它真的会红——把门槛临时提到 95 会失败，移走 `space_test.go` 让实际覆盖率掉到 71.5% 也会失败。注意门槛挡的是**整块逻辑缺失**：移走 `token_test.go` 只掉到 88.2%，仍然放行，这是留余量的代价，不是缺陷。

### 5.4 一条纪律

**发现结构被破坏时，修结构，不加例外。**

第一个 `// nolint` 或"就这一次直接调 adapter"，是所有架构腐化的起点。真的需要破例时，说明抽象错了——改抽象，不是加豁免。

---

## 6. 其他约定

### 6.1 错误处理

- 领域层返回带语义的错误类型（`ErrVersionConflict`、`ErrSpaceForbidden`），**不返回裸 `errors.New`**
- Host 层负责把领域错误映射为协议错误（MCP 错误码 / HTTP 状态码）
- 错误映射表集中在一处，不散落

### 6.2 时间与 ID

- **时间**：`platform/clock` 提供 `Now() time.Time`，一律 UTC。领域层不直接调 `time.Now()`（否则无法测试时间相关逻辑）
- **ID**：`platform/id` 生成 ULID。不在领域层散落生成逻辑

### 6.3 配置

- 单一 `config.Config` 结构体，启动时一次性加载并校验
- **启动时校验失败直接 fatal**，不允许运行期发现配置缺失
- 环境变量只在 `config/` 包读取，其他任何地方禁止 `os.Getenv`

### 6.4 测试组织

| 类型 | 位置 | 依赖 |
|---|---|---|
| 领域单测 | 与被测代码同目录 `_test.go` | 无外部依赖，必须快 |
| 适配器测试 | `adapters/*/*_integration_test.go`（`//go:build integration`） | 真实 MySQL，`make itest` |
| 集成测试 | `test/` | 完整装配 |
| 架构测试 | `test/arch_test.go` | 见 §5.1 |

**适配器测试打 `integration` tag，不进 `make check`**（2026-08-06 落地时定）。原考虑的 testcontainers 需要本地 Docker，SQLite 则要维护第二套方言——两者都是为了"测试能脱离真库跑"而付出的真实成本，而这套系统本来就有一台常驻的开发库。代价是 CI 没库时这层测不到，所以：**改完 SQL 必须手跑一次 `make itest`**。

**适配器测试的基准是领域层的 fake store**：同一个接口两个实现，行为必须一致，否则领域单测全绿而线上行为不同。写法上，绝大多数用例跑在一个最终 rollback 的事务里（不留痕迹），真实提交路径单独一个用例、自己清理。

---

## 7. 待确认

- [x] **SQL 方式**：**手写 `database/sql`**（2026-08-06 定）。两段式检索的粗筛要拼动态 `IN` 子句与可选条件，sqlc 对动态 SQL 支持有限，八成还是要手写逃生舱；30 张表规模手写完全可控，且少一层工具链。
- [x] **迁移工具**：**自实现**（2026-08-06 定）。见 §8。
- [x] **`domain/` 覆盖率门槛**：**85%，低于即 CI 失败**（2026-08-06 定）。`governance` 实测 89.1%，留出余量；低于 85 基本意味着有整块逻辑没被测到。**只卡 `domain/`**——`adapters/` 的测试需要真库，CI 未必有。
- [x] **是否引入 `sqlite` 适配器**：**不做**（2026-08-06 定）。本地开发用 Docker 跑 MySQL。项目已经用上 ngram 全文索引、生成列唯一索引、`GET_LOCK`——sqlite 全都没有，真要兼容就得把设计向下兑齐到两者的交集，而两段式检索的粗筛恰恰全是非平凡 SQL。`§3.2` 目录树里的 `sqlite/` 因此不建。

---

## 8. 迁移执行

```
migrations/embed.go                    //go:embed *.sql，编进二进制
internal/adapters/mysql/migrate.go     执行器 + schema_migrations 记账
cmd/server -migrate                    入口（make migrate），保持单一入口
```

| 保证 | 做法 |
|---|---|
| 顺序应用 | 按 `NNNN` 前缀排序，已应用的跳过 |
| 已发布迁移不可修改 | 记 SHA-256 校验和，不匹配即拒绝启动并提示新增迁移文件 |
| 多实例安全 | `GET_LOCK('ccs:schema_migrate', 30)`，滚动发布时不会两个实例同时改表 |
| 迁移不隐式发生 | 只在 `-migrate` 下执行；服务正常启动不改表结构 |
| 版本与二进制一致 | SQL 编进二进制，不存在「二进制和 migrations/ 对不上」 |

**不假装有原子性**：MySQL 的 DDL 隐式提交，把整个迁移包进事务是做不到的。执行器的保证是「顺序执行 + 精确记账」——任一语句失败即中止，报错给出停在哪个文件的第几条语句，人工确认库状态后重跑。

`splitStatements` 按分号拆语句时跳过注释与字符串字面量（有 12 个用例覆盖）。不用 `strings.Split(";")`：迁移里出现带分号的字面量是迟早的事，那时的报错会伪装成"语法错误"，排查代价极高。

集成测试 `test/schema_integration_test.go`（`make itest`，需真实 MySQL）验证迁移在库里的**实际结果**——文本检查挡不住「MySQL 接受了但语义不是你以为的那样」。
