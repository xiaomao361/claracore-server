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
}

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
		Embedding: Embedding{
			Provider: getStr("CCS_EMBEDDING_PROVIDER", "disabled"),
			BaseURL:  getStr("CCS_EMBEDDING_BASE_URL", ""),
			Model:    getStr("CCS_EMBEDDING_MODEL", ""),
			APIKey:   getStr("CCS_EMBEDDING_API_KEY", ""),
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

	switch c.Embedding.Provider {
	case "ollama", "openai-compatible":
		if c.Embedding.BaseURL == "" {
			return fmt.Errorf("CCS_EMBEDDING_PROVIDER=%s 时必须设置 CCS_EMBEDDING_BASE_URL", c.Embedding.Provider)
		}
	case "disabled":
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

func getInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
