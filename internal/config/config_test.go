package config

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func validTestConfig() *Config {
	return &Config{
		Env: "production", Addr: "127.0.0.1:51890", ShutdownDrain: 5 * time.Second,
		HTTP: HTTP{
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
			WriteTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute,
			MaxHeaderBytes: 64 << 10, WebMaxFormBytes: 1 << 20, MCPMaxBodyBytes: 4 << 20,
			MCPRateLimit: 300, MCPTenantRateLimit: 3000, MCPRateWindow: time.Minute,
		},
		MySQL: MySQL{
			Host: "127.0.0.1", Port: 3306, User: "ccs", Database: "ccs",
			MaxOpenConns: 25, MaxIdleConns: 5, ConnMaxLifetime: 30 * time.Minute,
		},
		Redis:       Redis{Timeout: 500 * time.Millisecond},
		Embedding:   Embedding{Provider: ProviderDisabled, Timeout: 30 * time.Second, IndexInterval: 5 * time.Second, IndexBatch: 32},
		AutoContext: AutoContext{MinScore: 0.55, MinMargin: 0.03},
		Password:    Password{MemoryKiB: 19 * 1024, Iterations: 2, Parallelism: 1},
		Web: Web{
			PublicMCPURL: "https://ccs.example/mcp", SecureCookie: true,
			LoginRateLimit: 10, LoginRateWindow: 15 * time.Minute,
			ExportMaxBytes: 1 << 30, IntegrityMaxAge: 25 * time.Hour,
		},
	}
}

func prepareLoadTest(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, key := range []string{
		"CCS_ENV", "CCS_INSTANCE_ID", "CCS_ADDR", "CCS_SHUTDOWN_DRAIN_SEC", "CCS_MYSQL_HOST", "CCS_MYSQL_PORT", "CCS_MYSQL_USER",
		"CCS_HTTP_READ_HEADER_TIMEOUT_SEC", "CCS_HTTP_READ_TIMEOUT_SEC", "CCS_HTTP_WRITE_TIMEOUT_SEC",
		"CCS_HTTP_IDLE_TIMEOUT_SEC", "CCS_HTTP_MAX_HEADER_BYTES", "CCS_WEB_MAX_FORM_BYTES", "CCS_MCP_MAX_BODY_BYTES",
		"CCS_MCP_RATE_LIMIT", "CCS_MCP_TENANT_RATE_LIMIT", "CCS_MCP_RATE_WINDOW_SEC",
		"CCS_METRICS_ADDR",
		"CCS_MYSQL_PASSWORD", "CCS_MYSQL_DATABASE", "CCS_MYSQL_MAX_OPEN_CONNS",
		"CCS_MYSQL_MAX_IDLE_CONNS", "CCS_MYSQL_CONN_MAX_LIFETIME_SEC", "CCS_MYSQL_TLS_MODE",
		"CCS_MYSQL_TLS_CA_FILE", "CCS_MYSQL_TLS_CERT_FILE", "CCS_MYSQL_TLS_KEY_FILE", "CCS_MYSQL_TLS_SERVER_NAME",
		"CCS_REDIS_ADDR", "CCS_REDIS_PASSWORD", "CCS_REDIS_PREFIX", "CCS_REDIS_TIMEOUT_MS", "CCS_REDIS_TLS_MODE",
		"CCS_REDIS_TLS_CA_FILE", "CCS_REDIS_TLS_CERT_FILE", "CCS_REDIS_TLS_KEY_FILE", "CCS_REDIS_TLS_SERVER_NAME", "CCS_ARGON2_MEMORY_KIB",
		"CCS_ARGON2_ITERATIONS", "CCS_ARGON2_PARALLELISM", "CCS_PUBLIC_MCP_URL",
		"CCS_WEB_SECURE_COOKIE", "CCS_INGEST_ROOT", "CCS_LOGIN_RATE_LIMIT",
		"CCS_LOGIN_RATE_WINDOW_SEC", "CCS_EXPORT_MAX_BYTES", "CCS_BACKUP_STATUS_FILE", "CCS_RESTORE_STATUS_FILE", "CCS_INTEGRITY_STATUS_FILE", "CCS_INTEGRITY_MAX_AGE_SEC", "CCS_EMBEDDING_PROVIDER", "CCS_EMBEDDING_BASE_URL",
		"CCS_EMBEDDING_MODEL", "CCS_EMBEDDING_API_KEY", "CCS_EMBEDDING_TIMEOUT_SEC",
		"CCS_EMBEDDING_INDEX_INTERVAL_SEC", "CCS_EMBEDDING_INDEX_BATCH",
		"CCS_EMBEDDING_CACHE_ENTRIES", "CCS_EMBEDDING_CACHE_TTL_SEC",
		"CCS_AUTO_CONTEXT_MIN_SCORE", "CCS_AUTO_CONTEXT_MIN_MARGIN",
		"CCS_ATLAS_MCP_URL", "CCS_ATLAS_MCP_TOKEN", "CCS_ATLAS_SYSTEM", "CCS_ATLAS_SOURCE_NAME",
		"CCS_ATLAS_SPACE_ID", "CCS_ATLAS_SYNC_INTERVAL_SEC", "CCS_ATLAS_SYNC_TIMEOUT_SEC", "CCS_ATLAS_TIMEOUT_SEC",
		"CCS_ATLAS_PAGE_SIZE", "CCS_ATLAS_MAX_WIRE_BYTES", "CCS_ATLAS_MAX_BODY_BYTES",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("CCS_MYSQL_HOST", "127.0.0.1")
	t.Setenv("CCS_MYSQL_USER", "ccs")
	t.Setenv("CCS_MYSQL_DATABASE", "ccs")
}

func TestConfigInstanceIDIsOptionalAndPublicSafe(t *testing.T) {
	for _, value := range []string{"ccs-a", "node_02", "az1.node3", "7"} {
		cfg := validTestConfig()
		cfg.InstanceID = value
		if err := cfg.validate(); err != nil {
			t.Fatalf("valid instance ID %q must pass: %v", value, err)
		}
	}
	for _, value := range []string{"-node", ".node", "node one", "节点一", strings.Repeat("a", 65)} {
		cfg := validTestConfig()
		cfg.InstanceID = value
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "CCS_INSTANCE_ID") {
			t.Fatalf("unsafe instance ID %q must fail: %v", value, err)
		}
	}

	prepareLoadTest(t)
	t.Setenv("CCS_INSTANCE_ID", "ccs-blue-1")
	cfg, err := Load()
	if err != nil || cfg.InstanceID != "ccs-blue-1" {
		t.Fatalf("instance ID must load through strict environment reader: id=%q err=%v", cfg.InstanceID, err)
	}
}

