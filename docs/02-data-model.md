# ClaraCore Server 数据模型与空间权限设计

> 创建日期：2026-08-05
> 状态：**已落地**（0001–0006 已应用到 development MySQL；`make itest` 已验证实际结构与 SQL 契约）。落地时的偏差在正文中就地标注，以 `migrations/` 为准
> 前置文档：`01-architecture.md`

---

## 1. 域划分

Desktop 的 35 张表在单一 `schema.sql` 中随功能逐步堆积，没有整体划分。Server 版**从一开始分三个域**，物理上可分文件、逻辑上禁止跨域直接 JOIN（跨域访问走领域层）：

| 域 | 职责 | 主要实体 |
|---|---|---|
| **治理域** (governance) | 谁是谁、谁能干什么 | tenant, user, agent, token, space, space_permission, audit_log |
| **核心域** (core) | 知识本身 | memory, card, card_recall_trigger, label, link, shared_line |
| **运行时域** (runtime) | 系统自身的运转痕迹 | embedding, trace, job, rate_counter |

**原则**：核心域的实体不感知租户以外的治理细节，只持有 `space_id`；权限判断在领域层完成后，以「可读空间集合」的形式传入查询。

---

## 2. 治理域

### 2.1 身份层级

```
tenant (租户 / 一个团队一套部署单元)
  └── user (人，管理员分配账号)
        └── agent (人的 agent，如 clara / codex)
              └── token (可多个，用于 MCP 连接)
```

**核心决策：agent 是审计维度，不是权限维度。**

- 权限**只**配置在 user 上
- agent 继承其所有者 user 的全部权限，无自己的权限记录
- token 绑定 `(user_id, agent_id)`，鉴权时从 token 反查 user，再查 user 的空间授权
- `agent_id` 的唯一作用是回答「这条内容是谁的哪个 agent 写的」

**推论**：token 表与 space_permission 表之间**没有任何外键或交叉**。新增一个 agent 不需要配任何权限。

### 2.2 空间模型

三层空间，全部落在同一张 `spaces` 表，用 `type` 区分：

| type | 数量 | 归属 | 默认权限 |
|---|---|---|---|
| `personal` | 每用户自动创建一个 | `owner_user_id` = 该用户 | 仅 owner 可读写，不可授权他人 |
| `project` | 可多个，显式创建 | `owner_user_id` = 创建者 | 显式授权（见 §7 待定） |
| `team` | 每租户一个 | 无 owner | 全员可读，写权限需显式授予 |

`personal` 空间的封闭性是**设计约束而非默认值**：它是用户敢于随手记录的前提。要共享就走提升（§4），不开放授权口子。

### 2.3 Token

| 规则 | 实现 |
|---|---|
| 一个 token = 一个 agent | `tokens.agent_id` 非空 |
| 一个 agent 可有多个有效 token | 无唯一约束，平滑轮换的前提 |
| 只展示一次 | 库中只存 `token_hash`（SHA-256），明文仅创建时返回 |
| 可识别前缀 | `token_prefix` 明文存储，用于列表展示与日志脱敏 |
| 记录最后使用 | `last_used_at`，用于识别可安全吊销的 token |

**轮换流程**：签发新 token → 各设备逐个更新 → 观察旧 token 的 `last_used_at` 不再变化 → 吊销。

---

## 3. 核心域

### 3.1 两类一等公民

`memory` 与 `card` 是**并列的独立实体**，不是继承关系（理由见 `01-architecture.md` §1.3）。

共性能力（labels / links / embeddings）通过**多态引用** `(entity_type, entity_id)` 实现，而不是各建一套表。

> **权衡说明**：多态引用在 MySQL 中无法使用外键约束，引用完整性需由应用层保证；换来的是 label / link / embedding 三套逻辑各只写一份，且将来新增第三种内容类型无需重复建表。此权衡是有意为之。

### 3.2 Card 的结构

