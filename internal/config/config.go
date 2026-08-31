// Package config 是整个进程读取环境变量的唯一位置。
//
// 其他任何包禁止调用 os.Getenv —— 由 test/arch_test.go 保障。
// 配置在启动时一次性加载并校验，校验失败直接 fatal，
// 绝不允许运行期才发现配置缺失（docs/05-code-structure.md §6.3）。
package config

import (
	"bufio"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env           string
	InstanceID    string
	Addr          string
	ShutdownDrain time.Duration
	HTTP          HTTP
	Metrics       Metrics
	MySQL         MySQL
	Redis         Redis
	Embedding     Embedding
	External      External
	AutoContext   AutoContext
	Password      Password
	Web           Web
}

// Metrics is a dedicated machine-scrape listener. It is never mounted on the
// public Web/MCP listener and may bind only a numeric loopback address.
type Metrics struct {
	Addr string
}

func (m Metrics) Enabled() bool { return m.Addr != "" }

// HTTP bounds resources at the application listener. Nginx mirrors these
// limits but is not a trust boundary: local misconfiguration or direct
// loopback callers must not be able to disable them.
type HTTP struct {
	ReadHeaderTimeout  time.Duration
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	IdleTimeout        time.Duration
	MaxHeaderBytes     int
	WebMaxFormBytes    int64
	MCPMaxBodyBytes    int64
	MCPRateLimit       int
	MCPTenantRateLimit int
	MCPRateWindow      time.Duration
}

// Web 是治理面的配置。
type Web struct {
	// PublicMCPURL 是发给用户复制的 MCP 连接地址。
	//
	// 必须可配：服务监听 127.0.0.1:51890，而用户要填进 agent 配置的是
	// nginx 后面那个对外地址。拿监听地址拼一个出来，等于给每个人
	// 发一份连不上的配置。
	PublicMCPURL string

	// SecureCookie 决定会话 cookie 带不带 Secure 标记。
	//
	// 生产必须为真。本地开发走 http 时为真会让 cookie 根本存不下来，
	// 症状是「登录成功但立刻又跳回登录页」——一个没人第一次就能猜对的
	// 现象，所以默认值跟着 CCS_ENV 走，而不是让人自己想起来配。
	SecureCookie bool

	// IngestRoot 是 Web 管理员可扫描的唯一文件树。页面只接受它下面的
	// 相对目录；留空即关闭 Web 摄取，避免表单退化成本机任意文件读取器。
	IngestRoot string

	// LoginRateLimit is the per client-IP + username attempt budget. A second
	// per-IP budget is derived at six times this value to bound username spray.
	LoginRateLimit  int
	LoginRateWindow time.Duration

	// ExportMaxBytes bounds the complete portable tenant ZIP spooled for one
	// administrator download. It limits temporary disk use, not database backup.
	ExportMaxBytes int64

	// BackupStatusFile is the bounded verification receipt written by the
	// scheduled backup unit. Empty disables dashboard integration without
	// disabling backup itself.
	BackupStatusFile string

	// RestoreStatusFile is the bounded technical restore receipt copied back
	// from an isolated rehearsal environment. Empty disables dashboard
	// integration; it does not turn a backup checksum into restore evidence.
	RestoreStatusFile string

	// IntegrityStatusFile is atomically published by -check-integrity and read
	// by the normal server for dashboards and metrics. MaxAge defines when the
	// last completed check stops being current operational evidence.
	IntegrityStatusFile string
	IntegrityMaxAge     time.Duration
}

// Password 是 argon2id 的哈希参数。
//
// 它在这里而不是在领域层，正是「领域层不读配置」这条铁律的直接后果：
// 参数该配多大取决于这台机器有多少内存、能容忍多长的登录延迟，
// 那是部署决定，不是业务规则（见 ports.PasswordHasher 的注释）。
//
// 默认值取 OWASP 对 argon2id 的推荐档之一。调高之后**存量用户仍能登录**——
// 参数编码在每个 hash 串里，验的时候用它自己记的那份。
type Password struct {
	MemoryKiB   int
	Iterations  int
	Parallelism int
}

