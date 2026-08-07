-- 0002_knowledge.sql —— 核心域：记什么，什么时候该想起来
--
-- 对应 docs/02-data-model.md §3、§4、§7.2。全局约定见 0001_governance.sql 头部注释。
--
-- 本迁移**不含** embeddings 表：它属于运行时域，真正的约束（向量维度、
-- 模型灰度、pending/failed 的重试语义）只有在实现两段式检索时才看得清楚，
-- 现在建等于凭想象定 schema（2026-08-07 拍板）。
--
-- memory 与 card 是**并列的一等公民**，不是继承关系（01-architecture.md §1.3）。
-- 两张表列结构相似是因为它们确实共享同一批属性，不是因为一个是另一个的特例——
-- 合并成一张带 kind 列的表会让 card 的五个 section 在 memory 行上永远为 NULL，
-- 而 card 的召回触发条件根本无处安放。

CREATE TABLE memories (
  id              CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id       CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  space_id        CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,  -- 无默认值
  author_user_id  CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  author_agent_id CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NULL,      -- 人工创建时为 NULL
  title           VARCHAR(255) NOT NULL,
  body            MEDIUMTEXT   NOT NULL,
  status          VARCHAR(16)  NOT NULL,       -- active | archived | superseded | deleted
  copied_from_type VARCHAR(16) NULL,           -- 提升来源（§4），memory | card
  copied_from_id   CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NULL,
  version         INT          NOT NULL,       -- 乐观锁（§6）
  created_at      DATETIME(3)  NOT NULL,
  updated_at      DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_memories_space_status (space_id, status, updated_at),  -- 粗筛主路径
  KEY idx_memories_author (author_user_id),
  FULLTEXT KEY ft_memories (title, body) WITH PARSER ngram
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 固定 section 落成独立列而非一个 JSON blob：`body_understanding` 是质量分界线
-- （该栏为空的卡只是摘要，不是合格卡片），它必须能被查询和统计。
-- 塞进 JSON 就只能靠应用层逐条解析，"有多少张卡还只是摘要"这个问题会变得昂贵。
--
-- status 三态（2026-08-07 拍板，取代原设计的 draft/verifying/confirmed/deprecated）：
--   draft      —— 摄取产生，不进默认检索，需人过一遍
--   active     —— 手写默认值，作者自管，正常可检索
--   deprecated —— 过时但保留追溯，检索时降权
-- 砍掉 verifying/confirmed 的理由：0806 已拍板"能写就能审、无独立审核实体、作者自管"，
-- 没有任何机制会推动一张卡从 draft 流转到 verifying 再到 confirmed，
-- 留着它们只会得到两个永远查不到的枚举值。
CREATE TABLE cards (
  id              CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id       CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  space_id        CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  author_user_id  CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  author_agent_id CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NULL,
  title           VARCHAR(255) NOT NULL,
  card_type       VARCHAR(64)  NOT NULL,       -- 如「Codex 协作安全」
  status          VARCHAR(16)  NOT NULL,       -- draft | active | deprecated
  source          TEXT         NULL,
  body_original      MEDIUMTEXT NULL,          -- 作者原意
  body_distilled     MEDIUMTEXT NULL,          -- 提炼
  body_understanding MEDIUMTEXT NULL,          -- 理解（空 = 只是摘要，非合格卡片）
  body_why_keep      MEDIUMTEXT NULL,          -- 为什么值得保留
  body_unresolved    MEDIUMTEXT NULL,          -- 未解决问题
  copied_from_type VARCHAR(16) NULL,
  copied_from_id   CHAR(26)    CHARACTER SET ascii COLLATE ascii_bin NULL,
  version         INT          NOT NULL,
  created_at      DATETIME(3)  NOT NULL,
  updated_at      DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_cards_space_status (space_id, status, updated_at),
  KEY idx_cards_author (author_user_id),
  -- 全文索引刻意不含 body_original：原文是未经整理的现场记录，噪音最多，
  -- 把它纳入索引会让"提炼"和"理解"这两栏的信号被稀释。
  FULLTEXT KEY ft_cards (title, body_distilled, body_understanding) WITH PARSER ngram
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 结构化召回条件（§3.3）——本产品的核心技术优势，必须独立建模。
--
-- 每条触发条件单独存行、将来单独 embedding：整卡一个向量会让「准备删除数据时」
-- 这个信号淹没在五十行复盘细节里；一条一个向量，agent 的当前情境直接对上，
-- 命中任一条即召回整卡。
--
-- space_id 是**有意冗余**：card_recall 的第一段粗筛只需要触发文本和空间权限，
-- 冗余这一列即可不 JOIN cards。labels 表不做同样的冗余，因为按标签检索
-- 无论如何都要回表取标题正文，那次 JOIN 省不掉（见下）。
CREATE TABLE card_recall_triggers (
  id            CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  card_id       CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  space_id      CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  trigger_text  VARCHAR(512) NOT NULL,
  sort_order    INT          NOT NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  KEY idx_triggers_card (card_id),
  KEY idx_triggers_space (space_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 多态引用：entity_type ∈ {memory, card}。
--
-- 权衡（§3.1，有意为之）：MySQL 无法为多态引用建外键，引用完整性由领域层保证。
-- 换来的是 label / link 两套逻辑各只写一份；将来若出现第三种内容类型，
-- 不必再复制一遍这两张表。与 0001 的"全库无外键"是同一套做法，
-- 不会造成"一半有约束一半没有"的心智负担。
--
-- 主键即 (entity_type, entity_id, label)：同一实体同一标签只能有一行，
-- 重复打标签是幂等的而不是产生第二行。
CREATE TABLE labels (
  tenant_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  entity_type   VARCHAR(16)  NOT NULL,
  entity_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  label         VARCHAR(64)  NOT NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (entity_type, entity_id, label),
  KEY idx_labels_lookup (tenant_id, label)     -- 按标签粗筛
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 有向关联。kind 取值与 Desktop 的 memoria_link 保持一致（03-mcp-contract.md §2）：
-- related | causes | evolved-from | contradicts | part-of。
--
-- 唯一键含 kind：同两个实体之间可以既 related 又 contradicts，
-- 那是两条不同的事实，不该互相覆盖。
CREATE TABLE links (
  id            CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  from_type     VARCHAR(16)  NOT NULL,
  from_id       CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  to_type       VARCHAR(16)  NOT NULL,
  to_id         CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  kind          VARCHAR(32)  NOT NULL,
  note          TEXT         NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_links (from_type, from_id, to_type, to_id, kind),
  KEY idx_links_to (to_type, to_id)            -- 反向查「谁指向了我」
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