Card 的固定 section 直接落成列，而非塞进一个 JSON blob——因为「毛仔的理解」这一栏是**质量分界线**（该栏为空的卡只是摘要），需要能被查询和统计。

| 列 | 对应 |
|---|---|
| `title` | 标题 |
| `card_type` | 类型（如「Codex 协作安全」） |
| `status` | 状态：`draft` / `active` / `deprecated`（2026-08-07 定，见下） |
| `source` | 来源 |
| `body_original` | 作者原意 |
| `body_distilled` | 提炼 |
| `body_understanding` | **理解**（空 = 只是摘要，非合格卡片） |
| `body_why_keep` | 为什么值得保留 |
| `body_unresolved` | 未解决问题 |

**`status` 为什么是三态（2026-08-07 定，实现 0002 迁移时拍板）**

原设计是 `draft / verifying / confirmed / deprecated` 四态。落地时发现它与 0806 已拍板的「能写就能审、无独立审核实体、作者自管」直接冲突：**没有任何机制会推动一张卡从 `draft` 流转到 `verifying` 再到 `confirmed`**。留着这两个值，库里就会多出两个谁也不会去查的枚举——而死枚举比没有枚举更糟，它让后来的人以为存在一套并不存在的审核流程。

砍到三态，每一个都对应一种确定的检索行为：

| 值 | 谁产生 | 检索行为 |
|---|---|---|
| `draft` | 摄取 | 不进默认检索，需人过一遍 |
| `active` | 手写（默认值） | 正常可检索 |
| `deprecated` | 人工标记 | 不进默认检索，但仍可按 ID 取到 |

`deprecated` 不删除：「这条约定曾经成立过」本身是信息，删掉之后团队会重复踩同一个已被否决的方案。

对照 `memories.status`（`active / archived / superseded / deleted`）——两套枚举**刻意不同**。Memory 记的是生命周期（还要不要它），Card 记的是知识成熟度（可不可信）。强行统一会让其中一套出现语义上无处安放的值。

### 3.3 召回触发条件：独立成表

**这是本产品的核心技术优势，必须独立建模。**

知识卡片的「未来什么时候该想起它」是作者**显式写下的结构化召回条件**，而非需要靠向量推测的隐含语义。现有卡片中它天然就是一个列表：

```
- 准备删除聊天记录、Agent 工作区、Memory、数据库或本地 Git 仓库时
- 为释放磁盘空间而清理隐藏目录、应用容器或历史工作区时
- 备份报告只给出归档路径、大小、校验和，却没有目标文件清单时
```

设计为独立表 `card_recall_triggers`，**每条触发条件单独 embedding**：

| 做法 | 效果 |
|---|---|
| ❌ 整张卡片一个 embedding | 50 行长文的向量被稀释，「删除操作」这个信号淹没在复盘细节里 |
| ✅ 每条触发条件一个 embedding | agent 的当前情境直接匹配触发条件，命中任一条即召回整卡 |

检索时，agent 传入当前工作情境 → 匹配 `card_recall_triggers` → 命中则返回完整卡片。精度远高于对全文做相似度。

> **2026-08-10 实测验证**（`test/recall_e2e_integration_test.go`）：一张召回条件与情境**没有字面重叠**的卡，加一张有字面重叠的干扰卡——纯全文路径把干扰卡排第一，两段式检索把正确的那张纠正回第一位（bge-m3，0.6195 vs 0.6051）。这条设计的价值是被真实模型验证过的，不只是推理出来的。
>
> **一处更正**：本节与 §7.2 说「冗余 `space_id` 让粗筛无需 JOIN cards」，那只对**纯权限过滤**成立。实现时发现躲不掉 JOIN，有两个原因：① 触发条件表上没有 `status` 列，而 draft / deprecated 的卡不该被召回；② 那个 `space_id` 是冗余列，拿它当权限依据等于信任一份可能不同步的副本（有集成测试专门构造了不同步的情况）。冗余列没有白留——它仍是「哪些触发条件属于我能看到的空间」最快的索引路径，只是不能单独作为唯一依据。
>
> `0003` 另给这张表补了 `FULLTEXT ... WITH PARSER ngram`：候选集截断需要一个非向量信号做排序依据，而且没有它，`CCS_EMBEDDING_PROVIDER=disabled` 时 `card_recall` 会一条都返回不了——降级是默认路径不是异常路径。