func TestConfigTenantExportSizeIsBounded(t *testing.T) {
	for _, value := range []int64{1<<20 - 1, 16<<30 + 1} {
		cfg := validTestConfig()
		cfg.Web.ExportMaxBytes = value
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "CCS_EXPORT_MAX_BYTES") {
			t.Fatalf("unsafe export size %d must fail: %v", value, err)
		}
	}
	prepareLoadTest(t)
	t.Setenv("CCS_EXPORT_MAX_BYTES", "8388608")
	cfg, err := Load()
	if err != nil || cfg.Web.ExportMaxBytes != 8<<20 {
		t.Fatalf("strict export size did not load: size=%d err=%v", cfg.Web.ExportMaxBytes, err)
	}
}

func TestConfigEmbeddingModelFitsPersistentQueueIdentity(t *testing.T) {
	for _, model := range []string{" bge-m3", "bge-m3 ", strings.Repeat("模", 65)} {
		cfg := validTestConfig()
		cfg.Embedding = Embedding{Provider: ProviderOllama, BaseURL: "http://127.0.0.1:11434", Model: model, Timeout: 30 * time.Second, IndexInterval: 5 * time.Second, IndexBatch: 32}
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "CCS_EMBEDDING_MODEL") {
			t.Fatalf("unsafe embedding model %q must fail before a database write: %v", model, err)
		}
	}
	cfg := validTestConfig()
	cfg.Embedding = Embedding{Provider: ProviderOllama, BaseURL: "http://127.0.0.1:11434", Model: strings.Repeat("模", 64), Timeout: 30 * time.Second, IndexInterval: 5 * time.Second, IndexBatch: 32}
	if err := cfg.validate(); err != nil {
		t.Fatalf("64-character embedding model must fit embeddings.model: %v", err)
	}
}

