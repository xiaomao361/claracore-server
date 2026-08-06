-- 0001_governance.sql —— 治理域：谁是谁，谁能干什么
--
-- 对应 docs/02-data-model.md §7.1。
--
-- 全局约定（新增迁移一律遵守，由 test/schema_test.go 强制）：
--   * 主键与所有 ID 列 —— CHAR(26) ULID，显式 `CHARACTER SET ascii COLLATE ascii_bin`。
--     表默认字符集仍是 utf8mb4（中文列需要），但 ID 与 hash 是纯 ASCII：
--     ai_ci 下 CHAR(26) 在索引里按 4 字节/字符计（104 字节），ascii_bin 只占 26 字节，
--     且比较是字节比较，无大小写折叠——ULID 是大写 Crockford base32，折叠没有意义。
--   * 时间列 —— DATETIME(3)，一律存 UTC。禁止 TIMESTAMP。
--   * 作用域列（tenant_id / space_id）—— 无默认值，缺失即报错。
--   * 零触发器，业务逻辑全在应用层。
--   * 无外键：引用完整性由领域层保证（与核心域的多态引用保持同一套做法，
--     避免"一半有约束一半没有"的心智负担）。
--
-- 身份层级：tenant → user → agent → token
-- 权限只挂 user，agent 继承其所有者的全部权限（§2.1）。

CREATE TABLE tenants (
  id            CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  name          VARCHAR(128) NOT NULL,
  status        VARCHAR(16)  NOT NULL,                  -- active | suspended
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE users (
  id             CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id      CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  username       VARCHAR(64)  NOT NULL,
  password_hash  VARCHAR(255) NOT NULL,                 -- argon2id，含参数与盐
  display_name   VARCHAR(128) NOT NULL,
  role           VARCHAR(16)  NOT NULL,                 -- admin | member
  must_change_pw TINYINT(1)   NOT NULL,                 -- 首次登录强制改密
  status         VARCHAR(16)  NOT NULL,                 -- active | disabled
  created_at     DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_users_tenant_username (tenant_id, username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- agent 是审计维度，不是权限维度：这张表不参与任何鉴权判断，
-- 唯一作用是回答「这条内容是谁的哪个 agent 写的」。
CREATE TABLE agents (
  id            CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id       CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  slug          VARCHAR(64)  NOT NULL,                  -- clara / codex / hermes
  display_name  VARCHAR(128) NOT NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_agents_user_slug (user_id, slug),
  KEY idx_agents_tenant (tenant_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 身份的唯一来源。绝不信任客户端声明的 header（Desktop 的 X-ClaraCore-Agent-ID 教训）。
-- idx_tokens_agent 刻意不是唯一键：一个 agent 多 token 并存，是平滑轮换的前提。
CREATE TABLE tokens (
  id            CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id       CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  agent_id      CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  name          VARCHAR(128) NOT NULL,                  -- 用户可读的标识，如 "MacBook"
  token_prefix  VARCHAR(16)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,  -- 明文前缀，列表展示与日志脱敏
  token_hash    CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,  -- SHA-256 hex，明文仅创建时返回一次
  expires_at    DATETIME(3)      NULL,
  last_used_at  DATETIME(3)      NULL,                  -- 判断可否安全吊销
  revoked_at    DATETIME(3)      NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_tokens_hash (token_hash),
  KEY idx_tokens_agent (agent_id),
  KEY idx_tokens_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 三层空间同表，用 type 区分（§2.2）。
--
-- 两个生成列把设计约束下沉到数据库：
--   * 每用户恰好一个 personal 空间
--   * 每租户恰好一个 team 空间
-- 靠应用层 SELECT-then-INSERT 保证这两条会在并发创建时失效（§6 明确要求 Server 处理并发），
-- 而 project 空间同一 owner 可有多个，所以不能直接对 (tenant_id, type, owner_user_id) 建唯一键。
CREATE TABLE spaces (
  id            CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  type          VARCHAR(16)  NOT NULL,                  -- personal | project | team
  name          VARCHAR(128) NOT NULL,
  owner_user_id CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NULL,  -- personal/project 有主，team 为 NULL
  created_at    DATETIME(3)  NOT NULL,

  personal_owner_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin
                    GENERATED ALWAYS AS (IF(type = 'personal', owner_user_id, NULL)) VIRTUAL,
  team_tenant_id    CHAR(26) CHARACTER SET ascii COLLATE ascii_bin
                    GENERATED ALWAYS AS (IF(type = 'team', tenant_id, NULL)) VIRTUAL,

  PRIMARY KEY (id),
  UNIQUE KEY uk_spaces_personal (personal_owner_id),
  UNIQUE KEY uk_spaces_team (team_tenant_id),
  KEY idx_spaces_tenant_type (tenant_id, type),
  KEY idx_spaces_owner (owner_user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 权限只挂 user，只分读写。与 tokens 表无任何交叉——新增 agent 不需要配权限。
-- idx_space_perm_user 服务于「我能读哪些空间」，这是每次检索粗筛前的必经查询。
CREATE TABLE space_permissions (
  space_id      CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id       CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  can_read      TINYINT(1)   NOT NULL,
  can_write     TINYINT(1)   NOT NULL,
  granted_by    CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  created_at    DATETIME(3)  NOT NULL,
  PRIMARY KEY (space_id, user_id),
  KEY idx_space_perm_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