---

## 4. 提升机制

```
个人空间的 memory / card ──复制──▶ 项目空间 / 团队空间
```

| 规则 | 说明 |
|---|---|
| **复制，非移动** | 原件保留在个人空间 |
| **记录来源** | `copied_from_type` + `copied_from_id`，可追溯「这条团队约定来自谁的哪次踩坑」 |
| **不自动同步** | 两份各自演进。原件更新时**提示**衍生副本可能过时，不做双向同步 |
| **提升即快照** | 复制发生时的内容即为新副本的初始内容，包括其 labels 与 recall_triggers |

---

## 5. 检索模型

### 5.1 两段式检索

Desktop 的教训：全表扫 + 单线程算余弦。Server 版拆成两段：

```
① 粗筛（SQL 层，必须）
   WHERE space_id IN (:可读空间集合)     ← 权限过滤，绝不后置
     AND [labels / 时间范围 / 全文匹配]
   LIMIT :candidate_cap                  ← 候选集上限（建议 500）

② 精算（应用层）
   计算候选集与查询向量的余弦相似度
   → 排序 → 截断 → 返回
```

**权限过滤必须下推到 SQL**，绝不允许「查完再在应用层过滤」——性能与信息泄露双重原因。

候选集超过 `candidate_cap` 时，按非向量信号（时间新近度、空间层级、label 匹配度）排序后截断，而非随机丢弃。

> **2026-08-10 实现时的两处更正**（详见 `HANDOFF.md` §检索层）：
>
> **① 第二段不开 goroutine 池。** 本节原写「goroutine 池并行计算余弦」，那是误诊了 Desktop 的病——那边真正的故障是 JS 单线程事件循环被一次大搜索独占、所有 agent 一起阻塞，而 Go 里每个请求本来就在自己的 goroutine 上，这一条由 runtime 免费解决。一次检索内部是 500 候选 × 千余维 ≈ 五十万次乘加，不到 1ms，开池只是加调度开销。这一段真正的成本在「从 MySQL 拉几百条向量 BLOB 并解码」，那是 IO。
>
> **② 第一段的 `MATCH` 只用于排序，不进 `WHERE`。** 进了 WHERE，候选集就只剩字面命中的行，第二段的向量再准也没用——而「字面不沾边但语义相关」正是要靠向量找出来的那一类。代价是每次检索要为空间内的全部可见行算一次相关性；万级规模没问题，撞墙时该做的是把粗筛拆成「全文索引命中集 ∪ 最近内容」两条索引路径求并，不是调 `candidate_cap`。
>
> **两类分数不通约。** 余弦与 ngram 相关性不是一个量纲，实现上分成两组返回（语义命中在前、全文命中在后，每条标注自己是哪一种），不做加权融合。

### 5.2 跨空间

- **默认跨**用户全部可读空间
- 结果**必须标注来源空间**——agent 据此判断权威性：team = 已达成共识，personal = 个人经验
- 工具参数支持显式 `spaces: [...]` 限定作为逃生舱
- 排序中空间层级只在分数接近时作为 tie-break（team > project > personal），不做无评估依据的可配权重

### 5.3 Embedding 与缓存

`embeddings` 表持有 `content_hash`：

- 内容未变则不重算，直接复用
- 可作为未来 Redis 查询向量缓存的 key（`hash(text + model)`）；当前尚未实现该缓存端口
- 换 embedding 模型时按 `model` 列区分，新旧共存，可灰度重建