func TestConfigMetricsListenerRequiresDedicatedNumericLoopback(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		want string
	}{
		{name: "public ipv4", addr: "0.0.0.0:51892", want: "loopback"},
		{name: "public ipv6", addr: "[::]:51892", want: "loopback"},
		{name: "hostname", addr: "localhost:51892", want: "loopback"},
		{name: "same port", addr: "127.0.0.1:51890", want: "不同的端口"},
		{name: "missing port", addr: "127.0.0.1", want: "host:port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validTestConfig()
			cfg.Metrics.Addr = tc.addr
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("metrics address %q must fail with %q: %v", tc.addr, tc.want, err)
			}
		})
	}
	for _, addr := range []string{"127.0.0.1:51892", "[::1]:51892"} {
		cfg := validTestConfig()
		cfg.Metrics.Addr = addr
		if err := cfg.validate(); err != nil {
			t.Fatalf("loopback metrics address %q must pass: %v", addr, err)
		}
	}
}

func TestLoadRejectsMalformedTypedEnvironment(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{key: "CCS_MYSQL_PORT", value: "three-thousand"},
		{key: "CCS_WEB_SECURE_COOKIE", value: "enabled"},
		{key: "CCS_EMBEDDING_INDEX_BATCH", value: "3.5"},
		{key: "CCS_EMBEDDING_CACHE_TTL_SEC", value: "9223372036854775807"},
		{key: "CCS_AUTO_CONTEXT_MIN_SCORE", value: "not-a-score"},
		{key: "CCS_SHUTDOWN_DRAIN_SEC", value: "later"},
		{key: "CCS_HTTP_MAX_HEADER_BYTES", value: "many"},
		{key: "CCS_MCP_TENANT_RATE_LIMIT", value: "all-of-them"},
		{key: "CCS_INTEGRITY_MAX_AGE_SEC", value: "tomorrow"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			prepareLoadTest(t)
			t.Setenv(tc.key, tc.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), tc.value) {
				t.Fatalf("malformed %s must fail with its source value: %v", tc.key, err)
			}
		})
	}
}

func TestLoadRejectsUnknownCCSVariableNames(t *testing.T) {
	prepareLoadTest(t)
	t.Setenv("CCS_MYSLQ_PORT", "3306")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "CCS_MYSLQ_PORT") || !strings.Contains(err.Error(), "未被当前版本识别") {
		t.Fatalf("unknown CCS key must fail instead of hiding a typo: %v", err)
	}
}

func TestLoadTreatsEmptyTypedValuesAsDocumentedDefaults(t *testing.T) {
	prepareLoadTest(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("empty optional values should use defaults: %v", err)
	}
	if cfg.MySQL.Port != 3306 || cfg.Web.SecureCookie || cfg.Web.LoginRateLimit != 10 || cfg.AutoContext.MinScore != 0.55 || cfg.ShutdownDrain != 5*time.Second ||
		cfg.HTTP.ReadHeaderTimeout != 10*time.Second || cfg.HTTP.ReadTimeout != 30*time.Second || cfg.HTTP.WriteTimeout != 5*time.Minute ||
		cfg.HTTP.IdleTimeout != 2*time.Minute || cfg.HTTP.MaxHeaderBytes != 64<<10 || cfg.HTTP.WebMaxFormBytes != 1<<20 || cfg.HTTP.MCPMaxBodyBytes != 4<<20 ||
		cfg.HTTP.MCPRateLimit != 300 || cfg.HTTP.MCPTenantRateLimit != 3000 || cfg.HTTP.MCPRateWindow != time.Minute {
		t.Fatalf("unexpected defaults: mysql=%d secure=%v limit=%d score=%v drain=%v http=%+v", cfg.MySQL.Port, cfg.Web.SecureCookie, cfg.Web.LoginRateLimit, cfg.AutoContext.MinScore, cfg.ShutdownDrain, cfg.HTTP)
	}
}

func TestLoadRejectsMalformedDotEnvInsteadOfIgnoringIt(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{name: "missing equals", body: "CCS_MYSQL_PORT 3306\n", want: "缺少 ="},
		{name: "invalid key", body: "CCS-MYSQL-PORT=3306\n", want: "变量名"},
		{name: "unterminated quote", body: "CCS_MYSQL_PORT='3306\n", want: "引号未闭合"},
		{name: "duplicate key", body: "CCS_MYSQL_PORT=3306\nCCS_MYSQL_PORT=3307\n", want: "重复定义"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepareLoadTest(t)
			if err := os.WriteFile(".env", []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("malformed .env must fail with %q: %v", tc.want, err)
			}
		})
	}
}

