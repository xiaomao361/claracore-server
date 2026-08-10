-- 0005_ingest.sql —— 摄取的幂等锚点
--
-- 对应 docs/04-web-scope.md §6「摄取必须做成幂等」。全局约定见 0001 头部注释。
--
-- # 为什么需要一列而不是靠应用层查一次
--
-- 幂等的含义是「同一个源文档反复摄取，得到同一张卡片而不是 N 张」。
-- 靠应用层 SELECT-then-INSERT 保证这件事，在并发下必然失效——
-- 0002 建 spaces 的生成列唯一索引时已经写过同一条教训。摄取尤其如此：
-- 它天生是批量的，而「源文档更新了要重扫」意味着它会被反复触发。
--
-- # 为什么是 source_key 而不是直接用 source 列
--
-- `source` 是给人看的自由文本（"0731 复盘"、某个 URL、某段说明）。
-- 拿它当唯一键有两个问题：TEXT 列建唯一索引要指定前缀长度，
-- 而前缀相同的两个不同来源会被误判成同一个；更要紧的是，
-- 人手写的卡片也会填 source，它们不该被卷进摄取的幂等约束里。
--
-- source_key 是**摄取器算出来的稳定标识**（源路径的 SHA-256），
-- 只有摄取产物才有。手写卡片这一列为 NULL。
--
-- # NULL 在唯一键里是自由的
--
-- MySQL 的唯一索引允许多行 NULL。于是这一个约束同时做到两件事：
--   * 摄取产物：同一空间同一源只能有一张卡（想要的幂等）
--   * 手写卡片：source_key 全是 NULL，互不冲突（完全不受影响）
--
-- 与 0001 里 spaces 用生成列做「每用户一个 personal」是同一个手法：
-- 让数据库只约束该约束的那部分行。

ALTER TABLE cards
  ADD COLUMN source_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL
    COMMENT '摄取源的稳定标识（SHA-256），手写卡片为 NULL',
  -- 内容指纹：源文档没变就整张卡都不用动（不动 version、不重算向量）。
  -- 没有它的话，每次重扫都会把全部卡片的 version 推高一格、
  -- 把全部向量作废重算——那是真金白银的外部调用。
  ADD COLUMN source_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL
    COMMENT '源内容的 SHA-256，用于判断这次重扫要不要更新',
  ADD UNIQUE KEY uk_cards_source (space_id, source_key);
