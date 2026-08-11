-- 0006_audit_logs.sql —— 活动流与不可抵赖的治理审计
--
-- 这张表同时承载两种保留策略不同、可见性也不同的记录：
--   * trace：MCP / Web 调用轨迹，供成员看清自己的 agent 做过什么；
--   * governance / meta_audit：治理动作与管理员审计查询，长期保留。
--
-- 可见性不编码进表，也不靠模板层事后过滤。读取时由 SQL 直接执行：
--   * 本人的全部活动可见；
--   * 他人的记录只在 access = 'write' 且 space_id 属于调用者可读的共享空间时可见；
--   * 管理员审计按 tenant_id 读取全量，审计查询本身写 meta_audit。
--
-- user_id 必填：第一版只记录已认证主体的动作。agent_id 可空，因为 Web 治理动作
-- 没有 agent；entity 与 space 也都是可选维度。无外键、无触发器，保持既有迁移约定。

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

  -- 管理员的租户级时间线。
  KEY idx_audit_tenant_created (tenant_id, created_at, id),

  -- 普通成员可见性查询的两个分支：自己的全部记录，与共享空间里的他人写入。
  KEY idx_audit_user_created (tenant_id, user_id, created_at, id),
  KEY idx_audit_space_write (tenant_id, space_id, access, created_at, id),

  -- trace 只保留 30 天；治理审计与 meta-audit 不进这条删除路径。
  KEY idx_audit_trace_retention (kind, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