func TestProductionRequiresSecureCookieAndHTTPSPublicURL(t *testing.T) {
	cfg := validTestConfig()
	cfg.Web.SecureCookie = false
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "SECURE_COOKIE") {
		t.Fatalf("production must reject an insecure session cookie: %v", err)
	}

	cfg = validTestConfig()
	cfg.Web.PublicMCPURL = "http://ccs.example/mcp"
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("production must reject an HTTP public MCP URL: %v", err)
	}

	cfg = validTestConfig()
	cfg.Web.PublicMCPURL = "https://user:secret@ccs.example/mcp"
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "用户名或密码") {
		t.Fatalf("public URL credentials must be rejected: %v", err)
	}

	cfg = validTestConfig()
	cfg.Env = "development"
	cfg.Web.SecureCookie = false
	cfg.Web.PublicMCPURL = "http://127.0.0.1:51890/mcp"
	if err := cfg.validate(); err != nil {
		t.Fatalf("local HTTP development must remain supported: %v", err)
	}
}

func TestProductionRemoteDependenciesRequireVerifiedTransport(t *testing.T) {
	cfg := validTestConfig()
	cfg.MySQL.Host = "mysql.internal"
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "MYSQL_TLS_MODE") {
		t.Fatalf("remote production MySQL must require verified TLS: %v", err)
	}
	cfg.MySQL.TLS = ClientTLS{Mode: TLSVerifyFull}
	if err := cfg.validate(); err != nil || cfg.MySQL.TLS.ServerName != "mysql.internal" {
		t.Fatalf("verified MySQL TLS must pass and derive server name: tls=%+v err=%v", cfg.MySQL.TLS, err)
	}

	cfg = validTestConfig()
	cfg.Redis.Addr = "redis.internal:6379"
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "REDIS_TLS_MODE") {
		t.Fatalf("remote production Redis must require verified TLS: %v", err)
	}
	cfg.Redis.TLS = ClientTLS{Mode: TLSVerifyFull, ServerName: "cache.internal"}
	if err := cfg.validate(); err != nil {
		t.Fatalf("verified Redis TLS must pass: %v", err)
	}

	cfg = validTestConfig()
	cfg.Embedding = Embedding{Provider: ProviderOllama, BaseURL: "http://embedding.internal", Model: "bge-m3", Timeout: 30 * time.Second, IndexInterval: 5 * time.Second, IndexBatch: 32}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("remote production embedding must require HTTPS: %v", err)
	}
	cfg.Embedding.BaseURL = "https://embedding.internal"
	if err := cfg.validate(); err != nil {
		t.Fatalf("remote HTTPS embedding must pass: %v", err)
	}
}

func TestClientTLSRejectsDowngradeAndUnsafeFileShape(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		tls        ClientTLS
	}{
		{name: "unknown mode", want: "disabled 或 verify-full", tls: ClientTLS{Mode: "preferred"}},
		{name: "CA while disabled", want: "MODE=disabled", tls: ClientTLS{Mode: TLSDisabled, CAFile: "/etc/ca.pem"}},
		{name: "relative CA", want: "绝对", tls: ClientTLS{Mode: TLSVerifyFull, CAFile: "ca.pem"}},
		{name: "client pair incomplete", want: "同时配置", tls: ClientTLS{Mode: TLSVerifyFull, CertFile: "/etc/client.pem"}},
		{name: "unsafe server name", want: "SERVER_NAME", tls: ClientTLS{Mode: TLSVerifyFull, ServerName: "mysql.internal\nforged"}},
		{name: "server name with space", want: "SERVER_NAME", tls: ClientTLS{Mode: TLSVerifyFull, ServerName: "mysql internal"}},
		{name: "wildcard server name", want: "SERVER_NAME", tls: ClientTLS{Mode: TLSVerifyFull, ServerName: "*.internal"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validTestConfig()
			cfg.MySQL.TLS = tc.tls
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe TLS config must fail with %q: %v", tc.want, err)
			}
		})
	}
}

func TestProductionAllowsPlaintextOnlyForLoopbackDependencies(t *testing.T) {
	cfg := validTestConfig()
	cfg.Redis.Addr = "[::1]:6379"
	cfg.Embedding = Embedding{Provider: ProviderOllama, BaseURL: "http://localhost:11434", Model: "bge-m3", Timeout: 30 * time.Second, IndexInterval: 5 * time.Second, IndexBatch: 32}
	if err := cfg.validate(); err != nil {
		t.Fatalf("same-host production dependencies may use plaintext: %v", err)
	}
}

