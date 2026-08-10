-- 0004_sessions.sql —— Web 登录会话
--
-- 对应 docs/04-web-scope.md §5。全局约定见 0001_governance.sql 头部注释。
--
-- 会话与 MCP 的 bearer token **彻底分开**（01-architecture.md §5.3）：
-- 两条入口、两套凭据、互不影响。合用一张表会立刻带来两个坏结果——
-- 浏览器里退出登录会顺手废掉机器上正在跑的 agent；而 token 的
-- 「一个 agent 多个并存、可单独吊销」与会话的「登出即失效」
-- 是两种不同的生命周期，硬塞进一张表就得加一列 kind 去分叉所有查询。
--
-- 存数据库而不是签名 cookie（JWT 那一路）：会话必须能被**立即吊销**——
-- 停用一个员工账号时，他浏览器里那张已签发的票必须当场失效，
-- 而不是等它自己过期。这是治理面的基本要求，也是 SessionStore
-- 默认实现落在数据库、而不是内存的原因（多实例部署时内存实现会
-- 让人在实例之间反复掉线）。

CREATE TABLE sessions (
  -- id 是会话令牌的 **SHA-256**，不是令牌本身。
  --
  -- 与 tokens 表同一条纪律：库里存哈希，明文只在签发时进一次 cookie。
  -- 数据库被读走时，攻击者拿到的是一堆无法用于登录的哈希。
  id            CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  tenant_id     CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id       CHAR(26)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,

  -- 会话不绑 agent：Web 是人在用（04 §1.3 的工作面/治理面切分）。
  -- 这也是它和 tokens 表最本质的区别。

  expires_at    DATETIME(3)  NOT NULL,
  created_at    DATETIME(3)  NOT NULL,

  -- last_seen_at 用于「滑动过期」：还在用的会话不该在开着的标签页里
  -- 突然掉线。与 tokens.last_used_at 一样有写入节流，且写失败不影响认证——
  -- 它是运维辅助信号，不是授权依据。
  last_seen_at  DATETIME(3)  NOT NULL,

  PRIMARY KEY (id),
  -- 「停用某人时废掉他的全部会话」的查询路径。这是 sessions 存库
  -- 而非签名 cookie 的全部意义所在，所以这个索引不是可选项。
  KEY idx_sessions_user (user_id),
  -- 过期清理走这条
  KEY idx_sessions_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