type MySQL struct {
	Host            string
	Port            int
	User            string
	Password        string
	Database        string
	TLS             ClientTLS
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// DSN 返回 go-sql-driver/mysql 的连接串。
//
// 时间处理三重锁定 UTC（docs/02-data-model.md §7 的"存储层统一 UTC"）：
//   - loc=UTC          驱动把 DATETIME 解析为 UTC 的 time.Time
//   - time_zone='+00:00' 连接会话时区固定，NOW() 等函数返回 UTC
//   - 列类型用 DATETIME(3) 而非 TIMESTAMP（由 test/schema_test.go 强制）
//
// Desktop 的 localDateForTimezone 教训：多设备跨时区时本地时区会让
// 同一条记录在不同设备显示成不同日期。
func (m MySQL) DSN() string {
	return fmt.Sprintf(
		"%s:%s@tcp(%s)/%s?charset=utf8mb4&collation=utf8mb4_0900_ai_ci&parseTime=true&loc=UTC&time_zone=%%27%%2B00%%3A00%%27",
		m.User, m.Password, net.JoinHostPort(strings.Trim(m.Host, "[]"), strconv.Itoa(m.Port)), m.Database,
	)
}

type Redis struct {
	Addr     string
	Password string
	Prefix   string
	Timeout  time.Duration
	TLS      ClientTLS
}

// ClientTLS is shared by stateful dependency clients. Only verified TLS is
// supported: encryption without server identity verification is forgeable and
// must not look like a production-safe option.
type ClientTLS struct {
	Mode       string
	CAFile     string
	CertFile   string
	KeyFile    string
	ServerName string
}

func (t ClientTLS) Enabled() bool { return t.Mode == TLSVerifyFull }

const (
	TLSDisabled   = "disabled"
	TLSVerifyFull = "verify-full"
)

// Enabled 表示是否配置了 Redis。未配置时所有相关端口降级为
// 内存/数据库实现，系统功能保持完整（docs/01-architecture.md §6.1）。
func (r Redis) Enabled() bool { return r.Addr != "" }

type Embedding struct {
	Provider string // ollama | openai-compatible | disabled
	BaseURL  string
	Model    string
	APIKey   string

	// Timeout 是单次批量调用的上限，同时也是索引 worker 卡住的上限。
	Timeout time.Duration

	// IndexInterval 是后台索引 worker 两轮之间的间隔。
	//
	// 它决定了「刚写完的内容多久之后能被语义检索到」。做成可配是因为
	// 这个取舍真的因部署而异：单人试用希望它小到几乎无感，
	// 大批量摄取时希望它大一点、让每一批攒满再发。
	IndexInterval time.Duration

	// IndexBatch 是一轮处理多少条。
	IndexBatch int

	// CacheEntries 为 0 时关闭进程内缓存。TTL 只控制重新校准周期；
	// key 已包含 model，因此换模型不会复用旧向量。
	CacheEntries int
	CacheTTL     time.Duration
}

type AutoContext struct {
	MinScore  float64
	MinMargin float64
}

type External struct {
	Atlas AtlasExternal
}

// AtlasExternal is an optional deployment-level live-source registration.
// Credentials stay in process configuration; only body-free source metadata is
// persisted in MySQL.
type AtlasExternal struct {
	Endpoint     string
	Token        string
	System       string
	Name         string
	SpaceID      string
	SyncInterval time.Duration
	SyncTimeout  time.Duration
	Timeout      time.Duration
	PageSize     int
	MaxWireBytes int64
	MaxBodyBytes int
}

func (a AtlasExternal) Enabled() bool { return a.Endpoint != "" }

// Enabled 表示是否配了真正的 embedding 服务。
//
// 未启用时不入索引队列、检索退化成中文全文匹配，系统功能保持完整
// ——降级是默认路径不是异常路径（docs/01-architecture.md §6.1）。
func (e Embedding) Enabled() bool { return e.Provider != "" && e.Provider != ProviderDisabled }

const (
	ProviderOllama       = "ollama"
	ProviderOpenAICompat = "openai-compatible"
	ProviderDisabled     = "disabled"
)

// Load 从环境变量加载配置。若存在 .env 文件则先载入（不覆盖已有环境变量）。
func Load() (*Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return nil, fmt.Errorf("读取 .env 失败: %w", err)
	}
	r := envReader{}
	environment := r.str("CCS_ENV", "development")

	c := &Config{
		Env:           environment,
		InstanceID:    r.str("CCS_INSTANCE_ID", ""),
		Addr:          r.str("CCS_ADDR", "127.0.0.1:51890"),
		ShutdownDrain: r.duration("CCS_SHUTDOWN_DRAIN_SEC", 5, time.Second),
		HTTP: HTTP{
			ReadHeaderTimeout:  r.duration("CCS_HTTP_READ_HEADER_TIMEOUT_SEC", 10, time.Second),
			ReadTimeout:        r.duration("CCS_HTTP_READ_TIMEOUT_SEC", 30, time.Second),
			WriteTimeout:       r.duration("CCS_HTTP_WRITE_TIMEOUT_SEC", 300, time.Second),
			IdleTimeout:        r.duration("CCS_HTTP_IDLE_TIMEOUT_SEC", 120, time.Second),
			MaxHeaderBytes:     r.integer("CCS_HTTP_MAX_HEADER_BYTES", 64<<10),
			WebMaxFormBytes:    int64(r.integer("CCS_WEB_MAX_FORM_BYTES", 1<<20)),
			MCPMaxBodyBytes:    int64(r.integer("CCS_MCP_MAX_BODY_BYTES", 4<<20)),
			MCPRateLimit:       r.integer("CCS_MCP_RATE_LIMIT", 300),
			MCPTenantRateLimit: r.integer("CCS_MCP_TENANT_RATE_LIMIT", 3000),
			MCPRateWindow:      r.duration("CCS_MCP_RATE_WINDOW_SEC", 60, time.Second),
		},
		Metrics: Metrics{Addr: r.str("CCS_METRICS_ADDR", "")},
		MySQL: MySQL{
			Host:     r.str("CCS_MYSQL_HOST", ""),
			Port:     r.integer("CCS_MYSQL_PORT", 3306),
			User:     r.str("CCS_MYSQL_USER", ""),
			Password: r.str("CCS_MYSQL_PASSWORD", ""),
			Database: r.str("CCS_MYSQL_DATABASE", ""),
			TLS: ClientTLS{
				Mode:       r.str("CCS_MYSQL_TLS_MODE", TLSDisabled),
				CAFile:     r.str("CCS_MYSQL_TLS_CA_FILE", ""),
				CertFile:   r.str("CCS_MYSQL_TLS_CERT_FILE", ""),
				KeyFile:    r.str("CCS_MYSQL_TLS_KEY_FILE", ""),
				ServerName: r.str("CCS_MYSQL_TLS_SERVER_NAME", ""),
			},
			MaxOpenConns:    r.integer("CCS_MYSQL_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    r.integer("CCS_MYSQL_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: r.duration("CCS_MYSQL_CONN_MAX_LIFETIME_SEC", 1800, time.Second),
		},
		Redis: Redis{
			Addr:     r.str("CCS_REDIS_ADDR", ""),
			Password: r.str("CCS_REDIS_PASSWORD", ""),
			Prefix:   r.str("CCS_REDIS_PREFIX", "ccs:"),
			Timeout:  r.duration("CCS_REDIS_TIMEOUT_MS", 500, time.Millisecond),
			TLS: ClientTLS{
				Mode:       r.str("CCS_REDIS_TLS_MODE", TLSDisabled),
				CAFile:     r.str("CCS_REDIS_TLS_CA_FILE", ""),
				CertFile:   r.str("CCS_REDIS_TLS_CERT_FILE", ""),
				KeyFile:    r.str("CCS_REDIS_TLS_KEY_FILE", ""),
				ServerName: r.str("CCS_REDIS_TLS_SERVER_NAME", ""),
			},
		},
		Password: Password{
			MemoryKiB:   r.integer("CCS_ARGON2_MEMORY_KIB", 19*1024),
			Iterations:  r.integer("CCS_ARGON2_ITERATIONS", 2),
			Parallelism: r.integer("CCS_ARGON2_PARALLELISM", 1),
		},
		Web: Web{
			PublicMCPURL:        r.str("CCS_PUBLIC_MCP_URL", ""),
			SecureCookie:        r.boolean("CCS_WEB_SECURE_COOKIE", environment == "production"),
			IngestRoot:          r.str("CCS_INGEST_ROOT", ""),
			LoginRateLimit:      r.integer("CCS_LOGIN_RATE_LIMIT", 10),
			LoginRateWindow:     r.duration("CCS_LOGIN_RATE_WINDOW_SEC", 900, time.Second),
			ExportMaxBytes:      int64(r.integer("CCS_EXPORT_MAX_BYTES", 1<<30)),
			BackupStatusFile:    r.str("CCS_BACKUP_STATUS_FILE", ""),
			RestoreStatusFile:   r.str("CCS_RESTORE_STATUS_FILE", ""),
			IntegrityStatusFile: r.str("CCS_INTEGRITY_STATUS_FILE", ""),
			IntegrityMaxAge:     r.duration("CCS_INTEGRITY_MAX_AGE_SEC", 90_000, time.Second),
		},
		Embedding: Embedding{
			Provider:      r.str("CCS_EMBEDDING_PROVIDER", ProviderDisabled),
			BaseURL:       r.str("CCS_EMBEDDING_BASE_URL", ""),
			Model:         r.str("CCS_EMBEDDING_MODEL", ""),
			APIKey:        r.str("CCS_EMBEDDING_API_KEY", ""),
			Timeout:       r.duration("CCS_EMBEDDING_TIMEOUT_SEC", 30, time.Second),
			IndexInterval: r.duration("CCS_EMBEDDING_INDEX_INTERVAL_SEC", 5, time.Second),
			IndexBatch:    r.integer("CCS_EMBEDDING_INDEX_BATCH", 32),
			CacheEntries:  r.integer("CCS_EMBEDDING_CACHE_ENTRIES", 1024),
			CacheTTL:      r.duration("CCS_EMBEDDING_CACHE_TTL_SEC", 86400, time.Second),
		},
		External: External{Atlas: AtlasExternal{
			Endpoint:     r.str("CCS_ATLAS_MCP_URL", ""),
			Token:        r.str("CCS_ATLAS_MCP_TOKEN", ""),
			System:       r.str("CCS_ATLAS_SYSTEM", ""),
			Name:         r.str("CCS_ATLAS_SOURCE_NAME", "atlas-docs"),
			SpaceID:      r.str("CCS_ATLAS_SPACE_ID", ""),
			SyncInterval: r.duration("CCS_ATLAS_SYNC_INTERVAL_SEC", 900, time.Second),
			SyncTimeout:  r.duration("CCS_ATLAS_SYNC_TIMEOUT_SEC", 600, time.Second),
			Timeout:      r.duration("CCS_ATLAS_TIMEOUT_SEC", 30, time.Second),
			PageSize:     r.integer("CCS_ATLAS_PAGE_SIZE", 100),
			MaxWireBytes: int64(r.integer("CCS_ATLAS_MAX_WIRE_BYTES", 8<<20)),
			MaxBodyBytes: r.integer("CCS_ATLAS_MAX_BODY_BYTES", 2<<20),
		}},
		AutoContext: AutoContext{
			MinScore:  r.number("CCS_AUTO_CONTEXT_MIN_SCORE", 0.55),
			MinMargin: r.number("CCS_AUTO_CONTEXT_MIN_MARGIN", 0.03),
		},
	}
	if err := r.err(); err != nil {
		return nil, err
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	var missing []string
	if c.MySQL.Host == "" {
		missing = append(missing, "CCS_MYSQL_HOST")
	}
	if c.MySQL.User == "" {
		missing = append(missing, "CCS_MYSQL_USER")
	}
	if c.MySQL.Database == "" {
		missing = append(missing, "CCS_MYSQL_DATABASE")
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少必需配置: %s（参考 .env.example）", strings.Join(missing, ", "))
	}
	if c.InstanceID != "" && !validInstanceID(c.InstanceID) {
		return fmt.Errorf("CCS_INSTANCE_ID 必须以字母或数字开头，仅含字母、数字、点、下划线或短横线，且不超过 64 字节")
	}
	_, listenPort, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("CCS_ADDR 必须是 host:port: %w", err)
	}
	parsedListenPort, err := strconv.Atoi(listenPort)
	if err != nil || parsedListenPort < 1 || parsedListenPort > 65535 {
		return fmt.Errorf("CCS_ADDR 的端口必须在 1 到 65535 之间")
	}
	if c.MySQL.Port < 1 || c.MySQL.Port > 65535 {
		return fmt.Errorf("CCS_MYSQL_PORT 必须在 1 到 65535 之间")
	}
	if err := validateClientTLS("CCS_MYSQL_TLS", &c.MySQL.TLS, c.MySQL.Host); err != nil {
		return err
	}
	if c.Env == "production" && !localDependencyHost(c.MySQL.Host) && !c.MySQL.TLS.Enabled() {
		return fmt.Errorf("CCS_ENV=production 连接非 loopback MySQL 时 CCS_MYSQL_TLS_MODE 必须为 verify-full")
	}
	if c.MySQL.MaxOpenConns <= 0 || c.MySQL.MaxOpenConns > 10_000 || c.MySQL.MaxIdleConns < 0 || c.MySQL.MaxIdleConns > c.MySQL.MaxOpenConns {
		return fmt.Errorf("MySQL 连接池要求 1 <= MAX_OPEN_CONNS <= 10000 且 0 <= MAX_IDLE_CONNS <= MAX_OPEN_CONNS")
	}
	if c.MySQL.ConnMaxLifetime <= 0 || c.MySQL.ConnMaxLifetime > 24*time.Hour {
		return fmt.Errorf("CCS_MYSQL_CONN_MAX_LIFETIME_SEC 必须在 1 到 86400 之间")
	}
	if c.ShutdownDrain < 0 || c.ShutdownDrain > time.Minute {
		return fmt.Errorf("CCS_SHUTDOWN_DRAIN_SEC 必须在 0 到 60 之间")
	}
	if c.HTTP.ReadHeaderTimeout < time.Second || c.HTTP.ReadHeaderTimeout > time.Minute ||
		c.HTTP.ReadTimeout < c.HTTP.ReadHeaderTimeout || c.HTTP.ReadTimeout > 5*time.Minute {
		return fmt.Errorf("HTTP 读取预算要求 1s <= READ_HEADER_TIMEOUT <= 60s 且 READ_HEADER_TIMEOUT <= READ_TIMEOUT <= 300s")
	}
	if c.HTTP.WriteTimeout < c.HTTP.ReadTimeout || c.HTTP.WriteTimeout > 30*time.Minute {
		return fmt.Errorf("CCS_HTTP_WRITE_TIMEOUT_SEC 必须不小于 READ_TIMEOUT 且不超过 1800 秒")
	}
	if c.Embedding.Enabled() && c.HTTP.WriteTimeout < c.Embedding.Timeout {
		return fmt.Errorf("CCS_HTTP_WRITE_TIMEOUT_SEC 不能小于 CCS_EMBEDDING_TIMEOUT_SEC")
	}
	if c.HTTP.IdleTimeout < time.Second || c.HTTP.IdleTimeout > 10*time.Minute {
		return fmt.Errorf("CCS_HTTP_IDLE_TIMEOUT_SEC 必须在 1 到 600 之间")
	}
	if c.HTTP.MaxHeaderBytes < 8<<10 || c.HTTP.MaxHeaderBytes > 1<<20 {
		return fmt.Errorf("CCS_HTTP_MAX_HEADER_BYTES 必须在 8192 到 1048576 之间")
	}
	if c.HTTP.WebMaxFormBytes < 64<<10 || c.HTTP.WebMaxFormBytes > 4<<20 {
		return fmt.Errorf("CCS_WEB_MAX_FORM_BYTES 必须在 65536 到 4194304 之间")
	}
	if c.HTTP.MCPMaxBodyBytes < 64<<10 || c.HTTP.MCPMaxBodyBytes > 16<<20 {
		return fmt.Errorf("CCS_MCP_MAX_BODY_BYTES 必须在 65536 到 16777216 之间")
	}
	if c.HTTP.MCPRateLimit <= 0 || c.HTTP.MCPRateLimit > 100_000 {
		return fmt.Errorf("CCS_MCP_RATE_LIMIT 必须在 1 到 100000 之间")
	}
	if c.HTTP.MCPTenantRateLimit < c.HTTP.MCPRateLimit || c.HTTP.MCPTenantRateLimit > 1_000_000 {
		return fmt.Errorf("CCS_MCP_TENANT_RATE_LIMIT 必须不小于 CCS_MCP_RATE_LIMIT 且不超过 1000000")
	}
	if c.HTTP.MCPRateWindow < time.Second || c.HTTP.MCPRateWindow > 24*time.Hour {
		return fmt.Errorf("CCS_MCP_RATE_WINDOW_SEC 必须在 1 到 86400 之间")
	}
	if c.Web.ExportMaxBytes < 1<<20 || c.Web.ExportMaxBytes > 16<<30 {
		return fmt.Errorf("CCS_EXPORT_MAX_BYTES 必须在 1048576 到 17179869184 之间")
	}
	if c.Metrics.Enabled() {
		host, port, err := net.SplitHostPort(c.Metrics.Addr)
		if err != nil {
			return fmt.Errorf("CCS_METRICS_ADDR 必须是 loopback host:port: %w", err)
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		metricsPort, err := strconv.Atoi(port)
		if ip == nil || !ip.IsLoopback() || err != nil || metricsPort < 1 || metricsPort > 65535 {
			return fmt.Errorf("CCS_METRICS_ADDR 只允许数字 loopback 地址与 1 到 65535 端口")
		}
		if metricsPort == parsedListenPort {
			return fmt.Errorf("CCS_METRICS_ADDR 必须使用与 CCS_ADDR 不同的端口")
		}
	}

	// 没配对外地址时退回监听地址：本地开发正好能用，
	// 而生产上 nginx 后面的部署必须显式配置（否则发出去的配置片段连不上）。
	if c.Web.PublicMCPURL == "" {
		c.Web.PublicMCPURL = "http://" + c.Addr + "/mcp"
	}
	publicURL, err := url.Parse(c.Web.PublicMCPURL)
	if err != nil || publicURL.Host == "" || (publicURL.Scheme != "http" && publicURL.Scheme != "https") {
		return fmt.Errorf("CCS_PUBLIC_MCP_URL 必须是完整的 http(s) URL")
	}
	if publicURL.User != nil {
		return fmt.Errorf("CCS_PUBLIC_MCP_URL 不能在 URL 中携带用户名或密码")
	}
	if c.Env == "production" {
		if !c.Web.SecureCookie {
			return fmt.Errorf("CCS_ENV=production 时 CCS_WEB_SECURE_COOKIE 必须为 true")
		}
		if publicURL.Scheme != "https" {
			return fmt.Errorf("CCS_ENV=production 时 CCS_PUBLIC_MCP_URL 必须使用 https")
		}
	}

	switch c.Embedding.Provider {
	case ProviderOllama, ProviderOpenAICompat:
		if c.Embedding.BaseURL == "" {
			return fmt.Errorf("CCS_EMBEDDING_PROVIDER=%s 时必须设置 CCS_EMBEDDING_BASE_URL", c.Embedding.Provider)
		}
		// 模型名会原样写进 embeddings.model 列，是「这个向量是谁算的」
		// 的唯一记录，也是换模型灰度重建的依据。空着的话整张表都无法区分。
		if c.Embedding.Model == "" {
			return fmt.Errorf("CCS_EMBEDDING_PROVIDER=%s 时必须设置 CCS_EMBEDDING_MODEL", c.Embedding.Provider)
		}
		if c.Embedding.Model != strings.TrimSpace(c.Embedding.Model) || len([]rune(c.Embedding.Model)) > 64 {
			return fmt.Errorf("CCS_EMBEDDING_MODEL 必须是不带首尾空白且不超过 64 个字符的模型名")
		}
		parsed, err := validateDependencyURL("CCS_EMBEDDING_BASE_URL", c.Embedding.BaseURL)
		if err != nil {
			return err
		}
		if c.Env == "production" && parsed.Scheme != "https" && !localDependencyHost(parsed.Hostname()) {
			return fmt.Errorf("CCS_ENV=production 连接非 loopback Embedding 时 CCS_EMBEDDING_BASE_URL 必须使用 https")
		}
	case ProviderDisabled:
	default:
		return fmt.Errorf("CCS_EMBEDDING_PROVIDER 取值非法: %q（可选 ollama | openai-compatible | disabled）", c.Embedding.Provider)
	}
	if math.IsNaN(c.AutoContext.MinScore) || math.IsInf(c.AutoContext.MinScore, 0) ||
		math.IsNaN(c.AutoContext.MinMargin) || math.IsInf(c.AutoContext.MinMargin, 0) ||
		c.AutoContext.MinScore < 0 || c.AutoContext.MinScore > 1 || c.AutoContext.MinMargin < 0 || c.AutoContext.MinMargin > 1 {
		return fmt.Errorf("CCS_AUTO_CONTEXT_MIN_SCORE 与 CCS_AUTO_CONTEXT_MIN_MARGIN 必须在 0 到 1 之间")
	}
	if c.Redis.Timeout <= 0 || c.Redis.Timeout > 30*time.Second {
		return fmt.Errorf("CCS_REDIS_TIMEOUT_MS 必须在 1 到 30000 之间")
	}
	redisHost := ""
	if c.Redis.Enabled() {
		var err error
		redisHost, _, err = net.SplitHostPort(c.Redis.Addr)
		if err != nil {
			return fmt.Errorf("CCS_REDIS_ADDR 必须是 host:port: %w", err)
		}
	}
	if err := validateClientTLS("CCS_REDIS_TLS", &c.Redis.TLS, strings.Trim(redisHost, "[]")); err != nil {
		return err
	}
	if !c.Redis.Enabled() && c.Redis.TLS.Enabled() {
		return fmt.Errorf("未配置 CCS_REDIS_ADDR 时不能启用 CCS_REDIS_TLS_MODE")
	}
	if c.Env == "production" && c.Redis.Enabled() && !localDependencyHost(redisHost) && !c.Redis.TLS.Enabled() {
		return fmt.Errorf("CCS_ENV=production 连接非 loopback Redis 时 CCS_REDIS_TLS_MODE 必须为 verify-full")
	}
	if c.Web.LoginRateLimit <= 0 || c.Web.LoginRateLimit > 1000 {
		return fmt.Errorf("CCS_LOGIN_RATE_LIMIT 必须在 1 到 1000 之间")
	}
	if c.Web.LoginRateWindow < time.Second || c.Web.LoginRateWindow > 24*time.Hour {
		return fmt.Errorf("CCS_LOGIN_RATE_WINDOW_SEC 必须在 1 到 86400 之间")
	}
	if c.Web.BackupStatusFile != "" && (!filepath.IsAbs(c.Web.BackupStatusFile) || filepath.Clean(c.Web.BackupStatusFile) != c.Web.BackupStatusFile || c.Web.BackupStatusFile == "/") {
		return fmt.Errorf("CCS_BACKUP_STATUS_FILE 必须是规范化的绝对文件路径")
	}
	if c.Web.RestoreStatusFile != "" && (!filepath.IsAbs(c.Web.RestoreStatusFile) || filepath.Clean(c.Web.RestoreStatusFile) != c.Web.RestoreStatusFile || c.Web.RestoreStatusFile == "/") {
		return fmt.Errorf("CCS_RESTORE_STATUS_FILE 必须是规范化的绝对文件路径")
	}
	if c.Web.IntegrityStatusFile != "" && (!filepath.IsAbs(c.Web.IntegrityStatusFile) || filepath.Clean(c.Web.IntegrityStatusFile) != c.Web.IntegrityStatusFile || c.Web.IntegrityStatusFile == "/") {
		return fmt.Errorf("CCS_INTEGRITY_STATUS_FILE 必须是规范化的绝对文件路径")
	}
	if c.Web.IntegrityMaxAge < time.Minute || c.Web.IntegrityMaxAge > 365*24*time.Hour {
		return fmt.Errorf("CCS_INTEGRITY_MAX_AGE_SEC 必须在 60 到 31536000 之间")
	}
	if c.Embedding.CacheEntries < 0 || c.Embedding.CacheEntries > 1_000_000 ||
		(c.Embedding.CacheEntries > 0 && (c.Embedding.CacheTTL <= 0 || c.Embedding.CacheTTL > 365*24*time.Hour)) {
		return fmt.Errorf("CCS_EMBEDDING_CACHE_ENTRIES 必须在 0 到 1000000 之间，启用时 CACHE_TTL_SEC 必须在 1 到 31536000 之间")
	}
	if c.Password.MemoryKiB < 8*1024 || c.Password.MemoryKiB > 1024*1024 || c.Password.Iterations < 1 || c.Password.Iterations > 20 || c.Password.Parallelism < 1 || c.Password.Parallelism > 255 {
		return fmt.Errorf("Argon2 参数超出安全范围：MEMORY_KIB 8192..1048576，ITERATIONS 1..20，PARALLELISM 1..255")
	}
	if c.Embedding.Timeout <= 0 || c.Embedding.Timeout > 10*time.Minute {
		return fmt.Errorf("CCS_EMBEDDING_TIMEOUT_SEC 必须在 1 到 600 之间")
	}
	if c.Embedding.IndexInterval <= 0 || c.Embedding.IndexInterval > time.Hour {
		return fmt.Errorf("CCS_EMBEDDING_INDEX_INTERVAL_SEC 必须在 1 到 3600 之间")
	}
	if c.Embedding.IndexBatch <= 0 || c.Embedding.IndexBatch > 1000 {
		return fmt.Errorf("CCS_EMBEDDING_INDEX_BATCH 必须在 1 到 1000 之间")
	}
	if c.External.Atlas.Enabled() {
		a := c.External.Atlas
		parsed, err := validateDependencyURL("CCS_ATLAS_MCP_URL", a.Endpoint)
		if err != nil {
			return err
		}
		if a.System == "" || a.SpaceID == "" || a.Name == "" {
			return fmt.Errorf("启用 Atlas 外部源时必须设置 CCS_ATLAS_SYSTEM、CCS_ATLAS_SPACE_ID 与 CCS_ATLAS_SOURCE_NAME")
		}
		if !isULIDText(a.SpaceID) {
			return fmt.Errorf("CCS_ATLAS_SPACE_ID 必须是 26 字符 ULID")
		}
		if len([]rune(a.Name)) > 120 || len([]rune(a.System)) > 120 {
			return fmt.Errorf("CCS_ATLAS_SOURCE_NAME 与 CCS_ATLAS_SYSTEM 不得超过 120 字符")
		}
		if a.SyncInterval < time.Minute || a.SyncInterval > 7*24*time.Hour {
			return fmt.Errorf("CCS_ATLAS_SYNC_INTERVAL_SEC 必须在 60 到 604800 之间")
		}
		if a.SyncTimeout < time.Minute || a.SyncTimeout > 24*time.Hour {
			return fmt.Errorf("CCS_ATLAS_SYNC_TIMEOUT_SEC 必须在 60 到 86400 之间")
		}
		if a.Timeout < time.Second || a.Timeout > 10*time.Minute {
			return fmt.Errorf("CCS_ATLAS_TIMEOUT_SEC 必须在 1 到 600 之间")
		}
		if a.PageSize < 1 || a.PageSize > 500 {
			return fmt.Errorf("CCS_ATLAS_PAGE_SIZE 必须在 1 到 500 之间")
		}
		if a.MaxWireBytes < 64<<10 || a.MaxWireBytes > 64<<20 || a.MaxBodyBytes < 1024 || int64(a.MaxBodyBytes) > a.MaxWireBytes {
			return fmt.Errorf("Atlas 响应边界要求 64KiB <= MAX_WIRE_BYTES <= 64MiB 且 1KiB <= MAX_BODY_BYTES <= MAX_WIRE_BYTES")
		}
		if !c.Embedding.Enabled() {
			return fmt.Errorf("Atlas 外部索引同步需要启用 CCS_EMBEDDING_PROVIDER")
		}
		if c.Env == "production" && parsed.Scheme != "https" && !localDependencyHost(parsed.Hostname()) {
			return fmt.Errorf("CCS_ENV=production 连接非 loopback Atlas 时 CCS_ATLAS_MCP_URL 必须使用 https")
		}
	}
	return nil
}

func validateClientTLS(prefix string, cfg *ClientTLS, defaultServerName string) error {
	if cfg.Mode == "" {
		cfg.Mode = TLSDisabled
	}
	switch cfg.Mode {
	case TLSDisabled:
		if cfg.CAFile != "" || cfg.CertFile != "" || cfg.KeyFile != "" || cfg.ServerName != "" {
			return fmt.Errorf("%s_MODE=disabled 时不能配置 CA、客户端证书、私钥或 SERVER_NAME", prefix)
		}
		return nil
	case TLSVerifyFull:
	default:
		return fmt.Errorf("%s_MODE 只支持 disabled 或 verify-full", prefix)
	}
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return fmt.Errorf("%s_CERT_FILE 与 %s_KEY_FILE 必须同时配置", prefix, prefix)
	}
	for name, path := range map[string]string{
		prefix + "_CA_FILE":   cfg.CAFile,
		prefix + "_CERT_FILE": cfg.CertFile,
		prefix + "_KEY_FILE":  cfg.KeyFile,
	} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/") {
			return fmt.Errorf("%s 必须是规范化的绝对文件路径", name)
		}
	}
	if cfg.ServerName == "" {
		cfg.ServerName = strings.Trim(defaultServerName, "[]")
	}
	if !validTLSServerName(cfg.ServerName) {
		return fmt.Errorf("%s_SERVER_NAME 必须是可验证的主机名或 IP，且不超过 253 字节", prefix)
	}
	return nil
}