func TestLoadReadsVerifiedTLSWithoutTouchingCertificateFiles(t *testing.T) {
	prepareLoadTest(t)
	t.Setenv("CCS_ENV", "production")
	t.Setenv("CCS_PUBLIC_MCP_URL", "https://ccs.example/mcp")
	t.Setenv("CCS_WEB_SECURE_COOKIE", "true")
	t.Setenv("CCS_MYSQL_HOST", "mysql.internal")
	t.Setenv("CCS_MYSQL_TLS_MODE", "verify-full")
	t.Setenv("CCS_MYSQL_TLS_CA_FILE", "/etc/claracore-server/mysql-ca.pem")
	t.Setenv("CCS_MYSQL_TLS_SERVER_NAME", "mysql.service.internal")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MySQL.TLS.Enabled() || cfg.MySQL.TLS.CAFile != "/etc/claracore-server/mysql-ca.pem" || cfg.MySQL.TLS.ServerName != "mysql.service.internal" {
		t.Fatalf("TLS environment did not load exactly: %+v", cfg.MySQL.TLS)
	}
}

func TestConfigRejectsUnsafeNumericRanges(t *testing.T) {
	tests := []struct {
		name, want string
		mutate     func(*Config)
	}{
		{name: "mysql port", want: "MYSQL_PORT", mutate: func(c *Config) { c.MySQL.Port = 70000 }},
		{name: "mysql pool", want: "连接池", mutate: func(c *Config) { c.MySQL.MaxIdleConns = c.MySQL.MaxOpenConns + 1 }},
		{name: "mysql pool upper bound", want: "MAX_OPEN_CONNS", mutate: func(c *Config) { c.MySQL.MaxOpenConns = 10_001 }},
		{name: "argon memory", want: "Argon2", mutate: func(c *Config) { c.Password.MemoryKiB = 1 }},
		{name: "argon parallelism", want: "Argon2", mutate: func(c *Config) { c.Password.Parallelism = 256 }},
		{name: "embedding timeout", want: "EMBEDDING_TIMEOUT", mutate: func(c *Config) { c.Embedding.Timeout = 11 * time.Minute }},
		{name: "embedding interval", want: "INDEX_INTERVAL", mutate: func(c *Config) { c.Embedding.IndexInterval = 0 }},
		{name: "embedding batch", want: "INDEX_BATCH", mutate: func(c *Config) { c.Embedding.IndexBatch = 0 }},
		{name: "nan score", want: "AUTO_CONTEXT", mutate: func(c *Config) { c.AutoContext.MinScore = math.NaN() }},
		{name: "shutdown drain", want: "SHUTDOWN_DRAIN", mutate: func(c *Config) { c.ShutdownDrain = 61 * time.Second }},
		{name: "relative backup receipt", want: "BACKUP_STATUS_FILE", mutate: func(c *Config) { c.Web.BackupStatusFile = "backup-status.json" }},
		{name: "unclean backup receipt", want: "BACKUP_STATUS_FILE", mutate: func(c *Config) { c.Web.BackupStatusFile = "/var/lib/../tmp/status.json" }},
		{name: "relative restore receipt", want: "RESTORE_STATUS_FILE", mutate: func(c *Config) { c.Web.RestoreStatusFile = "restore-status.json" }},
		{name: "unclean restore receipt", want: "RESTORE_STATUS_FILE", mutate: func(c *Config) { c.Web.RestoreStatusFile = "/var/lib/../tmp/restore.json" }},
		{name: "relative integrity receipt", want: "INTEGRITY_STATUS_FILE", mutate: func(c *Config) { c.Web.IntegrityStatusFile = "integrity-status.json" }},
		{name: "unclean integrity receipt", want: "INTEGRITY_STATUS_FILE", mutate: func(c *Config) { c.Web.IntegrityStatusFile = "/var/lib/../tmp/integrity.json" }},
		{name: "integrity freshness", want: "INTEGRITY_MAX_AGE", mutate: func(c *Config) { c.Web.IntegrityMaxAge = 30 * time.Second }},
		{name: "http read order", want: "读取预算", mutate: func(c *Config) { c.HTTP.ReadTimeout = 5 * time.Second }},
		{name: "http write order", want: "WRITE_TIMEOUT", mutate: func(c *Config) { c.HTTP.WriteTimeout = 5 * time.Second }},
		{name: "http idle", want: "IDLE_TIMEOUT", mutate: func(c *Config) { c.HTTP.IdleTimeout = 0 }},
		{name: "http headers", want: "MAX_HEADER_BYTES", mutate: func(c *Config) { c.HTTP.MaxHeaderBytes = 1024 }},
		{name: "web form body", want: "WEB_MAX_FORM_BYTES", mutate: func(c *Config) { c.HTTP.WebMaxFormBytes = 32 << 10 }},
		{name: "mcp body", want: "MCP_MAX_BODY_BYTES", mutate: func(c *Config) { c.HTTP.MCPMaxBodyBytes = 32 << 10 }},
		{name: "mcp rate", want: "MCP_RATE_LIMIT", mutate: func(c *Config) { c.HTTP.MCPRateLimit = 100_001 }},
		{name: "tenant mcp rate below agent", want: "MCP_TENANT_RATE_LIMIT", mutate: func(c *Config) { c.HTTP.MCPTenantRateLimit = c.HTTP.MCPRateLimit - 1 }},
		{name: "tenant mcp rate upper bound", want: "MCP_TENANT_RATE_LIMIT", mutate: func(c *Config) { c.HTTP.MCPTenantRateLimit = 1_000_001 }},
		{name: "mcp rate window", want: "MCP_RATE_WINDOW", mutate: func(c *Config) { c.HTTP.MCPRateWindow = 25 * time.Hour }},
		{name: "embedding exceeds write budget", want: "EMBEDDING_TIMEOUT", mutate: func(c *Config) {
			c.Embedding = Embedding{Provider: ProviderOllama, BaseURL: "http://127.0.0.1:11434", Model: "bge-m3", Timeout: 6 * time.Minute, IndexInterval: 5 * time.Second, IndexBatch: 32}
			c.HTTP.WriteTimeout = 5 * time.Minute
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validTestConfig()
			tc.mutate(cfg)
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe value must fail with %q: %v", tc.want, err)
			}
		})
	}
}

