-- 0003_embeddings.sql —— 运行时域：向量与索引队列
--
-- 对应 docs/02-data-model.md §5、§7.3。全局约定见 0001_governance.sql 头部注释。
--
-- 0002 刻意把这张表留到实现检索时再建（2026-08-07 拍板）：向量维度、模型灰度、
-- pending/failed 的重试语义，只有在真的写检索时才看得清楚。落地时确实改了三处
-- 草案，每一处都是写代码才暴露出来的：
--
--   1. vector 改为可空。草案写的是 `BLOB NOT NULL`，那隐含「行存在即向量已算好」。
--      但写入路径不能依赖外部 embedding 服务（Ollama 挂了照样要能记东西），
--      所以这张表同时是**索引队列**：写入事务里落一行 pending，向量后台补。
--      NOT NULL 会逼我们用一个空 BLOB 冒充「还没算」，那是个会骗人的值。
--
--   2. 增加 source_text。草案没有这一列，索引 worker 得按 entity_type 回去
--      load memory / card / trigger 再重新拼文本——多态分发写三遍、实体在入队
--      与索引之间被改还会让「算的是哪个版本」说不清。存下来之后，worker 是
--      这张表的纯函数，且「这个向量到底是照着哪段文本算的」可以直接查。
--      它不是正文的副本：领域层按 MaxEmbedTextLen 截断后才写进来——embedding
--      模型本身有上下文上限，与其让模型静默截断，不如我们显式截断并存证。
--
--   3. 不建 idx_emb_hash。草案留了它，用途是「另一个实体文本完全相同就复用向量」。
--      那个优化是真的（promote 复制卡片会产生一模一样的触发条件），但本版没做，
--      而没有查询会用到的索引只会让每次写入白维护一棵 B 树。content_hash 列保留：
--      入队时比对它就能回答「内容变了没有」，那条路径走的是唯一键不是它自己。
--
-- 为什么没有 space_id：检索的权限过滤发生在**第一段粗筛**（memories / cards /
-- card_recall_triggers 上的 space_id IN (...)），到第二段精算时候选实体已经全部
-- 过过权限了，这里只按 entity_id 批量取向量。在这张表上再放一份 space_id，
-- 等于给权限过滤开第二个可以走的入口——而它迟早会和第一个不一致。

CREATE TABLE embeddings (
  id            CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  entity_type   VARCHAR(24)  NOT NULL,       -- memory | card | card_trigger
  entity_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  model         VARCHAR(64)  NOT NULL,       -- 换模型时新旧共存，可灰度重建
  content_hash  CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,  -- SHA-256(source_text)
  source_text   TEXT         NOT NULL,       -- 真正被算成向量的那段文本（已截断）
  vector        BLOB         NULL,           -- float32 小端紧凑存储，非 JSON；pending 时为 NULL
  dim           INT          NOT NULL,       -- 未算出时为 0
  status        VARCHAR(16)  NOT NULL,       -- pending | ready | failed
  attempts      INT          NOT NULL,       -- 连续失败次数，超过上限即不再重试
  last_error    VARCHAR(512) NULL,           -- 截断后的失败原因，给运维看
  created_at    DATETIME(3)  NOT NULL,
  updated_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),

  -- 一个实体在一个模型下只有一行。换模型是新增行而不是覆盖，
  -- 灰度期间新旧向量共存，检索按当前模型取。
  UNIQUE KEY uk_emb (entity_type, entity_id, model),

  -- 索引 worker 领活的唯一路径：按模型取还没算好的，最久没动的排前面。
  -- updated_at 进索引是为了让失败重试自然排到队尾——失败一次就更新一次时间戳，
  -- 于是它不会霸着队头把整批活卡死。
  KEY idx_emb_queue (model, status, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- card_recall 的粗筛补一个全文索引。
--
-- 0002 建这张表时没给它全文索引，因为当时召回被设想成「纯靠向量」。写检索时
-- 发现那样有两个洞：
--
--   * 候选集截断没有依据。docs/02-data-model.md §5.1 明确要求候选超过上限时
--     「按非向量信号排序后截断，而非随机丢弃」——触发条件上一个非向量信号都没有，
--     只能按 created_at 截，等于按卡片创建时间决定谁有资格被召回。
--   * 没有降级路径。CCS_EMBEDDING_PROVIDER=disabled 时 card_recall 会一条都不返回，
--     而「降级是默认路径不是异常路径」（docs/01-architecture.md §6.1）。
--
-- 有了它，粗筛用 MATCH 分数排序后截断，向量关掉时整条召回退化成中文全文匹配——
-- 精度下降，但不是失灵。WITH PARSER ngram 是硬要求（schema 规则 7），
-- 少了它中文整段会被当成一个词，索引建得好好的但 MATCH 永远返回空。
ALTER TABLE card_recall_triggers
  ADD FULLTEXT KEY ft_triggers (trigger_text) WITH PARSER ngram;