自动上下文复用 Desktop 的确定性 Stage B：只接收已通过权限过滤且带语义分数的 Card，按可配置的最低分与 top-2 差值门槛筛选，弱相关、歧义或无向量时均 fail closed，最多返回 top-1 只读证据块。Server 不再调用第二个大模型；终端 Agent 配置的大模型负责最终语境判断。它不参与显式 `card_recall`，也不生成或改写知识。

---

## 6. 并发控制

Desktop 单进程 SQLite 天然无此问题；Server 必须处理。

**乐观锁**：所有可变实体带 `version` 列，更新时：

```sql
UPDATE memories SET ..., version = version + 1
WHERE id = ? AND version = ?
```

影响行数为 0 即冲突，向调用方返回明确的冲突错误（而非静默覆盖），由 agent 决定重试或合并。

**分布式锁**（`Locker` 端口）仅用于跨实体的复合操作（如批量提升、合并），单实体更新一律走乐观锁。

---

## 7. 表结构草案

> MySQL 8.0+。主键统一 `CHAR(26)` ULID —— 时间有序（插入性能优于随机 UUID）、分布式友好、可读。
> **所有时间列统一 UTC**，用 `DATETIME(3)` 而非 `TIMESTAMP`（避开时区自动转换与 2038 问题）。
> **作用域字段一律不给默认值** —— 缺失即报错，不让 bug 静默（Desktop 的 `DEFAULT 'local-user'` 教训）。
> **所有定长 `CHAR` 列显式 `CHARACTER SET ascii COLLATE ascii_bin`**（2026-08-06 实现 0001 时补充）——
> ID 与 hash 都是纯 ASCII，跟随表默认的 utf8mb4 会让索引按 4 字节/字符计（`CHAR(26)` 占 104 而非 26 字节），
> 且每次比较都走一遍大小写折叠；ULID 是大写 Crockford base32，折叠没有意义。
> 由 `test/schema_test.go` 规则 6 强制，并由 `test/schema_integration_test.go` 在真实库中复验。
>
> 下方 DDL 为设计草案，为可读性省略了列级字符集声明；**以 `migrations/` 下的实际迁移文件为准**。

### 7.1 治理域