func validTLSServerName(value string) bool {
	if value == "" || len(value) > 253 || strings.TrimSpace(value) != value {
		return false
	}
	if net.ParseIP(value) != nil {
		return true
	}
	value = strings.TrimSuffix(value, ".")
	if value == "" {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			if !asciiAlphaNumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func validateDependencyURL(name, raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("%s 必须是完整的 http(s) URL", name)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%s 不能在 URL 中携带用户名或密码", name)
	}
	return parsed, nil
}

func localDependencyHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validInstanceID(value string) bool {
	if len(value) == 0 || len(value) > 64 || !asciiAlphaNumeric(value[0]) {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !asciiAlphaNumeric(value[i]) && value[i] != '.' && value[i] != '_' && value[i] != '-' {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func isULIDText(value string) bool {
	if len(value) != 26 || value[0] < '0' || value[0] > '7' {
		return false
	}
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for i := range len(value) {
		if !strings.ContainsRune(alphabet, rune(value[i])) {
			return false
		}
	}
	return true
}

// loadDotEnv 载入 .env，已存在的环境变量优先，不被覆盖。
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 没有 .env 是正常的（生产环境用真实环境变量）
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	seen := make(map[string]struct{})
	lineNumber := 0
	for sc.Scan() {
		lineNumber++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("第 %d 行缺少 =", lineNumber)
		}
		key = strings.TrimSpace(key)
		if !validEnvKey(key) {
			return fmt.Errorf("第 %d 行的环境变量名 %q 非法", lineNumber, key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("第 %d 行重复定义 %s", lineNumber, key)
		}
		seen[key] = struct{}{}
		val, err = parseDotEnvValue(strings.TrimSpace(val))
		if err != nil {
			return fmt.Errorf("第 %d 行 %s 的值非法: %w", lineNumber, key, err)
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, val); err != nil {
				return fmt.Errorf("设置第 %d 行 %s 失败: %w", lineNumber, key, err)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return nil
}

func validEnvKey(key string) bool {
	if key == "" || !((key[0] >= 'A' && key[0] <= 'Z') || (key[0] >= 'a' && key[0] <= 'z') || key[0] == '_') {
		return false
	}
	for i := 1; i < len(key); i++ {
		b := key[i]
		if !((b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_') {
			return false
		}
	}
	return true
}

func parseDotEnvValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value[0] != '\'' && value[0] != '"' {
		return value, nil
	}
	if len(value) < 2 || value[len(value)-1] != value[0] {
		return "", fmt.Errorf("引号未闭合")
	}
	return value[1 : len(value)-1], nil
}

type envReader struct {
	errors []string
	read   map[string]struct{}
}

func (r *envReader) str(key, def string) string {
	r.mark(key)
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func (r *envReader) boolean(key string, def bool) bool {
	r.mark(key)
	if v, ok := os.LookupEnv(key); ok && v != "" {
		switch strings.ToLower(v) {
		case "true":
			return true
		case "false":
			return false
		default:
			r.errors = append(r.errors, fmt.Sprintf("%s=%q 必须是 true 或 false", key, v))
			return def
		}
	}
	return def
}

func (r *envReader) integer(key string, def int) int {
	r.mark(key)
	if v, ok := os.LookupEnv(key); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			r.errors = append(r.errors, fmt.Sprintf("%s=%q 必须是整数", key, v))
			return def
		}
		return n
	}
	return def
}

func (r *envReader) duration(key string, def int, unit time.Duration) time.Duration {
	n := r.integer(key, def)
	maxUnits := int64((1<<63 - 1) / unit)
	minUnits := -maxUnits - 1
	if int64(n) > maxUnits || int64(n) < minUnits {
		r.errors = append(r.errors, fmt.Sprintf("%s=%q 超出可表示范围", key, strconv.Itoa(n)))
		return time.Duration(def) * unit
	}
	return time.Duration(n) * unit
}

func (r *envReader) number(key string, def float64) float64 {
	r.mark(key)
	if v, ok := os.LookupEnv(key); ok && v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			r.errors = append(r.errors, fmt.Sprintf("%s=%q 必须是数字", key, v))
			return def
		}
		return n
	}
	return def
}

func (r *envReader) err() error {
	var unknown []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !strings.HasPrefix(key, "CCS_") {
			continue
		}
		if _, known := r.read[key]; !known {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		r.errors = append(r.errors, fmt.Sprintf("%s 未被当前版本识别", key))
	}
	if len(r.errors) == 0 {
		return nil
	}
	return fmt.Errorf("环境变量配置非法: %s", strings.Join(r.errors, "; "))
}

func (r *envReader) mark(key string) {
	if r.read == nil {
		r.read = make(map[string]struct{})
	}
	r.read[key] = struct{}{}
}
