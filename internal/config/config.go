// Package config 是整个进程读取环境变量的唯一位置。
//
// 其他任何包禁止调用 os.Getenv —— 由 test/arch_test.go 保障。
// 配置在启动时一次性加载并校验，校验失败直接 fatal，
// 绝不允许运行期才发现配置缺失（docs/05-code-structure.md §6.3）。
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env       string
	Addr      string
	MySQL     MySQL
	Redis     Redis
	Embedding Embedding
	Password  Password
	Web       Web
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
		"%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&collation=utf8mb4_0900_ai_ci&parseTime=true&loc=UTC&time_zone=%%27%%2B00%%3A00%%27",
		m.User, m.Password, m.Host, m.Port, m.Database,
	)
}

type Redis struct {
	Addr     string
	Password string
	Prefix   string
}

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
}

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
	loadDotEnv(".env")

	c := &Config{
		Env:  getStr("CCS_ENV", "development"),
		Addr: getStr("CCS_ADDR", "127.0.0.1:51890"),
		MySQL: MySQL{
			Host:            getStr("CCS_MYSQL_HOST", ""),
			Port:            getInt("CCS_MYSQL_PORT", 3306),
			User:            getStr("CCS_MYSQL_USER", ""),
			Password:        getStr("CCS_MYSQL_PASSWORD", ""),
			Database:        getStr("CCS_MYSQL_DATABASE", ""),
			MaxOpenConns:    getInt("CCS_MYSQL_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    getInt("CCS_MYSQL_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: time.Duration(getInt("CCS_MYSQL_CONN_MAX_LIFETIME_SEC", 1800)) * time.Second,
		},
		Redis: Redis{
			Addr:     getStr("CCS_REDIS_ADDR", ""),
			Password: getStr("CCS_REDIS_PASSWORD", ""),
			Prefix:   getStr("CCS_REDIS_PREFIX", "ccs:"),
		},
		Password: Password{
			MemoryKiB:   getInt("CCS_ARGON2_MEMORY_KIB", 19*1024),
			Iterations:  getInt("CCS_ARGON2_ITERATIONS", 2),
			Parallelism: getInt("CCS_ARGON2_PARALLELISM", 1),
		},
		Web: Web{
			PublicMCPURL: getStr("CCS_PUBLIC_MCP_URL", ""),
			SecureCookie: getBool("CCS_WEB_SECURE_COOKIE", getStr("CCS_ENV", "development") == "production"),
		},
		Embedding: Embedding{
			Provider:      getStr("CCS_EMBEDDING_PROVIDER", ProviderDisabled),
			BaseURL:       getStr("CCS_EMBEDDING_BASE_URL", ""),
			Model:         getStr("CCS_EMBEDDING_MODEL", ""),
			APIKey:        getStr("CCS_EMBEDDING_API_KEY", ""),
			Timeout:       time.Duration(getInt("CCS_EMBEDDING_TIMEOUT_SEC", 30)) * time.Second,
			IndexInterval: time.Duration(getInt("CCS_EMBEDDING_INDEX_INTERVAL_SEC", 5)) * time.Second,
			IndexBatch:    getInt("CCS_EMBEDDING_INDEX_BATCH", 32),
		},
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

	// 没配对外地址时退回监听地址：本地开发正好能用，
	// 而生产上 nginx 后面的部署必须显式配置（否则发出去的配置片段连不上）。
	if c.Web.PublicMCPURL == "" {
		c.Web.PublicMCPURL = "http://" + c.Addr + "/mcp"
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
	case ProviderDisabled:
	default:
		return fmt.Errorf("CCS_EMBEDDING_PROVIDER 取值非法: %q（可选 ollama | openai-compatible | disabled）", c.Embedding.Provider)
	}
	return nil
}

// loadDotEnv 载入 .env，已存在的环境变量优先，不被覆盖。
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // 没有 .env 是正常的（生产环境用真实环境变量）
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func getStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func getInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