func TestConfigRejectsDisabledOrUnboundedLoginLimiter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int
		window time.Duration
	}{
		{name: "zero limit", limit: 0, window: 15 * time.Minute},
		{name: "excessive limit", limit: 1001, window: 15 * time.Minute},
		{name: "short window", limit: 10, window: 0},
		{name: "long window", limit: 10, window: 25 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validTestConfig()
			cfg.Web.LoginRateLimit, cfg.Web.LoginRateWindow = tc.limit, tc.window
			if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "LOGIN_RATE") {
				t.Fatalf("invalid login limit must fail configuration: %v", err)
			}
		})
	}
}

func TestAtlasExternalSourceRequiresCompleteBoundedSemanticConfiguration(t *testing.T) {
	cfg := validTestConfig()
	cfg.External.Atlas = AtlasExternal{
		Endpoint: "https://atlas.example/mcp", Token: "secret", System: "shared-docs",
		Name: "atlas-docs", SpaceID: "01TESTSPACEAAAAAAAAAAAAAAA", SyncInterval: 15 * time.Minute,
		SyncTimeout: 10 * time.Minute, Timeout: 30 * time.Second, PageSize: 100, MaxWireBytes: 8 << 20, MaxBodyBytes: 2 << 20,
	}
	cfg.Embedding = Embedding{Provider: ProviderOllama, BaseURL: "http://127.0.0.1:11434", Model: "bge-m3", Timeout: 30 * time.Second, IndexInterval: 5 * time.Second, IndexBatch: 32}
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid Atlas source must pass: %v", err)
	}

	for _, tc := range []struct {
		name, want string
		mutate     func(*Config)
	}{
		{name: "missing system", want: "ATLAS_SYSTEM", mutate: func(c *Config) { c.External.Atlas.System = "" }},
		{name: "invalid space", want: "26", mutate: func(c *Config) { c.External.Atlas.SpaceID = "short" }},
		{name: "invalid space alphabet", want: "26", mutate: func(c *Config) { c.External.Atlas.SpaceID = "01TESTSPACEIIIIIIIIIIIIIII" }},
		{name: "disabled embedding", want: "EMBEDDING_PROVIDER", mutate: func(c *Config) { c.Embedding.Provider = ProviderDisabled }},
		{name: "unbounded page", want: "PAGE_SIZE", mutate: func(c *Config) { c.External.Atlas.PageSize = 501 }},
		{name: "body larger than wire", want: "MAX_BODY_BYTES", mutate: func(c *Config) { c.External.Atlas.MaxBodyBytes = 9 << 20 }},
		{name: "plaintext production credential", want: "https", mutate: func(c *Config) { c.External.Atlas.Endpoint = "http://atlas.internal/mcp" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clone := *cfg
			tc.mutate(&clone)
			if err := clone.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}