```sql
CREATE TABLE tenants (
  id            CHAR(26)     NOT NULL,
  name          VARCHAR(128) NOT NULL,
  status        VARCHAR(16)  NOT NULL,       -- active | suspended
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE users (
  id            CHAR(26)     NOT NULL,
  tenant_id     CHAR(26)     NOT NULL,       -- 无默认值
  username      VARCHAR(64)  NOT NULL,
  password_hash VARCHAR(255) NOT NULL,       -- argon2id
  display_name  VARCHAR(128) NOT NULL,
  role          VARCHAR(16)  NOT NULL,       -- admin | member
  must_change_pw TINYINT(1)  NOT NULL,       -- 首次登录强制改密
  status        VARCHAR(16)  NOT NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_users_tenant_username (tenant_id, username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE agents (
  id            CHAR(26)     NOT NULL,
  tenant_id     CHAR(26)     NOT NULL,
  user_id       CHAR(26)     NOT NULL,       -- agent 属于人
  slug          VARCHAR(64)  NOT NULL,       -- clara / codex / hermes
  display_name  VARCHAR(128) NOT NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_agents_user_slug (user_id, slug),
  KEY idx_agents_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE tokens (
  id            CHAR(26)     NOT NULL,
  tenant_id     CHAR(26)     NOT NULL,
  user_id       CHAR(26)     NOT NULL,
  agent_id      CHAR(26)     NOT NULL,       -- 一个 token = 一个 agent
  name          VARCHAR(128) NOT NULL,       -- 用户可读的标识，如 "MacBook"
  token_prefix  VARCHAR(16)  NOT NULL,       -- 明文前缀，列表展示与日志脱敏
  token_hash    CHAR(64)     NOT NULL,       -- SHA-256，明文仅创建时返回一次
  expires_at    DATETIME(3)      NULL,
  last_used_at  DATETIME(3)      NULL,       -- 判断可否安全吊销
  revoked_at    DATETIME(3)      NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_tokens_hash (token_hash),
  KEY idx_tokens_agent (agent_id),           -- 一 agent 多 token：无唯一约束
  KEY idx_tokens_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE spaces (
  id            CHAR(26)     NOT NULL,
  tenant_id     CHAR(26)     NOT NULL,
  type          VARCHAR(16)  NOT NULL,       -- personal | project | team
  name          VARCHAR(128) NOT NULL,
  owner_user_id CHAR(26)         NULL,       -- personal/project 有主，team 为 NULL
  created_at    DATETIME(3)  NOT NULL,

  -- 生成列把 §2.2 的两条设计约束下沉到数据库（2026-08-06 补充）：
  -- 每用户恰好一个 personal、每租户恰好一个 team。
  -- 不能直接对 (tenant_id, type, owner_user_id) 建唯一键——那会误杀
  -- 「同一用户创建多个 project 空间」这一合法场景。
  -- 靠应用层 SELECT-then-INSERT 保证唯一性在并发下必然失效（见 §6）。
  personal_owner_id CHAR(26) GENERATED ALWAYS AS (IF(type = 'personal', owner_user_id, NULL)) VIRTUAL,
  team_tenant_id    CHAR(26) GENERATED ALWAYS AS (IF(type = 'team', tenant_id, NULL)) VIRTUAL,

  PRIMARY KEY (id),
  UNIQUE KEY uk_spaces_personal (personal_owner_id),
  UNIQUE KEY uk_spaces_team (team_tenant_id),
  KEY idx_spaces_tenant_type (tenant_id, type),
  KEY idx_spaces_owner (owner_user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 权限只挂 user，不挂 agent；只分读写
CREATE TABLE space_permissions (
  space_id      CHAR(26)     NOT NULL,
  user_id       CHAR(26)     NOT NULL,
  can_read      TINYINT(1)   NOT NULL,
  can_write     TINYINT(1)   NOT NULL,
  granted_by    CHAR(26)     NOT NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (space_id, user_id),
  KEY idx_space_perm_user (user_id)          -- 「我能读哪些空间」的主查询路径
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 7.2 核心域

```sql
CREATE TABLE memories (
  id            CHAR(26)     NOT NULL,
  tenant_id     CHAR(26)     NOT NULL,
  space_id      CHAR(26)     NOT NULL,       -- 无默认值
  author_user_id  CHAR(26)   NOT NULL,       -- 谁的
  author_agent_id CHAR(26)       NULL,       -- 哪个 agent 写的（人工创建时为 NULL）
  title         VARCHAR(255) NOT NULL,
  body          MEDIUMTEXT   NOT NULL,
  status        VARCHAR(16)  NOT NULL,       -- active | archived | superseded | deleted
  copied_from_type VARCHAR(16)  NULL,        -- 提升来源
  copied_from_id   CHAR(26)     NULL,
  version       INT          NOT NULL,       -- 乐观锁
  created_at    DATETIME(3)  NOT NULL,
  updated_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_memories_space_status (space_id, status, updated_at),  -- 粗筛主路径
  KEY idx_memories_author (author_user_id),
  FULLTEXT KEY ft_memories (title, body) WITH PARSER ngram       -- 中文全文粗筛
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE cards (
  id            CHAR(26)     NOT NULL,
  tenant_id     CHAR(26)     NOT NULL,
  space_id      CHAR(26)     NOT NULL,
  author_user_id  CHAR(26)   NOT NULL,
  author_agent_id CHAR(26)       NULL,
  title         VARCHAR(255) NOT NULL,
  card_type     VARCHAR(64)  NOT NULL,
  status        VARCHAR(16)  NOT NULL,       -- draft | active | deprecated
  source        TEXT             NULL,
  body_original      MEDIUMTEXT  NULL,       -- 作者原意
  body_distilled     MEDIUMTEXT  NULL,       -- 提炼
  body_understanding MEDIUMTEXT  NULL,       -- 理解（空 = 只是摘要，非合格卡片）
  body_why_keep      MEDIUMTEXT  NULL,       -- 为什么值得保留
  body_unresolved    MEDIUMTEXT  NULL,       -- 未解决问题
  copied_from_type VARCHAR(16)   NULL,
  copied_from_id   CHAR(26)      NULL,
  version       INT          NOT NULL,
  created_at    DATETIME(3)  NOT NULL,
  updated_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_cards_space_status (space_id, status, updated_at),
  KEY idx_cards_author (author_user_id),
  FULLTEXT KEY ft_cards (title, body_distilled, body_understanding) WITH PARSER ngram
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 结构化召回条件：每条单独 embedding，命中任一条即召回整卡
CREATE TABLE card_recall_triggers (
  id            CHAR(26)     NOT NULL,
  tenant_id     CHAR(26)     NOT NULL,
  card_id       CHAR(26)     NOT NULL,
  space_id      CHAR(26)     NOT NULL,       -- 冗余，使粗筛无需 JOIN cards
  trigger_text  VARCHAR(512) NOT NULL,
  sort_order    INT          NOT NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_triggers_card (card_id),
  KEY idx_triggers_space (space_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 多态：entity_type ∈ {memory, card}
CREATE TABLE labels (
  tenant_id     CHAR(26)     NOT NULL,
  entity_type   VARCHAR(16)  NOT NULL,
  entity_id     CHAR(26)     NOT NULL,
  label         VARCHAR(64)  NOT NULL,
  created_at    DATETIME(3)  NOT NULL,       -- 实现 0002 时补：每张表都要有（schema 规则 4）
  PRIMARY KEY (entity_type, entity_id, label),
  KEY idx_labels_lookup (tenant_id, label)   -- 按标签粗筛
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- labels 刻意**不**冗余 space_id，而 card_recall_triggers 冗余了。
-- 不对称是有理由的：按标签检索无论如何都要回表取标题正文，那次 JOIN 省不掉；
-- 而 card_recall 的第一段粗筛只需要触发文本和空间权限，冗余一列即可完全避开 JOIN。

CREATE TABLE links (
  id            CHAR(26)     NOT NULL,
  tenant_id     CHAR(26)     NOT NULL,
  from_type     VARCHAR(16)  NOT NULL,
  from_id       CHAR(26)     NOT NULL,
  to_type       VARCHAR(16)  NOT NULL,
  to_id         CHAR(26)     NOT NULL,
  kind          VARCHAR(32)  NOT NULL,       -- related | causes | evolved-from | contradicts | part-of | supersedes
  note          TEXT             NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_links (from_type, from_id, to_type, to_id, kind),
  KEY idx_links_to (to_type, to_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 7.3 运行时与审计记录

`audit_logs` 物理上合并承载调用 trace、治理审计与管理员查询的 meta-audit，但通过 `kind` 保持不同保留策略：只有 `trace` 可进入 30 天清理路径，`governance` 与 `meta_audit` 长期保留。普通成员的可见性不存成一列，也不在模板层过滤；MySQL 查询直接返回「本人全部活动 + 调用者可读共享空间里的他人写入」。

```sql
-- ⚠️ 下面是**草案**。实际以 migrations/0003_embeddings.sql 为准，
-- 2026-08-10 落地时改了三处，每一处都是写代码才暴露出来的：
--   1. vector 改为可空 —— 这张表同时是索引队列，写入事务里落一行 pending，
--      向量后台补。NOT NULL 会逼我们用空 BLOB 冒充「还没算」。
--   2. 增加 source_text —— 存下真正被算成向量的那段文本（已截断），
--      让索引 worker 成为这张表的纯函数，也让「这个向量照着哪段文本算的」可查。
--   3. 不建 idx_emb_hash —— 跨实体复用向量的优化本版没做，
--      而没有查询用到的索引只会让每次写入白维护一棵 B 树。
CREATE TABLE embeddings (
  id            CHAR(26)     NOT NULL,
  tenant_id     CHAR(26)     NOT NULL,
  entity_type   VARCHAR(24)  NOT NULL,       -- memory | card | card_trigger
  entity_id     CHAR(26)     NOT NULL,
  model         VARCHAR(64)  NOT NULL,       -- 换模型时新旧共存，可灰度重建
  content_hash  CHAR(64)     NOT NULL,       -- 内容未变则不重算；亦为 Redis 缓存 key
  vector        BLOB         NOT NULL,       -- float32 紧凑存储，非 JSON
  dim           INT          NOT NULL,
  status        VARCHAR(16)  NOT NULL,       -- pending | ready | failed
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_emb (entity_type, entity_id, model),
  KEY idx_emb_hash (content_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 2026-08-11 已落成 migrations/0006_audit_logs.sql。
-- 与早期草案相比，真实迁移补齐了来源、保留类型、读写分类、结果分类，
-- 并把 user_id 收紧为 NOT NULL：第一版只记录已认证主体的动作。
CREATE TABLE audit_logs (
  id            CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id       CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  agent_id      CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NULL,
  source        VARCHAR(16)  NOT NULL,       -- mcp | web
  kind          VARCHAR(24)  NOT NULL,       -- trace | governance | meta_audit
  access        VARCHAR(16)  NOT NULL,       -- read | write
  outcome       VARCHAR(16)  NOT NULL,       -- success | error
  action        VARCHAR(64)  NOT NULL,
  entity_type   VARCHAR(24)      NULL,
  entity_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NULL,
  space_id      CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NULL,
  metadata      JSON             NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_audit_tenant_created (tenant_id, created_at, id),
  KEY idx_audit_user_created (tenant_id, user_id, created_at, id),
  KEY idx_audit_space_write (tenant_id, space_id, access, created_at, id),
  KEY idx_audit_trace_retention (kind, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

> **0006 的实现边界**：`metadata` 不是任意请求/响应快照。Host 只允许 ID、命中计数与语义标志等安全字段；query、situation、正文、密码、token 与错误文本不能写入。可选 ID 不符合定长 ASCII 存储形状时只丢目标维度，metadata 超过 8 KiB 时只丢补充信息，两者都不能让主 action/outcome 事件丢失。管理员完整查询必须先成功写入 `meta_audit`，否则拒绝查询。0811 的 `make migrate` 与 `make itest` 已验证开发 MySQL 上的实际结构、可见性、分页与清理契约。

### 7.1 Continuity（0008）

`0008_continuity.sql` 已落地四张 tenant-scoped 表：

- `continuity_lines`：稳定 owner（user + agent）、标题与 active/archived 状态；不加 `space_id`。
- `continuity_positions`：每条 line 唯一一行当前位置，保存摘要、解释状态、事实 ID、下一步、当前 writer 与乐观锁 `version`。
- `continuity_position_history`：每次成功位置写入都追加不可变快照，保留 writer provenance。
- `continuity_handoffs`：里程碑交接，保存目标、已完成、待办、下一步与 writer。

所有定位 SQL 同时带 `tenant_id` 与 `line_id`。跨 agent 显式更新只改变 position/handoff 的 writer，不改变 line owner；不传 `lineId` 且当前 agent 有多条 active line 时拒绝写入。Shared Line 是可恢复工作位置，不是第二套事实库，`facts_used` 仅保存事实 ID。

---

## 8. Desktop 教训落地对照

| 教训 | 本文档中的落地 |
|---|---|
| 身份靠 header 声明 | §2.1 token 反查 `(user, agent)`，`tokens` 表为唯一身份来源 |
| 单 token 轮换即全断 | §2.3 `idx_tokens_agent` 无唯一约束，多 token 并存 |
| 全表扫 + 单线程算余弦 | §5.1 两段式检索，粗筛索引 `idx_*_space_status` + ngram 全文 |
| 本地时区 | §7 全部 `DATETIME(3)` 存 UTC，不用 `TIMESTAMP` |
| `DEFAULT 'local-user'` | §7 作用域字段（`tenant_id`/`space_id`）一律无默认值 |
| SQL 触发器承载逻辑 | 本设计**零触发器**，全部逻辑在应用层 |
| 35 表无划分 | §1 三域划分，禁止跨域直接 JOIN |
| 长文只能靠「文件+指针」双层 hack | §3.2 Card 原生落库，`MEDIUMTEXT` 分栏存储 |

---

## 9. 待确认

- [x] **项目空间成员**：**显式加入**（2026-08-06 定）。创建 project 空间时只有创建者有权限，其他人需显式授权，不为 project 预置 `space_permissions` 行。

> **注意三类空间的规则形态不同**，`ResolveScope` 里是三个分支而非一条规则：
>
> | 类型 | 读 | 写 |
> |---|---|---|
> | `personal` | 仅 owner，**隐含** | 仅 owner，**隐含**；不可授权他人 |
> | `team` | 全租户，**隐含** | 需显式授权 |
> | `project` | 需显式授权 | 需显式授权 |
>
> `project` 的唯一例外是创建者：owner 视为拥有读写，不写授权行。这条规则不依赖一行可被误删的数据——创建者被自己建的空间锁在门外是没有恢复路径的故障。
>
> 规则只实现在 `internal/domain/governance.ResolveScope`，SQL 层不做任何权限判断（`ListSpaceMemberships` 只负责捞出相关行）。一旦 SQL 里出现 `WHERE can_read = 1 OR type = 'team'`，规则就同时活在两个地方。
- [x] **谁有权提升**：**任意有目标空间写权限者**（2026-08-06 定）。promote 校验**两端**：源空间可读 + 目标空间可写。只校目标写权限的话，拿到一条自己无权读的内容 ID 就能把它复制进自己能读的空间——ULID 不可猜，但「不可猜」不是权限。实现上就是多调一次 `SpaceScope.CanRead`。
- [x] **检索排序中空间权重**：**只做 tie-break，不可配**（2026-08-06 定）。相关性优先，分数接近时才按 team > project > personal 决胜。权重系数是典型的「调了也不知道好没好」参数，可配等于把调参责任推给没有评估集的用户。
- [x] **MySQL 最低版本**：**8.0**（ngram 全文解析器、CTE、JSON 函数、`spaces` 的生成列唯一索引均需要）。实际验证环境 8.0.36。
- [x] **Card 的 `status` 流转**：**按来源分**（2026-08-06 定）。人手写的默认 `active`，作者自行改 status，无评审动作——他愿意写已经是最稀缺的东西；机器摄取产生的一律落 `draft`，不进默认检索，需要有人确认。两个来源的信噪比差一个量级，用同一套规则必然要么卡死人工输入、要么让摄取垃圾污染检索。不引入新权限概念：**能写就能审**。
  <br>**2026-08-07 补**：实现 0002 时据此把枚举从四态砍到 `draft / active / deprecated`——`verifying` 与 `confirmed` 没有任何流转机制会写入它们，见 §3.2。
- [x] **Memory 是否也需要 recall_triggers**：**只给 Card，单态表**（2026-08-06 定）。`recall_triggers` 直接持 `card_id`，不做多态。① Card 的 trigger 是作者显式写下的，而 Memory 就是随手记——要求随手记时还写召回条件，等于取消了「随手」；② 延后代价可控：将来真要改多态，就是加一列 `entity_type`、存量行填 `'card'`、改一个索引，一次在线 DDL（本来就没有外键）。为未知变化不留接口。
